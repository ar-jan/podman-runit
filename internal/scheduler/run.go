package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"podman-runit/internal/config"
	"podman-runit/internal/podmanapi"
)

type Status struct {
	Requests              podmanapi.Stats
	MaxSchedulingDelay    time.Duration
	PID                   int
	Started, Updated      time.Time
	Socket, State, Detail string
	Eligible, Running     int
	Excluded              map[string]string
}

func ReadStatus(dir string) (Status, error) {
	var s Status
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

type Options struct {
	Socket, Runtime  string
	Workers          int
	Reconcile, Drain time.Duration
}
type result struct {
	job Job
	err error
}

func Run(ctx context.Context, c *podmanapi.Client, o Options) error {
	if o.Workers < 1 || o.Workers > 64 || o.Reconcile < time.Second || o.Drain < 0 {
		return errors.New("invalid scheduler bounds")
	}
	holds := map[string]time.Time{}
	journal := filepath.Join(o.Runtime, "pending.json")
	if b, err := os.ReadFile(journal); err == nil {
		if err := json.Unmarshal(b, &holds); err != nil {
			return fmt.Errorf("invalid recovery journal: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	core := NewCore(holds)
	persist := func() error {
		b, err := json.Marshal(core.Holds)
		if err != nil {
			return err
		}
		return config.Atomic(journal, b)
	}
	status := Status{PID: os.Getpid(), Started: time.Now(), Socket: o.Socket, State: "waiting"}
	writeStatus := func() {
		status.Updated = time.Now()
		status.Requests = c.Stats()
		status.Eligible = core.Count()
		status.Running = len(core.busy)
		status.Excluded = core.Reasons
		b, _ := json.MarshalIndent(status, "", "  ")
		if err := config.Atomic(filepath.Join(o.Runtime, "status.json"), b); err != nil {
			log.Printf("status: %v", err)
		}
	}
	mctx, stopMonitor := context.WithCancel(ctx)
	defer stopMonitor()
	updates := make(chan Update, 1)
	refresh := make(chan struct{}, 1)
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); Monitor(mctx, c, updates, refresh, o.Reconcile) }()
	// Workers get their own lifetime: SIGTERM first stops discovery and dispatch,
	// then drains existing requests; only the drain deadline cancels them.
	wctx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	jobs := make(chan Job)
	results := make(chan result, o.Workers)
	done := make(chan struct{}, o.Workers)
	for n := 0; n < o.Workers; n++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := range jobs {
				probe, cancel := context.WithTimeout(wctx, j.Budget)
				err := c.Health(probe, j.ID)
				cancel()
				results <- result{j, err}
			}
		}()
	}
	defer func() {
		stopMonitor()
		<-monitorDone
		close(jobs)
		stopWorkers()
		for n := 0; n < o.Workers; n++ {
			<-done
		}
		status.State = "stopped"
		writeStatus()
	}()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	writeStatus()
	lastLog := time.Time{}
	applyResult := func(r result) {
		core.Complete(r.job, time.Now(), r.err)
		if r.err != nil {
			status.State = "degraded"
			status.Detail = "health execution uncertain; reconciling before recovery delay"
			select {
			case refresh <- struct{}{}:
			default:
			}
			if time.Since(lastLog) > 30*time.Second {
				log.Printf("health request: %v (recovery delay applied)", r.err)
				lastLog = time.Now()
			}
		}
	}
	for {
		timer.Stop()
		var due <-chan time.Time
		if status.State == "ready" && len(core.busy) < o.Workers {
			if at, ok := core.Next(); ok {
				timer.Reset(max(time.Until(at), 0))
				due = timer.C
			}
		}
		select {
		case <-ctx.Done():
			stopMonitor()
			status.State = "stopping"
			writeStatus()
			drain := time.NewTimer(o.Drain)
			for len(core.busy) > 0 {
				select {
				case r := <-results:
					applyResult(r)
				case <-drain.C:
					stopWorkers()
					_ = persist()
					return nil
				}
			}
			drain.Stop()
			return persist()
		case u := <-updates:
			if u.State != "" {
				if (u.State != status.State || lastLog.IsZero()) && (time.Since(lastLog) > 30*time.Second || u.State == "ready") {
					log.Printf("%s: %s", u.State, u.Detail)
					lastLog = time.Now()
				}
				status.State = u.State
				status.Detail = u.Detail
			}
			if u.Full {
				seen := map[string]bool{}
				for _, v := range u.Containers {
					seen[v.ID] = true
				}
				for id := range u.Failed {
					seen[id] = true
				}
				for id := range core.items {
					if !seen[id] {
						core.Remove(id)
					}
				}
				for id := range core.Holds {
					if !seen[id] {
						if _, busy := core.busy[id]; !busy {
							delete(core.Holds, id)
						}
					}
				}
				for id := range core.Reasons {
					if !seen[id] {
						delete(core.Reasons, id)
					}
				}
			}
			for _, id := range u.Removed {
				core.Remove(id)
				if _, busy := core.busy[id]; !busy {
					delete(core.Holds, id)
				}
			}
			for _, v := range u.Containers {
				old := core.Reasons[v.ID]
				core.Update(v, time.Now())
				if reason := core.Reasons[v.ID]; reason != old && strings.HasPrefix(reason, "unsupported") {
					log.Printf("container %s: %s", v.ID, reason)
				}
			}
			// Retried at the next reconcile or lifecycle event for the container.
			for id, why := range u.Failed {
				reason := "inspect failed: " + why
				if core.Reasons[id] != reason {
					log.Printf("container %s: %s", id, reason)
				}
				core.Exclude(id, reason)
			}
			writeStatus()
		case <-due:
			at, _ := core.Next()
			status.MaxSchedulingDelay = max(status.MaxSchedulingDelay, time.Since(at))
			if j, ok := core.Take(time.Now()); ok {
				if err := persist(); err != nil {
					return fmt.Errorf("persist execution intent: %w", err)
				}
				jobs <- j
			}
		case r := <-results:
			applyResult(r)
			if err := persist(); err != nil {
				return err
			}
		case <-heartbeat.C:
			core.PruneHolds(time.Now())
			writeStatus()
		}
	}
}
