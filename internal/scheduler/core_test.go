package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"podman-runit/internal/podmanapi"
	"testing"
	"time"
)

func fixture(id int) podmanapi.Container {
	var c podmanapi.Container
	c.ID = fmt.Sprintf("%064x", id)
	c.State.Running = true
	c.State.StartedAt = time.Unix(100, 0)
	d := 250 * time.Millisecond
	timeout := time.Second
	c.Config.Healthcheck = &podmanapi.HealthConfig{Test: []string{"CMD", "true"}, Interval: &d, Timeout: &timeout, StartPeriod: time.Hour}
	return c
}
func TestEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		edit     func(*podmanapi.Container)
		eligible bool
	}{
		{"start period", func(c *podmanapi.Container) {}, true},
		{"absent", func(c *podmanapi.Container) { c.Config.Healthcheck = nil }, false},
		{"NONE", func(c *podmanapi.Container) { c.Config.Healthcheck.Test = []string{"NONE"} }, false},
		{"empty", func(c *podmanapi.Container) { c.Config.Healthcheck.Test = nil }, false},
		{"missing interval", func(c *podmanapi.Container) { c.Config.Healthcheck.Interval = nil }, false},
		{"zero", func(c *podmanapi.Container) { *c.Config.Healthcheck.Interval = 0 }, false},
		{"paused", func(c *podmanapi.Container) { c.State.Paused = true }, false},
		{"stopped", func(c *podmanapi.Container) { c.State.Running = false }, false},
		{"label", func(c *podmanapi.Container) { c.Config.Labels = map[string]string{ExclusionLabel: "false"} }, false},
		{"startup", func(c *podmanapi.Container) {
			c.Config.StartupHealthCheck = json.RawMessage(`{"Test":["CMD","true"],"Successes":2}`)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(1)
			tc.edit(&c)
			_, _, why := Timing(c)
			if (why == "") != tc.eligible {
				t.Fatal(why)
			}
		})
	}
}
func TestCompletionClockNoOverlapOrCatchup(t *testing.T) {
	c := NewCore(nil)
	v := fixture(1)
	now := time.Unix(200, 0)
	c.Update(v, now)
	j, ok := c.Take(now)
	if !ok {
		t.Fatal("first probe delayed by start_period")
	}
	if _, ok := c.Take(now.Add(time.Hour)); ok {
		t.Fatal("overlap")
	}
	end := now.Add(time.Hour)
	c.Complete(j, end, nil)
	if _, ok := c.Take(end.Add(249 * time.Millisecond)); ok {
		t.Fatal("not completion based")
	}
	if _, ok := c.Take(end.Add(250 * time.Millisecond)); !ok {
		t.Fatal("subsecond lost")
	}
}
func TestGenerationsAndBoundedQueue(t *testing.T) {
	c := NewCore(nil)
	v := fixture(1)
	now := time.Unix(200, 0)
	c.Update(v, now)
	job, _ := c.Take(now)
	for i := 0; i < 10000; i++ {
		v.State.StartedAt = now.Add(time.Duration(i))
		c.Update(v, now)
	}
	if _, ok := c.Take(now.Add(time.Hour)); ok {
		t.Fatal("restart overlaps outstanding request")
	}
	c.Complete(job, now, nil)
	j, ok := c.Take(now)
	if !ok || j.Generation == job.Generation {
		t.Fatal("obsolete generation restored")
	}
	c.Remove(v.ID)
	c.Complete(j, now, nil)
	if _, ok := c.Next(); ok {
		t.Fatal("removed container requeued")
	}
}
func TestHistoryAndUncertainCompletion(t *testing.T) {
	now := time.Unix(200, 0)
	v := fixture(1)
	// A previous start's health history must not defer the first check.
	v.State.Health.Log = append(v.State.Health.Log, struct{ Start, End time.Time }{time.Unix(50, 0), now})
	c := NewCore(nil)
	c.Update(v, now)
	j, ok := c.Take(now)
	if !ok {
		t.Fatal("old history used")
	}
	c.Complete(j, now, errors.New("lost response"))
	if _, ok := c.Take(now.Add(time.Second)); ok {
		t.Fatal("immediate ambiguous retry")
	}
	restarted := NewCore(c.Holds)
	restarted.Update(v, now)
	if _, ok := restarted.Take(now); ok {
		t.Fatal("restart discarded recovery hold")
	}
}
func TestLifecycleAndConfigRefresh(t *testing.T) {
	now := time.Unix(200, 0)
	v := fixture(1)
	c := NewCore(nil)
	c.Update(v, now)
	v.State.Paused = true
	c.Update(v, now)
	if _, ok := c.Take(now); ok {
		t.Fatal("paused")
	}
	v.State.Paused = false
	c.Update(v, now)
	j, ok := c.Take(now)
	if !ok {
		t.Fatal("unpause not discovered")
	}
	c.Complete(j, now, nil)
	*v.Config.Healthcheck.Interval = time.Minute
	c.Update(v, now)
	j, ok = c.Take(now)
	if !ok {
		t.Fatal("configuration not refreshed")
	}
	c.Complete(j, now, nil)
	if _, ok := c.Take(now.Add(time.Second)); ok {
		t.Fatal("old interval used")
	}
}

func TestStockInspectFixtures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		reason   string
	}{
		{"default", 30 * time.Second, ""}, {"disabled", 0, "automatic interval disabled"}, {"startup", 0, "unsupported Podman startup probe"},
	} {
		b, err := os.ReadFile("../podmanapi/testdata/stock-6.1.4-" + tc.name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var c podmanapi.Container
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		interval, _, why := Timing(c)
		if interval != tc.interval || why != tc.reason {
			t.Fatal(tc.name, interval, why)
		}
	}
	b, err := os.ReadFile("../podmanapi/testdata/stock-6.1.4-compose-health.json")
	if err != nil {
		t.Fatal(err)
	}
	var h podmanapi.HealthConfig
	json.Unmarshal(b, &h)
	if h.StartInterval != 0 || h.StartPeriod != time.Minute {
		t.Fatal("stock Compose contract changed")
	}
}
