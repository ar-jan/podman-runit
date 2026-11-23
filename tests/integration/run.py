#!/usr/bin/env python3
"""Isolated, opt-in stock Podman contract and scheduler smoke suite.
Never reads or modifies the user's container store or API service.
"""
import argparse, hashlib, json, os, pathlib, pwd, shutil, shlex, subprocess, tempfile, time

p = argparse.ArgumentParser()
p.add_argument('--podman', required=True, type=pathlib.Path)
p.add_argument('--keep', action='store_true')
a = p.parse_args()
root = pathlib.Path(__file__).resolve().parents[2]
work = pathlib.Path(tempfile.mkdtemp(prefix='pr-integration-'))
runtime = work/'runtime'; runtime.mkdir(mode=0o700)
conf = work/'containers.conf'
conf.write_text('[engine]\ncgroup_manager="cgroupfs"\nevents_logger="file"\n')
env = os.environ.copy()
for key in ('CONTAINER_HOST', 'CONTAINER_CONNECTION', 'DOCKER_HOST'): env.pop(key,None)
env.update(XDG_RUNTIME_DIR=str(runtime), CONTAINERS_CONF=str(conf), CONTAINERS_CONF_OVERRIDE=str(conf), CONTAINERS_STORAGE_CONF=str(work/'storage.conf'), PODMAN_COMPOSE_PROVIDER='/usr/bin/docker-compose')
(work/'storage.conf').write_text(f'[storage]\ndriver="vfs"\nrunroot="{work}/runroot"\ngraphroot="{work}/storage"\n')
base = [str(a.podman.resolve()), '--root', str(work/'storage'), '--runroot', str(work/'runroot'), '--storage-driver', 'vfs', '--cgroup-manager', 'cgroupfs', '--events-backend', 'file', '--network-config-dir', str(work/'networks')]
socket = runtime/'api.sock'
# This randomly named socket has its own scheduler state, shared across launches.
uid_runtime = pathlib.Path('/run/user')/str(os.getuid())
state_root = uid_runtime if socket.is_relative_to(uid_runtime) else pathlib.Path(pwd.getpwuid(os.getuid()).pw_dir)/'.local/state'
scheduler_dir = state_root/'podman-runit'/hashlib.sha256(os.fsencode(socket)).hexdigest()[:24]
processes=[]; ids=[]; evidence={}; logs=[]
class Managed:
    def __init__(self, command, log):
        self.dir=work/f'service-{len(processes)}'
        self.dir.mkdir()
        run=self.dir/'run';run.write_text('#!/bin/sh\nexec '+shlex.join(command)+'\n');run.chmod(0o700)
        self.runner=subprocess.Popen(['runsv',str(self.dir)],env=env,stdout=log,stderr=log)
        self.pid=wait(lambda: self.children(),5)[0]
    def children(self):
        return [int(p) for p in pathlib.Path(f'/proc/{self.runner.pid}/task/{self.runner.pid}/children').read_text().split()]
    def terminate(self):
        subprocess.run(['sv','-w','12','down',str(self.dir)],capture_output=True,timeout=15,check=True)
        self.runner.terminate()
    def wait(self,timeout): return self.runner.wait(timeout=timeout)
    def poll(self):return self.runner.poll()
    def kill(self):
        for pid in self.children():os.kill(pid,9)
        self.runner.kill()
def process_ids():
    found=set()
    def visit(pid):
        if pid in found:return
        found.add(pid)
        try:
            for thread in pathlib.Path(f'/proc/{pid}/task').iterdir():
                for child in (thread/'children').read_text().split():visit(int(child))
        except (FileNotFoundError, ProcessLookupError):pass
    for proc in processes:
        if proc.poll() is None:visit(proc.runner.pid)
    return found
