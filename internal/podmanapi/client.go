// Package podmanapi implements the small, read-mostly native API surface used by
// the scheduler. Health is the only operation that changes engine state.
package podmanapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// NativeVersion is the validated native API schema. Requests use it in their
// route, which later Podman releases of the same major version still serve.
const NativeVersion = "6.1.4"
const maxResponse = 16 << 20

type HealthConfig struct {
	Test          []string
	Interval      *time.Duration
	Timeout       *time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
	Retries       int
}
type Container struct {
	ID    string `json:"Id"`
	State struct {
		Running   bool
		Paused    bool
		StartedAt time.Time
		Health    struct {
			Status string
			Log    []struct{ Start, End time.Time }
		}
	}
	Config struct {
		Healthcheck        *HealthConfig
		StartupHealthCheck json.RawMessage
		Labels             map[string]string
	}
}
type Info struct {
	Host struct {
		EventLogger string
		Security    struct{ Rootless bool }
	}
}
type Version struct {
	Version    string
	ApiVersion string
	Components []struct {
		Name    string
		Details map[string]string
	}
	Native string `json:"-"`
}
type Event struct {
	Type, Action string
	Actor        struct{ ID string }
	TimeNano     int64
}

var Actions = []string{"start", "restart", "restore", "stop", "died", "exited", "remove", "pause", "unpause", "update"}

func Lifecycle(e Event) bool {
	if e.Type != "container" {
		return false
	}
	for _, a := range Actions {
		if a == e.Action {
			return true
		}
	}
	return false
}

type HTTPError struct {
	Code    int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Podman HTTP %d", e.Code)
	}
	return fmt.Sprintf("Podman HTTP %d: %s", e.Code, e.Message)
}
func IsRace(err error) bool {
	var e *HTTPError
	return errors.As(err, &e) && (e.Code == 404 || e.Code == 409)
}

type Stats struct{ Metadata, Health, Streams uint64 }
type Client struct {
	metadata, execution, events             *http.Client
	metadataCount, healthCount, streamCount atomic.Uint64
}

func (c *Client) Stats() Stats {
	return Stats{c.metadataCount.Load(), c.healthCount.Load(), c.streamCount.Load()}
}

func New(socket string) *Client {
	makeClient := func(fresh bool, timeout time.Duration, headerTimeout time.Duration) *http.Client {
		tr := &http.Transport{Proxy: nil, DisableKeepAlives: fresh, MaxConnsPerHost: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: headerTimeout,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
			}}
		if fresh {
			// The scheduler's workers bound probe concurrency. A connection cap
			// would only queue probes while their budget runs out unsent.
			tr.MaxConnsPerHost = 0
		}
		return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	// Go retries GETs only on previously used connections. Health executions always
	// get a fresh connection, so a lost response can never replay the operation.
	return &Client{metadata: makeClient(false, 10*time.Second, 10*time.Second), execution: makeClient(true, 0, 0), events: makeClient(false, 0, 10*time.Second)}
}
func (c *Client) Close() {
	c.metadata.CloseIdleConnections()
	c.execution.CloseIdleConnections()
	c.events.CloseIdleConnections()
}
func request(ctx context.Context, c *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://podman"+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		defer res.Body.Close()
		var model struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&model)
		return nil, &HTTPError{res.StatusCode, model.Message}
	}
	return res, nil
}
func decode(res *http.Response, out any) error {
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(b) > maxResponse {
		return fmt.Errorf("%w: exceeds size limit", errUnusable)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%w: %w", errUnusable, err)
	}
	return nil
}

// errUnusable marks a complete response whose body cannot be used, as opposed
// to a connection that failed while reading it.
var errUnusable = errors.New("unusable Podman response")

func (c *Client) get(ctx context.Context, path string, out any) error {
	c.metadataCount.Add(1)
	res, err := request(ctx, c.metadata, path)
	if err != nil {
		return err
	}
	return decode(res, out)
}
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	c.metadataCount.Add(1)
	res, err := request(ctx, c.metadata, "/version")
	if err != nil {
		return v, err
	}
	v.Native = res.Header.Get("Libpod-API-Version")
	if err = decode(res, &v); err != nil {
		return v, err
	}
	if v.Native == "" {
		for _, p := range v.Components {
			if p.Name == "Podman Engine" {
				v.Native = p.Details["APIVersion"]
			}
		}
	}
	// The native API is semantically versioned, so later 6.x releases keep the
	// validated schema; another major version stays gated until validated.
	// Docker's top-level ApiVersion is never a candidate for native negotiation.
	if !supportedNative(v.Native) {
		return v, fmt.Errorf("native API %q unsupported; requires %s or a later release of the same major version", v.Native, NativeVersion)
	}
	return v, nil
}

