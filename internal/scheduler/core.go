package scheduler

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"time"

	"podman-runit/internal/podmanapi"
)

const ExclusionLabel = "io.podman.runit.healthcheck"

// Timing reads stored native inspect values. Stock 6.1.4 omits zero intervals in JSON; defaults are stored explicitly.
// Neither an omitted nor zero interval enables automatic checks.
func Timing(c podmanapi.Container) (interval, budget time.Duration, reason string) {
	if !c.State.Running || c.State.Paused {
		return 0, 0, "not running"
	}
	h := c.Config.Healthcheck
	if h == nil || len(h.Test) == 0 || h.Test[0] == "NONE" {
		return 0, 0, "no health check"
	}
	if c.Config.Labels[ExclusionLabel] == "false" {
		return 0, 0, "excluded by label"
	}
	if len(c.Config.StartupHealthCheck) > 0 && string(c.Config.StartupHealthCheck) != "null" {
		return 0, 0, "unsupported Podman startup probe"
	}
	if h.Interval == nil {
		return 0, 0, "automatic interval disabled"
	}
	if *h.Interval <= 0 {
		return 0, 0, "automatic interval disabled"
	}
	if *h.Interval > 365*24*time.Hour {
		return 0, 0, "unsupported interval over one year"
	}
	if (h.Test[0] == "CMD" || h.Test[0] == "CMD-SHELL") && (len(h.Test) < 2 || h.Test[1] == "") {
		return 0, 0, "no health check"
	}
	timeout := 30 * time.Second
	if h.Timeout != nil {
		timeout = *h.Timeout
	}
	// Unlimited probes get a finite client budget; cancellation cannot stop the
	// server-side command. Recovery remains deliberately conservative.
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if timeout > 24*time.Hour {
		return 0, 0, "unsupported timeout over 24h"
	}
	return *h.Interval, timeout + 30*time.Second, ""
}

type item struct {
	ID, Signature    string
	Generation       uint64
	Interval, Budget time.Duration
	Due              time.Time
	index            int
}
type queue []*item

func (q queue) Len() int           { return len(q) }
func (q queue) Less(i, j int) bool { return q[i].Due.Before(q[j].Due) }
func (q queue) Swap(i, j int)      { q[i], q[j] = q[j], q[i]; q[i].index = i; q[j].index = j }
func (q *queue) Push(v any)        { i := v.(*item); i.index = len(*q); *q = append(*q, i) }
func (q *queue) Pop() any {
	old := *q
	i := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	i.index = -1
	return i
}

type Job struct {
	ID         string
	Generation uint64
	Budget     time.Duration
}
type Core struct {
	items      map[string]*item
	busy       map[string]Job
	Holds      map[string]time.Time
	Reasons    map[string]string
	q          queue
	generation uint64
}

func NewCore(holds map[string]time.Time) *Core {
	if holds == nil {
		holds = map[string]time.Time{}
	}
	return &Core{items: map[string]*item{}, busy: map[string]Job{}, Holds: holds, Reasons: map[string]string{}}
}
func (c *Core) Remove(id string) {
	if i := c.items[id]; i != nil {
		if i.index >= 0 {
			heap.Remove(&c.q, i.index)
		}
		delete(c.items, id)
	}
	delete(c.Reasons, id)
}
func (c *Core) Update(v podmanapi.Container, now time.Time) {
	interval, budget, reason := Timing(v)
	if reason != "" {
		c.Remove(v.ID)
		c.Reasons[v.ID] = reason
		return
	}
	delete(c.Reasons, v.ID)
	raw, _ := json.Marshal(v.Config)
	signature := fmt.Sprintf("%s/%s", v.State.StartedAt.Format(time.RFC3339Nano), raw)
	if old := c.items[v.ID]; old != nil && old.Signature == signature {
		return
	}
	c.Remove(v.ID)
	c.generation++
	i := &item{ID: v.ID, Signature: signature, Generation: c.generation, Interval: interval, Budget: budget, Due: now, index: -1}
	// Persisted wall time is converted into a fresh monotonic deadline.
	latest := time.Time{}
	for _, l := range v.State.Health.Log {
		if !l.Start.Before(v.State.StartedAt) && !l.End.Before(l.Start) && l.End.After(latest) && !l.End.After(now) {
			latest = l.End
		}
	}
	if !latest.IsZero() {
		delay := latest.Add(interval).Sub(now)
		if delay > 0 {
			i.Due = now.Add(delay)
		}
	}
	if hold := c.Holds[v.ID]; hold.After(i.Due) {
		i.Due = now.Add(hold.Sub(now))
	}
	c.items[v.ID] = i
	if _, busy := c.busy[v.ID]; !busy {
		heap.Push(&c.q, i)
	}
}
func (c *Core) Next() (time.Time, bool) {
	if len(c.q) == 0 {
		return time.Time{}, false
	}
	return c.q[0].Due, true
}
func (c *Core) Take(now time.Time) (Job, bool) {
	if len(c.q) == 0 || c.q[0].Due.After(now) {
		return Job{}, false
	}
	i := heap.Pop(&c.q).(*item)
	j := Job{i.ID, i.Generation, i.Budget}
	c.busy[i.ID] = j
	// Written durably before the HTTP request, this survives abrupt daemon death.
	c.Holds[i.ID] = now.Add(2*i.Budget + i.Interval)
	return j, true
}
func (c *Core) Complete(j Job, now time.Time, err error) {
	delete(c.busy, j.ID)
	if err == nil {
		delete(c.Holds, j.ID)
	} else {
		c.Holds[j.ID] = now.Add(j.Budget)
	}
	i := c.items[j.ID]
	if i == nil {
		if _, known := c.Reasons[j.ID]; !known {
			delete(c.Holds, j.ID)
		}
		return
	}
	i.Due = now.Add(i.Interval)
	if i.Generation != j.Generation {
		i.Due = now
	}
	if h := c.Holds[j.ID]; h.After(i.Due) {
		i.Due = now.Add(h.Sub(now))
	}
	if i.index < 0 {
		heap.Push(&c.q, i)
	}
	// An obsolete generation's completion never restores its configuration or
	// health state. A new generation gets its prompt first check once the old
	// request has ended, subject only to an uncertainty hold.
}
func (c *Core) Count() int { return len(c.items) }
func (c *Core) PruneHolds(now time.Time) {
	for id, t := range c.Holds {
		if !t.After(now) {
			if _, ok := c.busy[id]; !ok {
				delete(c.Holds, id)
			}
		}
	}
}