def metrics():
    rows={}
    for pid in process_ids():
        try:
            status=pathlib.Path(f'/proc/{pid}/status').read_text()
            stat=pathlib.Path(f'/proc/{pid}/stat').read_text().rsplit(')',1)[1].split()
            smaps=pathlib.Path(f'/proc/{pid}/smaps_rollup').read_text()
            def field(text,name):return int(next(line.split()[1] for line in text.splitlines() if line.startswith(name+':')))
            rows[pid]={'name':status.splitlines()[0].split()[1], 'rss_kib':field(status,'VmRSS'),'pss_kib':field(smaps,'Pss'),'cpu_ticks':int(stat[11])+int(stat[12]),'fds':len(list(pathlib.Path(f'/proc/{pid}/fd').iterdir()))}
        except (FileNotFoundError, ProcessLookupError, StopIteration, PermissionError):pass
    return rows
def sample(name,seconds=3):
    before=metrics();start=time.monotonic();time.sleep(seconds);after=metrics()
    evidence.setdefault('measurements',{})[name]={'seconds':time.monotonic()-start,'processes':after,'cpu_seconds':sum(max(0,v['cpu_ticks']-before.get(pid,v)['cpu_ticks']) for pid,v in after.items())/os.sysconf('SC_CLK_TCK')}

def cli(*args, check=True):
    r=subprocess.run(base+list(args),env=env,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=120)
    if check and r.returncode: raise RuntimeError(f'{args}: {r.stderr}')
    return r.stdout.strip()
def api(path):
    r=subprocess.run(['curl','-fsS','--unix-socket',str(socket),'http://localhost'+path],text=True,capture_output=True,timeout=15)
    if r.returncode: raise RuntimeError(r.stderr)
    return json.loads(r.stdout)
def start_api():
    log=open(work/f'api-{len(processes)}.log','w');logs.append(log)
    proc=Managed(base+['system','service','--time=0','unix://'+str(socket)],log);processes.append(proc)
    wait(lambda: socket.exists() and api('/version'),10)
    return proc
def start_scheduler():
    log=open(work/f'scheduler-{len(processes)}.log','w');logs.append(log)
    proc=Managed([str(root/'build/podman-healthcheckd'),'--socket',str(socket)],log);processes.append(proc)
    return proc
def stop(proc):
    proc.terminate();proc.wait(timeout=12)
def wait(fn, timeout=20):
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        try:
            value=fn()
            if value:return value
        except (RuntimeError,FileNotFoundError):pass
        time.sleep(.1)
    raise RuntimeError('condition timed out')
def inspect(id):return api('/v6.1.4/libpod/containers/'+id+'/json')
def container(name,*opts):
    id=cli('run','-d','--name',name,'--label','io.podman.runit.test=true','--network','none',*opts,'docker.io/library/busybox:1.37.0','sleep','600');ids.append(id);return id
