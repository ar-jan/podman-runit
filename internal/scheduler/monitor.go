package scheduler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"podman-runit/internal/podmanapi"
)

type Update struct {
	Full       bool
	Containers []podmanapi.Container
	Removed    []string
	// Failed maps containers that exist but could not be inspected to the
	// reason. They stay known, so their recovery holds are kept.
	Failed        map[string]string
	State, Detail string
}

// inspect adds one container to u. Podman failing to describe this container
// alone excludes it; only engine-level errors are returned.
func inspect(ctx context.Context, c *podmanapi.Client, id string, u *Update) error {
	v, err := c.Inspect(ctx, id)
	var fault *podmanapi.ContainerError
	switch {
	case podmanapi.IsRace(err):
		u.Removed = append(u.Removed, id)
	case errors.As(err, &fault):
		if u.Failed == nil {
			u.Failed = map[string]string{}
		}
		u.Failed[id] = fault.Error()
	case err != nil:
		return err
	default:
		u.Containers = append(u.Containers, v)
	}
	return nil
}
func snapshot(ctx context.Context, c *podmanapi.Client, known map[string]bool) (Update, error) {
	ids, err := c.List(ctx)
	u := Update{Full: true, State: "ready"}
	if errors.Is(err, podmanapi.ErrListTimeout) {
		ids = slices.Sorted(maps.Keys(known))
		u.Full = false
		u.Detail = "container listing timed out; inspecting known containers until discovery recovers"
	} else if err != nil {
		return Update{}, err
	}
	for _, id := range ids {
		if err := inspect(ctx, c, id, &u); err != nil {
			return Update{}, err
		}
	}
	if !u.Full && len(u.Containers) == 0 {
		// Keep the subscription open. A lifecycle event may identify a
		// container we can inspect before the next successful listing.
		u.State = "waiting"
		return u, nil
	}
	// One broken container is excluded, but when every container fails the
	// engine is the likelier fault: report it rather than claim readiness.
	if len(u.Failed) > 0 && len(u.Containers) == 0 {
		for _, id := range ids {
			if reason, ok := u.Failed[id]; ok {
				return Update{}, fmt.Errorf("inspect failed for all %d containers (%.12s: %s)", len(u.Failed), id, reason)
			}
		}
	}
	return u, nil
}
func send(ctx context.Context, out chan<- Update, u Update) bool {
	select {
	case out <- u:
		return true
	case <-ctx.Done():
		return false
	}
}

// Monitor owns API synchronization, independently of the execution workers.
func Monitor(ctx context.Context, c *podmanapi.Client, out chan<- Update, refresh <-chan struct{}, reconcile time.Duration) {
	known := make(map[string]bool)
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		began := time.Now()
		err := session(ctx, c, out, refresh, reconcile, known)
		if ctx.Err() != nil {
			return
		}
		send(ctx, out, Update{State: "waiting", Detail: fmt.Sprint(err)})
		if time.Since(began) > time.Minute {
			backoff = 250 * time.Millisecond
		}
		timer := time.NewTimer(backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}
func session(parent context.Context, c *podmanapi.Client, out chan<- Update, refresh <-chan struct{}, reconcile time.Duration, known map[string]bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if _, err := c.Version(ctx); err != nil {
		return err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if err := podmanapi.CheckEngine(info); err != nil {
		return err
	}
	body, err := c.Events(ctx, time.Now().Add(-2*time.Second))
	if err != nil {
		return err
	}
	events := make(chan podmanapi.Event, 256)
	overflow := make(chan struct{}, 1)
	ended := make(chan error, 1)
	go func() {
		ended <- podmanapi.ReadEvents(body, func(e podmanapi.Event) {
			select {
			case events <- e:
			default:
				select {
				case overflow <- struct{}{}:
				default:
				}
			}
		})
	}()
	defer func() { cancel(); body.Close(); <-ended }()
	send(ctx, out, Update{State: "synchronizing"})
	publish := func(u Update) {
		if u.Full {
			clear(known)
		}
		for _, v := range u.Containers {
			known[v.ID] = true
		}
		for id := range u.Failed {
			known[id] = true
		}
		for _, id := range u.Removed {
			delete(known, id)
		}
		send(ctx, out, u)
	}
	var discoveryDetail string
	full := func() error {
		u, err := snapshot(ctx, c, known)
		if err == nil {
			discoveryDetail = u.Detail
			publish(u)
		}
		return err
	}
	if err := full(); err != nil {
		return err
	}
	periodic := time.NewTicker(reconcile)
	defer periodic.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	pending := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ended:
			ended <- err
			return err
		case <-overflow:
			if err := full(); err != nil {
				return err
			}
			clear(pending)
		case <-refresh:
			if err := full(); err != nil {
				return err
			}
		case <-periodic.C:
			if err := full(); err != nil {
				return err
			}
		case e := <-events:
			if len(pending) == 0 {
				debounce.Reset(50 * time.Millisecond)
			}
			pending[e.Actor.ID] = true
			known[e.Actor.ID] = true
			if len(pending) >= 256 {
				if err := full(); err != nil {
					return err
				}
				clear(pending)
			}
		case <-debounce.C:
			u := Update{Detail: discoveryDetail}
			for id := range pending {
				if err := inspect(ctx, c, id, &u); err != nil {
					return err
				}
			}
			clear(pending)
			if len(u.Containers) > 0 || discoveryDetail == "" {
				u.State = "ready"
			}
			publish(u)
		}
	}
}
