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
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorSnapshotRaceReplayOverflowReconnect(t *testing.T) {
	dir, err := os.MkdirTemp("", "pr-mon-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "api")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var streamCount, listCount atomic.Int32
	var present atomic.Bool
	disconnect := make(chan struct{})
	burst := make(chan struct{})
	var missing atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			if missing.Load() {
				w.WriteHeader(503)
				return
			}
			w.Header().Set("Libpod-API-Version", "6.1.4")
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/info"):
			fmt.Fprint(w, `{"host":{"eventLogger":"file","security":{"rootless":true}}}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			n := streamCount.Add(1)
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			if n == 1 {
				present.Store(true)
				fmt.Fprintf(w, `{"Type":"container","Action":"start","Actor":{"ID":"%s"}}`+"\n", fixture(1).ID)
				w.(http.Flusher).Flush()
				select {
				case <-burst:
				case <-r.Context().Done():
					return
				}
				// Old stop events must cause inspection, not authoritative removal. Flood
				// the bounded queue to force resynchronization without losing identity.
				for i := 0; i < 2000; i++ {
					fmt.Fprintf(w, `{"Type":"container","Action":"stop","Actor":{"ID":"%s"}}`+"\n", fixture(1).ID)
				}
				w.(http.Flusher).Flush()
				select {
				case <-disconnect:
					return
				case <-r.Context().Done():
					return
				}
			}
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			n := listCount.Add(1)
			if n == 1 {
				time.Sleep(30 * time.Millisecond)
				fmt.Fprint(w, `[]`)
			} else if present.Load() {
				fmt.Fprintf(w, `[{"Id":"%s"}]`, fixture(1).ID)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default:
			json.NewEncoder(w).Encode(fixture(1))
		}
	})}
	go server.Serve(l)
	defer server.Close()
	c := podmanapi.New(socket)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Update, 100)
	refresh := make(chan struct{}, 1)
	done := make(chan struct{})
	missing.Store(true)
	go func() { defer close(done); Monitor(ctx, c, out, refresh, time.Hour) }()
	next := func(test func(Update) bool) Update {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case u := <-out:
				if test(u) {
					return u
				}
			case <-deadline.C:
				t.Fatal("monitor did not converge")
				return Update{}
			}
		}
	}
	next(func(u Update) bool { return u.State == "waiting" })
	missing.Store(false)
	next(func(u Update) bool { return len(u.Containers) == 1 })
	close(burst)
	next(func(u Update) bool { return len(u.Containers) == 1 })
	close(disconnect)
	next(func(u Update) bool { return u.State == "waiting" })
	next(func(u Update) bool { return u.Full && len(u.Containers) == 1 && streamCount.Load() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor goroutine leaked")
	}
}
