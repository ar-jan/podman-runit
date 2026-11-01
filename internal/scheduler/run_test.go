package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"podman-runit/internal/podmanapi"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunBoundedWorkersMetadataAndShutdown(t *testing.T) {
	dir, err := os.MkdirTemp("", "pr-run-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "api")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var active, peak, inspects atomic.Int32
	var subscribed atomic.Bool
	var lock sync.Mutex
	called := map[string]bool{}
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			w.Header().Set("Libpod-API-Version", "6.1.4")
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/info"):
			fmt.Fprint(w, `{"host":{"eventLogger":"file","security":{"rootless":true}}}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			subscribed.Store(true)
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			if !subscribed.Load() {
				t.Error("snapshot before subscription")
			}
			var ids []map[string]string
			for i := 1; i <= 8; i++ {
				ids = append(ids, map[string]string{"Id": fixture(i).ID})
			}
			json.NewEncoder(w).Encode(ids)
		case strings.HasSuffix(r.URL.Path, "/healthcheck"):
			id := strings.Split(r.URL.Path, "/")[4]
			lock.Lock()
			if called[id] {
				t.Error("duplicate or overlapping execution")
			}
			called[id] = true
			lock.Unlock()
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer active.Add(-1)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			fmt.Fprint(w, `{"Status":"healthy"}`)
		default:
			inspects.Add(1)
			id := strings.Split(r.URL.Path, "/")[4]
			v := fixture(1)
			v.ID = id
			json.NewEncoder(w).Encode(v)
		}
	})
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	defer server.Close()
	c := podmanapi.New(socket)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- Run(ctx, c, Options{Socket: socket, Runtime: dir, Workers: 4, Reconcile: time.Second, Drain: 20 * time.Millisecond})
	}()
	deadline := time.After(5 * time.Second)
	for active.Load() != 4 || inspects.Load() < 16 {
		select {
		case <-deadline:
			t.Fatal("workers or independent reconciliation stalled")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unbounded shutdown")
	}
	close(release)
	if peak.Load() != 4 {
		t.Fatal("worker bound", peak.Load())
	}
	b, err := os.ReadFile(filepath.Join(dir, "pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	var holds map[string]time.Time
	json.Unmarshal(b, &holds)
	if len(holds) != 4 {
		t.Fatal("shutdown lost uncertain execution intents", len(holds))
	}
}
func TestPartialSnapshotRejected(t *testing.T) {
	// An incomplete snapshot must never be published as an empty authoritative one.
	dir, err := os.MkdirTemp("", "pr-part-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "api")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "containers/json") {
			fmt.Fprintf(w, `[{"Id":"%s"}]`, fixture(1).ID)
		} else {
			w.WriteHeader(500)
		}
	})}
	go s.Serve(l)
	defer s.Close()
	c := podmanapi.New(socket)
	defer c.Close()
	u, err := snapshot(context.Background(), c)
	if err == nil || u.Full {
		t.Fatal("partial snapshot accepted")
	}
}
