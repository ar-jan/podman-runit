package podmanapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func server(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "pr-api-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: h}
	go s.Serve(l)
	c := New(socket)
	t.Cleanup(func() { c.Close(); s.Close(); os.RemoveAll(dir) })
	return c
}
func TestNativeVersion(t *testing.T) {
	for _, tc := range []struct {
		body, header string
		ok           bool
	}{
		{`{"ApiVersion":"1.44","Components":[{"Name":"Podman Engine","Details":{"APIVersion":"6.1.4"}}]}`, "", true},
		{`{"ApiVersion":"1.44"}`, "6.1.4", true},
		{`{"ApiVersion":"6.1.4"}`, "", false},
		{`{}`, "7.0.0", false},
	} {
		c := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Libpod-API-Version", tc.header)
			w.Write([]byte(tc.body))
		})
		_, err := c.Version(context.Background())
		if (err == nil) != tc.ok {
			t.Fatal(err)
		}
	}
}
func TestHealthNeverReplaysOrRedirects(t *testing.T) {
	var calls atomic.Int32
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	err := c.Health(context.Background(), strings.Repeat("a", 64))
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	c = server(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Redirect(w, r, "/other", 302) })
	calls.Store(0)
	if err := c.Health(context.Background(), strings.Repeat("a", 64)); err == nil || calls.Load() != 1 {
		t.Fatal("redirect followed")
	}
}
func TestResponseContracts(t *testing.T) {
	for _, code := range []int{200, 404, 409, 500} {
		c := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"Status":"unhealthy"}`))
		})
		err := c.Health(context.Background(), strings.Repeat("a", 64))
		if (err == nil) != (code == 200) {
			t.Fatal(code, err)
		}
		if IsRace(err) != (code == 404 || code == 409) {
			t.Fatal("race classification")
		}
	}
	var h HealthConfig
	if err := json.Unmarshal([]byte(`{"Interval":9007199254740993,"Timeout":1}`), &h); err != nil {
		t.Fatal(err)
	}
	if *h.Interval != 9007199254740993 {
		t.Fatal("duration precision lost")
	}
	c := server(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`not json`)) })
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("malformed response accepted")
	}
}
func TestEventBarrierIdleAndSchema(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "" {
			t.Error("missing overlap")
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	body, err := c.Events(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	body.Close()
	count := 0
	err = ReadEvents(strings.NewReader(`{"Type":"container","Action":"start","Actor":{"ID":"`+strings.Repeat("b", 64)+`"},"timeNano":123}`+"\n"+`{"Type":"container","Action":"exec","Actor":{"ID":"`+strings.Repeat("b", 64)+`"}}`+"\n"), func(e Event) { count++ })
	if count != 1 || err == nil {
		t.Fatal(count, err)
	}
}
