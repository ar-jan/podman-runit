package scheduler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"podman-runit/internal/podmanapi"
)

type Update struct {
	Full          bool
	Containers    []podmanapi.Container
	Removed       []string
	State, Detail string
}

func snapshot(ctx context.Context, c *podmanapi.Client) (Update, error) {
	u := Update{Full: true, State: "ready"}
	ids, err := c.List(ctx)
	if err != nil {
		return u, err
	}
	for _, id := range ids {
		v, err := c.Inspect(ctx, id)
		if podmanapi.IsRace(err) {
			continue
		}
		if err != nil {
			return Update{}, err
		}
		u.Containers = append(u.Containers, v)
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
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		began := time.Now()
		err := session(ctx, c, out, refresh, reconcile)
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
func session(parent context.Context, c *podmanapi.Client, out chan<- Update, refresh <-chan struct{}, reconcile time.Duration) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if _, err := c.Version(ctx); err != nil {
		return err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if !info.Host.Security.Rootless {
		return fmt.Errorf("rootful engine unsupported")
	}
	if info.Host.EventLogger != "file" && info.Host.EventLogger != "journald" {
		return fmt.Errorf("event backend %q unsupported: configure events_logger=file in containers.conf", info.Host.EventLogger)
	}
	body, err := c.Events(ctx, time.Now().Add(-2*time.Second))
	if err != nil {
		return err
	}
	defer body.Close()
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
	full := func() error {
		u, err := snapshot(ctx, c)
		if err == nil {
			send(ctx, out, u)
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
			if len(pending) >= 256 {
				if err := full(); err != nil {
					return err
				}
				clear(pending)
			}
		case <-debounce.C:
			u := Update{State: "ready"}
			for id := range pending {
				v, err := c.Inspect(ctx, id)
				if podmanapi.IsRace(err) {
					u.Removed = append(u.Removed, id)
				} else if err != nil {
					return err
				} else {
					u.Containers = append(u.Containers, v)
				}
			}
			clear(pending)
			send(ctx, out, u)
		}
	}
}