// supportedNative accepts NativeVersion and later releases with the same major
// version. Pre-release and build suffixes are ignored.
func supportedNative(version string) bool {
	got, ok := parseVersion(version)
	want, _ := parseVersion(NativeVersion)
	if !ok || got[0] != want[0] {
		return false
	}
	return got[1] > want[1] || got[1] == want[1] && got[2] >= want[2]
}
func parseVersion(version string) ([3]int, bool) {
	var out [3]int
	if i := strings.IndexAny(version, "-+"); i >= 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
func route(path string) string { return "/v" + NativeVersion + "/libpod/" + path }

// CheckEngine rejects engines the scheduler does not support.
func CheckEngine(i Info) error {
	if !i.Host.Security.Rootless {
		return errors.New("rootful engine unsupported")
	}
	if i.Host.EventLogger != "file" && i.Host.EventLogger != "journald" {
		return fmt.Errorf("event backend %q unsupported: configure events_logger=file in containers.conf", i.Host.EventLogger)
	}
	return nil
}
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.get(ctx, route("info"), &i)
	return i, err
}
func ValidID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// ErrListTimeout means the all-container listing timed out while /_ping still
// answered. Known containers can still be inspected individually.
var ErrListTimeout = errors.New("container listing timed out while Podman stayed responsive")

func (c *Client) List(ctx context.Context) ([]string, error) {
	var entries []struct{ ID string }
	if err := c.get(ctx, route("containers/json?all=true"), &entries); err != nil {
		if c.responsiveAfterTimeout(ctx, err) {
			return nil, fmt.Errorf("%w: %w", ErrListTimeout, err)
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if !ValidID(e.ID) {
			return nil, errors.New("invalid full container ID")
		}
		ids = append(ids, e.ID)
	}
	return ids, nil
}

// ContainerError reports an inspect failure confined to one container: Podman
// answered, but with an error status or an unusable description. Races (404,
// 409) and connection failures are returned as other errors.
type ContainerError struct {
	ID  string
	Err error
}

func (e *ContainerError) Error() string { return e.Err.Error() }
func (e *ContainerError) Unwrap() error { return e.Err }

func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	var v Container
	if !ValidID(id) {
		return v, errors.New("invalid full container ID")
	}
	err := c.get(ctx, route("containers/"+id+"/json"), &v)
	var httpErr *HTTPError
	switch {
	case IsRace(err):
		return v, err
	case errors.As(err, &httpErr) || errors.Is(err, errUnusable):
		return v, &ContainerError{id, err}
	// Inspect waits for the container's lock, which a stuck operation on that
	// container can hold indefinitely. If Podman still answers a lock-free
	// request, the timeout belongs to this container, not the engine.
	case c.responsiveAfterTimeout(ctx, err):
		return v, &ContainerError{id, fmt.Errorf("inspect timed out after %s while Podman stayed responsive", c.metadata.Timeout)}
	case err != nil:
		return v, err
	case v.State.Running && v.State.StartedAt.IsZero():
		return v, &ContainerError{id, errors.New("running inspect missing start generation")}
	case v.ID != id:
		return v, &ContainerError{id, errors.New("inspect ID mismatch")}
	}
	return v, nil
}

// /_ping takes no container lock, even when list or inspect has timed out.
func (c *Client) responsiveAfterTimeout(ctx context.Context, err error) bool {
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() || ctx.Err() != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.metadataCount.Add(1)
	res, err := request(ctx, c.metadata, "/_ping")
	if err != nil {
		return false
	}
	res.Body.Close()
	return true
}

// probeTimeout prefixes Podman's define.ErrHealthCheckTimeout message.
const probeTimeout = "healthcheck command exceeded timeout"

// Health returns nil once Podman has run and recorded a probe, whether it
// passed, failed or timed out. An error leaves the execution uncertain.
func (c *Client) Health(ctx context.Context, id string) error {
	if !ValidID(id) {
		return errors.New("invalid full container ID")
	}
	c.healthCount.Add(1)
	res, err := request(ctx, c.execution, route("containers/"+id+"/healthcheck"))
	// Podman 6 answers a timed-out probe with HTTP 500 after logging it as a
	// failed check, so the next probe follows the interval as with Podman's own
	// timer. Podman leaves the timed-out command running in the container.
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.Code == 500 && strings.HasPrefix(httpErr.Message, probeTimeout) {
		return nil
	}
	if err != nil {
		return err
	}
	var out struct{ Status string }
	if err := decode(res, &out); err != nil {
		return err
	}
	if out.Status == "" {
		return errors.New("health response missing operation status")
	}
	return nil
}

// Events returns only after response headers arrive. Podman flushes them after
// registering its reader. A timestamp overlap also covers file-reader startup.
func (c *Client) Events(ctx context.Context, since time.Time) (io.ReadCloser, error) {
	filters, _ := json.Marshal(map[string][]string{"type": {"container"}, "event": Actions})
	q := url.Values{"stream": {"true"}, "since": {since.UTC().Format(time.RFC3339Nano)}, "filters": {string(filters)}}
	c.streamCount.Add(1)
	res, err := request(ctx, c.events, route("events?")+q.Encode())
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}
func ReadEvents(body io.Reader, emit func(Event)) error {
	scan := bufio.NewScanner(body)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var e Event
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			return err
		}
		if Lifecycle(e) && ValidID(e.Actor.ID) {
			emit(e)
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	return io.EOF
}