try:
    evidence['engine']=cli('--version')
    api_proc=start_api()
    evidence['version']=api('/version')
    evidence['info']=api('/v6.1.4/libpod/info')['host']['eventLogger']
    scheduler=start_scheduler()
    sample('no containers')
    cli('pull','docker.io/library/busybox:1.37.0')
    none_id=container('pr-none','--no-healthcheck')
    sample('one container without checks')
    default_id=container('pr-default','--health-cmd','true')
    [container(f'pr-long-{n}','--health-cmd','true') for n in range(2)]
    sample('three checks with 30s intervals')
    pass_id=container('pr-pass','--health-cmd','true','--health-interval','500ms','--health-start-period','60s')
    disabled_id=container('pr-disabled','--health-cmd','true','--health-interval','disable')
    fail_id=container('pr-fail','--health-cmd','false','--health-interval','500ms','--health-retries','2')
    timeout_id=container('pr-timeout','--health-cmd','sleep 3','--health-interval','500ms','--health-timeout','1s')
    startup_id=container('pr-startup','--health-cmd','true','--health-startup-cmd','true','--health-startup-success','2')
    stop_id=container('pr-onfailure','--health-cmd','false','--health-interval','500ms','--health-retries','1','--health-on-failure','stop')
    evidence['inspect_configs']={name:{k:v for k,v in inspect(id)['Config'].items() if k in ('Healthcheck','StartupHealthCheck')} for name,id in [('default',default_id),('disabled',disabled_id),('startup',startup_id)]}
    wait(lambda: inspect(pass_id)['State']['Health']['Status']=='healthy')
    wait(lambda: inspect(fail_id)['State']['Health']['Status']=='unhealthy')
    wait(lambda: len(inspect(timeout_id)['State']['Health']['Log'] or [])>0)
    wait(lambda: not inspect(stop_id)['State']['Running'])
    assert not inspect(disabled_id)['State']['Health']['Log']
    assert not inspect(startup_id)['State']['Health']['Log']
    cli('update','--health-interval','2s',pass_id)
    wait(lambda: inspect(pass_id)['Config']['Healthcheck']['Interval']==2000000000)
    [container(f'pr-short-{n}','--health-cmd','true','--health-interval','500ms') for n in range(4)]
    sample('seven short interval checks including slow failing probes')
    last=inspect(pass_id)['State']['Health']['Log'][-1]['End']
    stop(scheduler);scheduler=start_scheduler()
    wait(lambda: inspect(pass_id)['State']['Health']['Log'][-1]['End']>last)
    stop(api_proc);api_proc=start_api()
    last=inspect(pass_id)['State']['Health']['Log'][-1]['End']
    wait(lambda: inspect(pass_id)['State']['Health']['Log'][-1]['End']>last)
    for _ in range(3):
        stop(scheduler);scheduler=start_scheduler()
        time.sleep(.3)
    sample('after repeated scheduler restarts')
    compose=work/'compose.yaml'
    compose.write_text('''services:
  checked:
    image: docker.io/library/busybox:1.37.0
    command: ["sleep", "600"]
    network_mode: none
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      start_period: 60s
      start_interval: 250ms
  dependent:
    image: docker.io/library/busybox:1.37.0
    command: ["sleep", "600"]
    network_mode: none
    depends_on:
      checked:
        condition: service_healthy
''')
    env['DOCKER_HOST']='unix://'+str(socket)
    began=time.monotonic();cli('compose','-p','prisolated','-f',str(compose),'up','-d');evidence['compose_seconds']=time.monotonic()-began
    created=json.loads(cli('ps','--format','json'));checked=next(x['Id'] for x in created if 'prisolated-checked-1' in x['Names']);dependent=next(x['Id'] for x in created if 'prisolated-dependent-1' in x['Names'])
    assert inspect(dependent)['State']['Running']
    evidence['compose_healthcheck']=inspect(checked)['Config']['Healthcheck']
    evidence['checks']=['CMD and CMD-SHELL health commands','first success during start_period','retries/unhealthy','timeout','disabled interval','startup excluded','failure action stop','interval update','scheduler restart','API restart','actual Docker Compose service_healthy']
    evidence['scheduler_proc_status']=(pathlib.Path('/proc')/str(scheduler.pid)/'status').read_text()
    evidence['api_proc_status']=(pathlib.Path('/proc')/str(api_proc.pid)/'status').read_text()
    evidence['scheduler_status']=json.loads((scheduler_dir/'status.json').read_text())
    evidence['result']='passed'
finally:
    for proc in reversed(processes):
        if proc.poll() is None:
            try:stop(proc)
            except subprocess.TimeoutExpired:proc.kill();proc.wait()
    # Explicitly isolated storage arguments, even if Compose set remote env.
    env.pop('CONTAINER_HOST',None);env.pop('DOCKER_HOST',None)
    cli('rm','-af',check=False)
    if scheduler_dir.exists():
        shutil.copytree(scheduler_dir, work/'scheduler-state')
        shutil.rmtree(scheduler_dir)
    for log in logs:log.close()
    (work/'evidence.json').write_text(json.dumps(evidence,indent=2))
    print('Evidence:',work)
    if evidence.get('result')=='passed':
        dest=root/'build/integration-evidence.json';shutil.copy(work/'evidence.json',dest);print('PASS',dest)
        if not a.keep:shutil.rmtree(work)
