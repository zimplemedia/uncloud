package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/internal/machine/caddyconfig"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testMachineID = "machine-1"

// eventLog records the order of fake Docker, store and wait events across goroutines.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// storeWrite is one CreateOrUpdateContainer call as the store would persist it.
type storeWrite struct {
	ID      string
	JSON    []byte
	Healthy bool
	// Passed is the exact object passed to CreateOrUpdateContainer.
	Passed api.ServiceContainer
}

// fakeStore implements containerStore and drainStore.
type fakeStore struct {
	log      *eventLog
	mu       sync.Mutex
	writes   []storeWrite
	writeErr func(ctr api.ServiceContainer) error
	// services answers ListContainers with a service filter (the drain's sibling check). Nil = one healthy sibling
	// "sib" of service "web" on another machine.
	services func(ctx context.Context, opts store.ListOptions) ([]store.ContainerRecord, error)
}

func (s *fakeStore) ListContainers(ctx context.Context, opts store.ListOptions) ([]store.ContainerRecord, error) {
	if opts.ServiceIDOrName.ID == "" && opts.ServiceIDOrName.Name == "" {
		return nil, nil // the controller's own-machine listing
	}
	if s.services != nil {
		return s.services(ctx, opts)
	}
	return []store.ContainerRecord{siblingRecord(newCtr("sib", "web"))}, nil
}

func siblingRecord(ctr api.ServiceContainer) store.ContainerRecord {
	return store.ContainerRecord{Container: ctr, MachineID: "machine-2", SyncStatus: store.SyncStatusSynced}
}

func siblings(ctrs ...api.ServiceContainer) func(context.Context, store.ListOptions) ([]store.ContainerRecord, error) {
	return func(_ context.Context, opts store.ListOptions) ([]store.ContainerRecord, error) {
		var recs []store.ContainerRecord
		for _, c := range ctrs {
			if c.ServiceID() == opts.ServiceIDOrName.ID {
				recs = append(recs, siblingRecord(c))
			}
		}
		return recs, nil
	}
}

func (s *fakeStore) DeleteContainers(context.Context, store.DeleteOptions) error {
	return nil
}

func (s *fakeStore) CreateOrUpdateContainer(_ context.Context, ctr api.ServiceContainer, machineID string) error {
	if machineID != testMachineID {
		return fmt.Errorf("unexpected machine ID %q", machineID)
	}
	if s.writeErr != nil {
		if err := s.writeErr(ctr); err != nil {
			return err
		}
	}
	b, err := json.Marshal(ctr)
	if err != nil {
		return err
	}
	// Decode like DNS and the Caddy controller do when they read the store.
	var decoded api.ServiceContainer
	if err = json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	healthy := decoded.Container.Healthy()

	s.mu.Lock()
	s.writes = append(s.writes, storeWrite{ID: ctr.ID, JSON: b, Healthy: healthy, Passed: ctr})
	s.mu.Unlock()
	if s.log != nil {
		s.log.add(fmt.Sprintf("write %s healthy=%t", ctr.ID, healthy))
	}
	return nil
}

func (s *fakeStore) allWrites() []storeWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storeWrite(nil), s.writes...)
}

// latest returns the last write for the container ID.
func (s *fakeStore) latest(t *testing.T, id string) storeWrite {
	t.Helper()
	ws := s.allWrites()
	for i := len(ws) - 1; i >= 0; i-- {
		if ws[i].ID == id {
			return ws[i]
		}
	}
	t.Fatalf("no store write for container %s", id)
	return storeWrite{}
}

// fakeDocker implements drainDocker and serviceContainerLister over in-memory containers.
type fakeDocker struct {
	log        *eventLog
	mu         sync.Mutex
	containers map[string]api.ServiceContainer
	stopErr    error
	stopCalls  int
	// listGate, when set, blocks the sync's ListServiceContainers (All=true) until it's closed (listStarted is closed
	// when it blocks).
	listGate    chan struct{}
	listStarted chan struct{}
	// syncListHook, when set, runs at the start of the sync's ListServiceContainers (All=true).
	syncListHook func(ctx context.Context) error
	// inspectHook, when set, runs at the start of InspectServiceContainer.
	inspectHook func(ctx context.Context, id string) error
}

func newFakeDocker(log *eventLog, ctrs ...api.ServiceContainer) *fakeDocker {
	d := &fakeDocker{log: log, containers: make(map[string]api.ServiceContainer)}
	for _, c := range ctrs {
		d.containers[c.ID] = c
	}
	return d
}

// clone returns a deep copy, like a fresh Docker inspect.
func clone(t testing.TB, ctr api.ServiceContainer) api.ServiceContainer {
	b, err := json.Marshal(ctr)
	if err != nil {
		t.Fatal(err)
	}
	var c api.ServiceContainer
	if err = json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (d *fakeDocker) InspectServiceContainer(ctx context.Context, id string) (api.ServiceContainer, error) {
	if d.inspectHook != nil {
		if err := d.inspectHook(ctx, id); err != nil {
			return api.ServiceContainer{}, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[id]
	if !ok {
		// Resolve a name, like Docker.
		for _, cc := range d.containers {
			if strings.TrimPrefix(cc.Name, "/") == id {
				c, ok = cc, true
				break
			}
		}
	}
	if !ok {
		return api.ServiceContainer{}, errdefs.ErrNotFound.WithMessage("no such container: " + id)
	}
	b, _ := json.Marshal(c)
	var out api.ServiceContainer
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func (d *fakeDocker) ContainerStop(_ context.Context, id string, opts container.StopOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopCalls++
	timeout := "nil"
	if opts.Timeout != nil {
		timeout = fmt.Sprint(*opts.Timeout)
	}
	if d.log != nil {
		d.log.add(fmt.Sprintf("stop %s timeout=%s", id, timeout))
	}
	if d.stopErr != nil {
		return d.stopErr
	}
	c, ok := d.containers[id]
	if !ok {
		return errdefs.ErrNotFound.WithMessage("no such container: " + id)
	}
	base := *c.ContainerJSONBase
	st := *base.State
	st.Running = false
	st.Status = container.StateExited
	base.State = &st
	c.ContainerJSONBase = &base
	d.containers[id] = c
	return nil
}

func (d *fakeDocker) stops() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopCalls
}

func (d *fakeDocker) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, id)
}

func (d *fakeDocker) ListServiceContainers(
	ctx context.Context, serviceNameOrID string, opts container.ListOptions,
) (ListServiceContainersResult, error) {
	if opts.All && d.listGate != nil {
		close(d.listStarted)
		<-d.listGate
	}
	if opts.All && d.syncListHook != nil {
		if err := d.syncListHook(ctx); err != nil {
			return ListServiceContainersResult{}, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var res ListServiceContainersResult
	for _, c := range d.containers {
		if serviceNameOrID != "" && c.ServiceID() != serviceNameOrID && c.ServiceName() != serviceNameOrID {
			continue
		}
		if !opts.All && !c.State.Running {
			continue
		}
		if c.IsHook() {
			res.HookContainers = append(res.HookContainers, c)
		} else {
			res.Containers = append(res.Containers, c)
		}
	}
	return res, nil
}

type ctrOpt func(*api.ServiceContainer)

func withHealth(status container.HealthStatus) ctrOpt {
	return func(c *api.ServiceContainer) {
		c.Config.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "true"}}
		c.State.Health = &container.Health{
			Status:        status,
			FailingStreak: 0,
			Log:           []*container.HealthcheckResult{{ExitCode: 0, Output: "ok"}},
		}
	}
}

func stopped() ctrOpt {
	return func(c *api.ServiceContainer) {
		c.State.Running = false
		c.State.Status = container.StateExited
	}
}

func hook() ctrOpt {
	return func(c *api.ServiceContainer) {
		c.Config.Labels[api.LabelHook] = api.LabelHookPreDeploy
		c.Config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
	}
}

func newCtr(id, service string, opts ...ctrOpt) api.ServiceContainer {
	c := api.ServiceContainer{
		Container: api.Container{
			InspectResponse: container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:   id,
					Name: "/" + service + "-" + id,
					State: &container.State{
						Status:  container.StateRunning,
						Running: true,
					},
				},
				Config: &container.Config{
					Labels: map[string]string{
						api.LabelManaged:     "",
						api.LabelServiceID:   service + "-id",
						api.LabelServiceName: service,
					},
				},
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						NetworkName: {IPAddress: "10.210.0.10"},
					},
				},
			},
		},
		ServiceSpec: api.ServiceSpec{Name: service},
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func testMachineIDFn() string { return testMachineID }

// newTestDrainer returns an enabled drainer whose wait runs hook (if set) instead of sleeping.
func newTestDrainer(
	docker drainDocker, st drainStore, log *eventLog, hook func(ctx context.Context) error,
) *Drainer {
	d := newDrainer(3*time.Second, docker, st, testMachineIDFn)
	d.wait = func(ctx context.Context, dur time.Duration) error {
		if log != nil {
			log.add(fmt.Sprintf("wait %s", dur))
		}
		if hook != nil {
			return hook(ctx)
		}
		return nil
	}
	return d
}

func intPtr(i int) *int { return &i }

func TestParseStopDrain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{value: "", want: 0},
		{value: "0", want: 0},
		{value: "0s", want: 0},
		{value: "3s", want: 3 * time.Second},
		{value: "1500ms", want: 1500 * time.Millisecond},
		{value: "30s", want: 30 * time.Second},
		{value: "30001ms", wantErr: true},
		{value: "31s", wantErr: true},
		{value: "3m", wantErr: true},
		{value: "3", wantErr: true},
		{value: "-1s", wantErr: true},
		{value: " 3s", wantErr: true},
		{value: "abc", wantErr: true},
	} {
		got, err := ParseStopDrain(tc.value)
		if tc.wantErr {
			assert.Error(t, err, "value %q", tc.value)
			assert.Zero(t, got, "value %q", tc.value)
			continue
		}
		require.NoError(t, err, "value %q", tc.value)
		assert.Equal(t, tc.want, got, "value %q", tc.value)
	}

	assert.False(t, (*Drainer)(nil).Enabled())
	assert.False(t, newDrainer(0, nil, nil, testMachineIDFn).Enabled())
	assert.True(t, newDrainer(time.Millisecond, nil, nil, testMachineIDFn).Enabled())
}

func TestCaddyServiceNameMatchesCaddyconfig(t *testing.T) {
	t.Parallel()
	assert.Equal(t, caddyconfig.CaddyServiceName, caddyServiceName)
}

// fakeDockerAPI is a fake Docker Engine HTTP API that records requests.
type fakeDockerAPI struct {
	mu       sync.Mutex
	requests []string
	// ids maps a container name or short ID to its full ID for inspect requests.
	ids map[string]string
}

func (f *fakeDockerAPI) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.47/containers/"), "/json")
		full, ok := f.ids[ref]
		if !ok {
			http.Error(w, `{"message":"No such container: `+ref+`"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"Id":%q,"Name":"/%s","State":{"Running":true}}`, full, ref)
	case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/stop") ||
		strings.HasSuffix(r.URL.Path, "/start")):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected request", http.StatusTeapot)
	}
}

func (f *fakeDockerAPI) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func newFakeDockerAPIClient(t *testing.T) (*client.Client, *fakeDockerAPI) {
	t.Helper()
	f := &fakeDockerAPI{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")),
		client.WithHTTPClient(srv.Client()),
		client.WithVersion("1.47"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli, f
}

func stopRequest(t *testing.T, id string, timeout *int) *pb.StopContainerRequest {
	t.Helper()
	opts, err := json.Marshal(container.StopOptions{Timeout: timeout})
	require.NoError(t, err)
	return &pb.StopContainerRequest{Id: id, Options: opts}
}

// Setting off: StopContainer makes exactly the one Docker stop call it made before, and nothing is written to the
// store by the stop path.
func TestServerStopContainer_DrainOff(t *testing.T) {
	t.Parallel()

	for name, drainer := range map[string]func(st *fakeStore) *Drainer{
		"nil drainer":      func(*fakeStore) *Drainer { return nil },
		"disabled drainer": func(st *fakeStore) *Drainer { return newDrainer(0, newFakeDocker(nil), st, testMachineIDFn) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cli, fakeAPI := newFakeDockerAPIClient(t)
			st := &fakeStore{}
			s := &Server{client: cli, drain: drainer(st)}

			_, err := s.StopContainer(context.Background(), stopRequest(t, "abc", intPtr(15)))
			require.NoError(t, err)

			assert.Equal(t, []string{"POST /v1.47/containers/abc/stop?t=15"}, fakeAPI.list())
			assert.Empty(t, st.allWrites())
		})
	}
}

// Setting off: a sync passes the very container objects from Docker to the store, so the bytes written are the same
// as without the drain code. With the setting on, a container that isn't draining is passed unchanged too.
func TestControllerSync_NotDrainingWritesUnchanged(t *testing.T) {
	t.Parallel()

	for name, mk := range map[string]func(dd drainDocker, st *fakeStore) *Drainer{
		"nil drainer":                 func(drainDocker, *fakeStore) *Drainer { return nil },
		"disabled drainer":            func(dd drainDocker, st *fakeStore) *Drainer { return newDrainer(0, dd, st, testMachineIDFn) },
		"enabled drainer, none drain": func(dd drainDocker, st *fakeStore) *Drainer { return newTestDrainer(dd, st, nil, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker(nil,
				newCtr("a", "web", withHealth(container.Healthy)),
				newCtr("b", "web"),
				newCtr("h", "web", hook()),
			)
			st := &fakeStore{}
			c := &Controller{machineID: testMachineID, store: st, lister: docker, drain: mk(docker, st)}

			require.NoError(t, c.syncContainersToStore(context.Background()))

			ws := st.allWrites()
			require.Len(t, ws, 3)
			for _, w := range ws {
				orig := docker.containers[w.ID]
				// The same object (shared pointers), not a copy.
				assert.Same(t, orig.ContainerJSONBase, w.Passed.ContainerJSONBase, w.ID)
				assert.Same(t, orig.State, w.Passed.State, w.ID)
				want, err := json.Marshal(orig)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(w.JSON), w.ID)
				assert.True(t, w.Healthy, w.ID)
			}
		})
	}
}

// With the setting on: the store shows the container as not healthy before the wait starts and before ContainerStop;
// then the stop runs with the caller's options and the true state is republished.
func TestDrainStop_PublishBeforeWait(t *testing.T) {
	t.Parallel()

	for name, opt := range map[string]ctrOpt{
		"with healthcheck":    withHealth(container.Healthy),
		"without healthcheck": func(*api.ServiceContainer) {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			orig := newCtr("x", "web", opt)
			docker := newFakeDocker(log, orig)
			st := &fakeStore{log: log}

			var d *Drainer
			d = newTestDrainer(docker, st, log, func(context.Context) error {
				// At the start of the wait the mark is confirmed in the store and nothing has been stopped yet.
				w := st.latest(t, "x")
				assert.False(t, w.Healthy, "store must show the container as not healthy before the wait")
				assert.Equal(t, 0, docker.stops(), "ContainerStop must not be called before the wait")
				assert.True(t, d.IsDraining("x"))

				var stored api.ServiceContainer
				require.NoError(t, json.Unmarshal(w.JSON, &stored))
				assert.True(t, stored.State.Running, "the override must keep State.Running")
				require.NotNil(t, stored.State.Health)
				assert.Equal(t, container.Unhealthy, stored.State.Health.Status)
				return nil
			})

			require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{Timeout: intPtr(20)}))

			assert.Equal(t, []string{
				"write x healthy=false",
				"wait 3s",
				"stop x timeout=20",
				"write x healthy=false", // republished true state: stopped
			}, log.list())
			last := st.latest(t, "x")
			var stored api.ServiceContainer
			require.NoError(t, json.Unmarshal(last.JSON, &stored))
			assert.False(t, stored.State.Running, "the last write is the true (stopped) state")
			if orig.State.Health != nil {
				assert.Equal(t, container.Healthy, stored.State.Health.Status, "true state carries the real health")
			} else {
				assert.Nil(t, stored.State.Health, "true state of a container without a healthcheck has no health")
			}
			assert.Equal(t, 0, d.drainingCount(), "registry must be empty after the stop")
		})
	}
}

// A sync during the wait (event-driven or periodic, same code) keeps the override for the draining container and
// writes the others unchanged.
func TestDrainStop_SyncDuringWaitKeepsOverride(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	docker := newFakeDocker(nil, newCtr("x", "web", withHealth(container.Healthy)), newCtr("y", "web"))
	st := &fakeStore{log: log}
	var d *Drainer
	var c *Controller
	d = newTestDrainer(docker, st, log, func(ctx context.Context) error {
		// Two syncs during the wait, like a health_status event sync followed by the 30 s periodic sync.
		for i := 0; i < 2; i++ {
			before := len(st.allWrites())
			require.NoError(t, c.syncContainersToStore(ctx))
			ws := st.allWrites()[before:]
			require.Len(t, ws, 2)
			for _, w := range ws {
				switch w.ID {
				case "x":
					assert.False(t, w.Healthy, "sync during the drain must keep x not healthy")
					assert.NotSame(t, docker.containers["x"].State, w.Passed.State, "override applies to a copy")
				case "y":
					assert.True(t, w.Healthy)
					assert.Same(t, docker.containers["y"].State, w.Passed.State, "y is passed unchanged")
				}
			}
		}
		return nil
	})
	c = &Controller{machineID: testMachineID, store: st, lister: docker, drain: d}

	require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
	assert.Equal(t, 0, d.drainingCount())
	// Docker state was never changed by the override.
	assert.Equal(t, container.Healthy, docker.containers["x"].State.Health.Status)
}

// A sync that read Docker before the mark and is still writing must not overwrite the mark: the mark waits for the
// in-flight sync, so the store shows not healthy when the wait starts.
func TestDrainStop_MarkSerialisedWithInFlightSync(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	docker := newFakeDocker(nil, newCtr("x", "web", withHealth(container.Healthy)))
	docker.listGate = make(chan struct{})
	docker.listStarted = make(chan struct{})
	st := &fakeStore{log: log}
	waitStarted := make(chan struct{})
	d := newTestDrainer(docker, st, log, func(context.Context) error {
		close(waitStarted)
		return nil
	})
	c := &Controller{machineID: testMachineID, store: st, lister: docker, drain: d}

	syncDone := make(chan error, 1)
	go func() { syncDone <- c.syncContainersToStore(context.Background()) }()
	<-docker.listStarted // the sync holds the write lock and is reading Docker (x healthy)

	stopDone := make(chan error, 1)
	go func() { stopDone <- d.StopContainer(context.Background(), "x", container.StopOptions{}) }()

	select {
	case <-waitStarted:
		t.Fatal("drain wait started while a sync was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	assert.Empty(t, st.allWrites(), "mark must wait for the in-flight sync")

	close(docker.listGate)
	require.NoError(t, <-syncDone)
	require.NoError(t, <-stopDone)

	assert.Equal(t, []string{
		"write x healthy=true",  // the in-flight sync, before the mark
		"write x healthy=false", // the mark
		"wait 3s",
		"write x healthy=false", // true state after the stop
	}, log.list())
}

// Cancelling the request during the wait: no stop, the mark is cleared and the true (healthy) state republished. Via
// the gRPC server, the error is a Canceled status.
func TestDrainStop_CancelDuringWait(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	docker := newFakeDocker(log, newCtr("x", "web"))
	st := &fakeStore{log: log}
	ctx, cancel := context.WithCancel(context.Background())
	d := newTestDrainer(docker, st, log, func(wctx context.Context) error {
		cancel()
		<-wctx.Done()
		return wctx.Err()
	})
	s := &Server{drain: d}

	_, err := s.StopContainer(ctx, stopRequest(t, "x", nil))
	require.Error(t, err)
	assert.Equal(t, codes.Canceled, status.Code(err))
	assert.Equal(t, 0, docker.stops(), "a cancelled request must not stop the container")
	assert.Equal(t, 0, d.drainingCount())
	assert.True(t, st.latest(t, "x").Healthy, "true state (running, healthy) must be republished")
	assert.Equal(t, []string{"write x healthy=false", "wait 3s", "write x healthy=true"}, log.list())
}

// Cancelling while the mark waits for the write lock: nothing is written or stopped and the registry stays empty.
func TestDrainStop_CancelWhileWaitingForWriteLock(t *testing.T) {
	t.Parallel()

	docker := newFakeDocker(nil, newCtr("x", "web"))
	st := &fakeStore{}
	d := newTestDrainer(docker, st, nil, nil)
	release, err := d.lockWrites(context.Background())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = d.StopContainer(ctx, "x", container.StopOptions{})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 0, docker.stops())
	assert.Empty(t, st.allWrites())
	assert.Equal(t, 0, d.drainingCount())
}

// A failed stop of a container that still runs: the error is returned, the mark is cleared and the true (healthy)
// state is republished.
func TestDrainStop_FailedStopClears(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	docker := newFakeDocker(log, newCtr("x", "web", withHealth(container.Healthy)))
	docker.stopErr = errors.New("boom")
	st := &fakeStore{log: log}
	d := newTestDrainer(docker, st, log, nil)

	err := d.StopContainer(context.Background(), "x", container.StopOptions{})
	assert.EqualError(t, err, "boom")
	assert.Equal(t, 0, d.drainingCount())
	assert.True(t, st.latest(t, "x").Healthy, "still-running container must be republished healthy")
	assert.Equal(t, []string{
		"write x healthy=false", "wait 3s", "stop x timeout=nil", "write x healthy=true",
	}, log.list())
}

// A mark that can't be written is never waited on: the container is stopped at once, the registry is empty and the
// true state is republished (a failed write may still have been applied).
func TestDrainStop_MarkWriteFailsStopsWithoutWait(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	docker := newFakeDocker(log, newCtr("x", "web"))
	st := &fakeStore{log: log, writeErr: func(ctr api.ServiceContainer) error {
		if ctr.State.Health != nil && ctr.State.Health.Status == container.Unhealthy {
			return errors.New("corrosion unavailable")
		}
		return nil
	}}
	d := newTestDrainer(docker, st, log, nil)

	require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
	assert.Equal(t, []string{"write x healthy=true", "stop x timeout=nil"}, log.list())
	assert.Equal(t, 0, d.drainingCount())
}

// Containers that don't need a drain go straight to stop: no store write, no wait.
func TestDrainStop_Skipped(t *testing.T) {
	t.Parallel()

	for name, ctr := range map[string]api.ServiceContainer{
		"pre-deploy hook":    newCtr("x", "web", hook()),
		"caddy":              newCtr("x", "caddy"),
		"not running":        newCtr("x", "web", stopped()),
		"unhealthy":          newCtr("x", "web", withHealth(container.Unhealthy)),
		"health starting":    newCtr("x", "web", withHealth(container.Starting)),
		"not found in local": newCtr("other", "web"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			docker := newFakeDocker(log, ctr)
			// A healthy sibling of every service, so only the skip rule under test can prevent the drain.
			st := &fakeStore{log: log, services: siblings(newCtr("sib-web", "web"), newCtr("sib-caddy", "caddy"))}
			d := newTestDrainer(docker, st, log, nil)

			err := d.StopContainer(context.Background(), "x", container.StopOptions{Timeout: intPtr(5)})
			if ctr.ID != "x" {
				assert.True(t, errdefs.IsNotFound(err), "stop of an unknown container returns NotFound as before")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []string{"stop x timeout=5"}, log.list())
			assert.Equal(t, 0, d.drainingCount())
		})
	}
}

// fullID is a 64-hex container ID, as Docker returns and the registry stores.
var fullID = strings.Repeat("ab", 32)

// RemoveContainer and RemoveServiceContainer drop the container from the registry, also when the client sends a name
// or short ID (resolved to the full ID before the removal), and a drain that ends after the container was removed
// leaves nothing behind.
func TestDrain_RegistryEmptyAfterRemoval(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		ref          string
		wantRequests []string
	}{
		"full ID": {ref: fullID, wantRequests: []string{"DELETE /v1.47/containers/" + fullID + "?"}},
		"name": {ref: "web-1", wantRequests: []string{
			"GET /v1.47/containers/web-1/json?", "DELETE /v1.47/containers/web-1?",
		}},
		"short ID": {ref: fullID[:12], wantRequests: []string{
			"GET /v1.47/containers/" + fullID[:12] + "/json?", "DELETE /v1.47/containers/" + fullID[:12] + "?",
		}},
	} {
		t.Run("RemoveContainer by "+name, func(t *testing.T) {
			t.Parallel()
			cli, fakeAPI := newFakeDockerAPIClient(t)
			fakeAPI.ids = map[string]string{"web-1": fullID, fullID[:12]: fullID}
			d := newTestDrainer(newFakeDocker(nil), &fakeStore{}, nil, nil)
			d.add(fullID)
			s := &Server{client: cli, drain: d}

			_, err := s.RemoveContainer(context.Background(),
				&pb.RemoveContainerRequest{Id: tc.ref, Options: []byte("{}")})
			require.NoError(t, err)
			assert.Equal(t, tc.wantRequests, fakeAPI.list())
			assert.Equal(t, 0, d.drainingCount())
		})
	}

	t.Run("RemoveContainer with an empty registry doesn't inspect", func(t *testing.T) {
		t.Parallel()
		cli, fakeAPI := newFakeDockerAPIClient(t)
		s := &Server{client: cli, drain: newTestDrainer(newFakeDocker(nil), &fakeStore{}, nil, nil)}
		_, err := s.RemoveContainer(context.Background(), &pb.RemoveContainerRequest{Id: "web-1", Options: []byte("{}")})
		require.NoError(t, err)
		assert.Equal(t, []string{"DELETE /v1.47/containers/web-1?"}, fakeAPI.list())
	})

	t.Run("removed during the wait", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		docker := newFakeDocker(log, newCtr("x", "web"))
		st := &fakeStore{log: log}
		var d *Drainer
		d = newTestDrainer(docker, st, log, func(context.Context) error {
			docker.remove("x")
			d.Forget("x")
			return nil
		})

		err := d.StopContainer(context.Background(), "x", container.StopOptions{})
		assert.True(t, errdefs.IsNotFound(err))
		assert.Equal(t, 0, d.drainingCount())
		// Only the mark was written; nothing republished for a removed container.
		assert.Equal(t, []string{"write x healthy=false", "wait 3s", "stop x timeout=nil"}, log.list())
	})
}

// The drain only runs when another healthy container of the service (any machine, by the DNS rule) can take the
// traffic. Otherwise, or when the store can't answer, the container is stopped at once: no store write, no wait.
func TestDrainStop_HealthySiblingRule(t *testing.T) {
	t.Parallel()

	noIP := newCtr("sib", "web")
	noIP.NetworkSettings.Networks = nil
	otherService := newCtr("sib", "api")

	for name, tc := range map[string]struct {
		services func(context.Context, store.ListOptions) ([]store.ContainerRecord, error)
		local    []string // containers this daemon is already draining
		drain    bool
	}{
		"healthy sibling":                  {services: siblings(newCtr("sib", "web")), drain: true},
		"healthy sibling with healthcheck": {services: siblings(newCtr("sib", "web", withHealth(container.Healthy))), drain: true},
		"no other container":               {services: siblings(newCtr("x", "web")), drain: false},
		"no records":                       {services: siblings(), drain: false},
		"sibling unhealthy":                {services: siblings(newCtr("sib", "web", withHealth(container.Unhealthy))), drain: false},
		"sibling starting":                 {services: siblings(newCtr("sib", "web", withHealth(container.Starting))), drain: false},
		"sibling published as draining":    {services: siblings(withDrainOverride(newCtr("sib", "web"))), drain: false},
		"sibling stopped":                  {services: siblings(newCtr("sib", "web", stopped())), drain: false},
		"sibling is a hook":                {services: siblings(newCtr("sib", "web", hook())), drain: false},
		"sibling without uncloud IP":       {services: siblings(noIP), drain: false},
		"sibling of another service":       {services: siblings(otherService), drain: false},
		"sibling draining on this machine": {services: siblings(newCtr("sib", "web")), local: []string{"sib"}, drain: false},
		"one of two siblings left":         {services: siblings(newCtr("s1", "web", stopped()), newCtr("s2", "web")), drain: true},
		"store error": {services: func(context.Context, store.ListOptions) ([]store.ContainerRecord, error) {
			return nil, errors.New("corrosion unavailable")
		}, drain: false},
		"store too slow": {services: func(ctx context.Context, _ store.ListOptions) ([]store.ContainerRecord, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, drain: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			docker := newFakeDocker(log, newCtr("x", "web"))
			st := &fakeStore{log: log, services: tc.services}
			d := newTestDrainer(docker, st, log, nil)
			d.siblingTimeout = 50 * time.Millisecond
			for _, id := range tc.local {
				d.add(id)
			}

			start := time.Now()
			require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
			assert.Less(t, time.Since(start), 2*time.Second)
			if tc.drain {
				assert.Equal(t, []string{
					"write x healthy=false", "wait 3s", "stop x timeout=nil", "write x healthy=false",
				}, log.list())
			} else {
				assert.Equal(t, []string{"stop x timeout=nil"}, log.list(), "stop at once, no write, no wait")
			}
			assert.False(t, d.IsDraining("x"))
		})
	}

	t.Run("cancelled during the check", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		docker := newFakeDocker(log, newCtr("x", "web"))
		ctx, cancel := context.WithCancel(context.Background())
		st := &fakeStore{log: log, services: func(lctx context.Context, _ store.ListOptions) ([]store.ContainerRecord, error) {
			cancel()
			<-lctx.Done()
			return nil, lctx.Err()
		}}
		d := newTestDrainer(docker, st, log, nil)

		err := d.StopContainer(ctx, "x", container.StopOptions{})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, log.list(), "a cancelled request doesn't stop the container")
	})
}

// Daemon restart: the registry is in memory, so a new daemon (new Drainer) publishes a container that is still
// running as healthy on its first sync.
func TestDrain_DaemonRestartPublishesHealthy(t *testing.T) {
	t.Parallel()

	docker := newFakeDocker(nil, newCtr("x", "web", withHealth(container.Healthy)))
	st := &fakeStore{}

	// The old daemon marked x and was killed during the wait (the wait never returns).
	old := newTestDrainer(docker, st, nil, nil)
	marked, err := old.mark(context.Background(), "x")
	require.NoError(t, err)
	require.True(t, marked)
	require.False(t, st.latest(t, "x").Healthy)

	restarted := newTestDrainer(docker, st, nil, nil)
	c := &Controller{machineID: testMachineID, store: st, lister: docker, drain: restarted}
	require.NoError(t, c.syncContainersToStore(context.Background()))

	assert.True(t, st.latest(t, "x").Healthy, "a running container is published healthy after a daemon restart")
	assert.Equal(t, 0, restarted.drainingCount())
}

// Two stop requests drain the same container: cancelling one must not unmark the container for the other.
func TestDrainStop_ConcurrentDrainsOfOneContainer(t *testing.T) {
	t.Parallel()

	docker := newFakeDocker(nil, newCtr("x", "web"))
	st := &fakeStore{}
	inWait := make(chan struct{}, 2)
	release := make(chan struct{})
	d := newTestDrainer(docker, st, nil, func(ctx context.Context) error {
		inWait <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	done2 := make(chan error, 1)
	go func() { done1 <- d.StopContainer(ctx1, "x", container.StopOptions{}) }()
	go func() { done2 <- d.StopContainer(context.Background(), "x", container.StopOptions{}) }()
	<-inWait
	<-inWait

	cancel1()
	assert.ErrorIs(t, <-done1, context.Canceled)
	assert.True(t, d.IsDraining("x"), "the other drain is still in flight")
	assert.False(t, st.latest(t, "x").Healthy, "the container must stay not healthy for the other drain")

	close(release)
	require.NoError(t, <-done2)
	assert.Equal(t, 0, d.drainingCount())
	assert.Equal(t, 1, docker.stops())
}

func TestWithDrainOverride_CopyOnly(t *testing.T) {
	t.Parallel()

	for name, ctr := range map[string]api.ServiceContainer{
		"with healthcheck":    newCtr("x", "web", withHealth(container.Healthy)),
		"without healthcheck": newCtr("x", "web"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before, err := json.Marshal(ctr)
			require.NoError(t, err)
			require.True(t, ctr.Container.Healthy())

			got := withDrainOverride(ctr)

			after, err := json.Marshal(ctr)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "the original must not change")
			assert.False(t, got.Container.Healthy())
			assert.True(t, got.State.Running)
			assert.Equal(t, container.Unhealthy, got.State.Health.Status)

			decoded := clone(t, got)
			assert.False(t, decoded.Container.Healthy(), "not healthy after a store JSON round trip")
		})
	}
}

// A stuck store (here: a sync holding the write lock) must not hang a stop: after storeTimeout the container is
// stopped without a drain, and a clear that can't get the lock still empties the registry.
func TestDrainStop_StuckStoreFallsBackToStop(t *testing.T) {
	t.Parallel()

	t.Run("mark", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		docker := newFakeDocker(log, newCtr("x", "web"))
		st := &fakeStore{log: log}
		d := newTestDrainer(docker, st, log, nil)
		d.storeTimeout = 50 * time.Millisecond
		release, err := d.lockWrites(context.Background())
		require.NoError(t, err)
		defer release()

		start := time.Now()
		require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
		assert.Less(t, time.Since(start), 2*time.Second)
		assert.Equal(t, []string{"stop x timeout=nil"}, log.list(), "no write, no wait, one stop")
		assert.Equal(t, 0, d.drainingCount())
	})

	t.Run("clear", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		docker := newFakeDocker(log, newCtr("x", "web"))
		st := &fakeStore{log: log}
		var d *Drainer
		var release func()
		d = newTestDrainer(docker, st, log, func(ctx context.Context) error {
			var err error
			release, err = d.lockWrites(ctx) // a sync gets stuck holding the lock during the wait
			return err
		})
		d.storeTimeout = 50 * time.Millisecond

		require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
		release()
		assert.Equal(t, []string{"write x healthy=false", "wait 3s", "stop x timeout=nil"}, log.list())
		assert.Equal(t, 0, d.drainingCount(), "the mark is dropped even when the true state can't be republished")
	})
}

func (d *fakeDocker) setRunning(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.containers[id]
	base := *c.ContainerJSONBase
	st := *base.State
	st.Running = true
	st.Status = container.StateRunning
	base.State = &st
	c.ContainerJSONBase = &base
	d.containers[id] = c
}

// A sync stuck holding the write lock when a drain ends (cancelled request, or failed stop, container still
// running): the clear can't take the lock, so the container is kept for a retry. Once the sync is released, or
// times out (syncTimeout) and the next sync runs, the container is published healthy again and the retry set is empty.
func TestDrain_StuckSyncRestoresRunningContainer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		cancel    bool // cancel the request during the wait; otherwise the stop fails
		syncTimes bool // the stuck sync is never released and must time out
	}{
		{name: "cancel, sync released", cancel: true},
		{name: "failed stop, sync released"},
		{name: "cancel, sync times out", cancel: true, syncTimes: true},
		{name: "failed stop, sync times out", syncTimes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker(nil, newCtr("x", "web", withHealth(container.Healthy)))
			if !tc.cancel {
				docker.stopErr = errors.New("boom")
			}
			st := &fakeStore{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var c *Controller
			var d *Drainer
			gate := make(chan struct{})
			syncStarted := make(chan struct{})
			syncDone := make(chan error, 1)
			stuck := true
			docker.syncListHook = func(lctx context.Context) error {
				if !stuck {
					return nil
				}
				close(syncStarted)
				if tc.syncTimes {
					<-lctx.Done() // a hung Docker call that only ends with the sync deadline
					return lctx.Err()
				}
				<-gate
				return nil
			}
			d = newTestDrainer(docker, st, nil, func(wctx context.Context) error {
				go func() { syncDone <- c.syncContainersToStore(context.Background()) }()
				<-syncStarted // the sync holds the write lock now
				if tc.cancel {
					cancel()
					<-wctx.Done()
					return wctx.Err()
				}
				return nil
			})
			d.storeTimeout = 50 * time.Millisecond
			d.syncTimeout = 300 * time.Millisecond
			c = &Controller{machineID: testMachineID, store: st, lister: docker, drain: d}

			err := d.StopContainer(ctx, "x", container.StopOptions{})
			require.Error(t, err)
			assert.Equal(t, 0, d.drainingCount())
			assert.Equal(t, 1, d.pendingCount(), "the failed restore is kept for a retry")
			assert.False(t, st.latest(t, "x").Healthy, "still marked while the sync is stuck")

			if tc.syncTimes {
				assert.ErrorIs(t, <-syncDone, context.DeadlineExceeded, "the stuck sync is cut at syncTimeout")
			} else {
				close(gate)
				require.NoError(t, <-syncDone)
				assert.True(t, st.latest(t, "x").Healthy, "the released sync writes the true state")
			}

			stuck = false
			require.NoError(t, c.syncContainersToStore(context.Background()))
			assert.True(t, st.latest(t, "x").Healthy, "running container published healthy again")
			assert.Equal(t, 0, d.pendingCount())
		})
	}
}

// A republish that fails is retried at the next clear (of any container) and at the start of the next sync.
func TestDrain_FailedRestoreRetried(t *testing.T) {
	t.Parallel()

	for _, by := range []string{"next clear", "next sync"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			docker := newFakeDocker(log, newCtr("x", "web"), newCtr("y", "web"))
			failOnce := true
			st := &fakeStore{log: log, writeErr: func(ctr api.ServiceContainer) error {
				if ctr.ID == "x" && ctr.State.Health == nil && failOnce {
					failOnce = false
					return errors.New("corrosion unavailable")
				}
				return nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			var d *Drainer
			d = newTestDrainer(docker, st, log, func(wctx context.Context) error {
				cancel()
				<-wctx.Done()
				return wctx.Err()
			})

			assert.ErrorIs(t, d.StopContainer(ctx, "x", container.StopOptions{}), context.Canceled)
			assert.Equal(t, 1, d.pendingCount())
			assert.False(t, st.latest(t, "x").Healthy, "the restore failed, x is still published as draining")

			if by == "next clear" {
				d.wait = func(context.Context, time.Duration) error { return nil }
				require.NoError(t, d.StopContainer(context.Background(), "y", container.StopOptions{}))
			} else {
				// Only the retry can write x here: the sync's lister returns no containers.
				c := &Controller{machineID: testMachineID, store: st, lister: newFakeDocker(nil), drain: d}
				require.NoError(t, c.syncContainersToStore(context.Background()))
			}
			assert.True(t, st.latest(t, "x").Healthy)
			assert.Equal(t, 0, d.pendingCount())
		})
	}
}

// With the drain on, a sync runs with a deadline (syncTimeout); with it off, the context is passed unchanged.
func TestControllerSync_DeadlineOnlyWhenDrainOn(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		drain        func(dd drainDocker, st *fakeStore) *Drainer
		wantDeadline bool
	}{
		"nil drainer":      {drain: func(drainDocker, *fakeStore) *Drainer { return nil }},
		"disabled drainer": {drain: func(dd drainDocker, st *fakeStore) *Drainer { return newDrainer(0, dd, st, testMachineIDFn) }},
		"enabled drainer": {
			drain:        func(dd drainDocker, st *fakeStore) *Drainer { return newTestDrainer(dd, st, nil, nil) },
			wantDeadline: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker(nil, newCtr("x", "web"))
			var deadline time.Time
			var hasDeadline bool
			docker.syncListHook = func(ctx context.Context) error {
				deadline, hasDeadline = ctx.Deadline()
				return nil
			}
			st := &fakeStore{}
			c := &Controller{machineID: testMachineID, store: st, lister: docker, drain: tc.drain(docker, st)}
			require.NoError(t, c.syncContainersToStore(context.Background()))
			assert.Equal(t, tc.wantDeadline, hasDeadline)
			if tc.wantDeadline {
				assert.WithinDuration(t, time.Now().Add(SyncInterval), deadline, 2*time.Second)
			}
		})
	}
}

// Start-first replace: the new container is healthy in local Docker but its record isn't in the store yet. The stop
// waits up to siblingWait for the store: catches up → drain; never → stop without a drain (no mark). Without a
// healthy local sibling, the stop doesn't wait at all.
func TestDrainStop_StoreLaggingBehindLocalSibling(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		local       []api.ServiceContainer
		catchUpAt   int // the store shows the sibling from this ListContainers call on (0 = never)
		drain       bool
		maxDuration time.Duration
		maxCalls    int
	}{
		"store catches up": {
			local: []api.ServiceContainer{newCtr("new", "web")}, catchUpAt: 4, drain: true, maxDuration: time.Second,
		},
		"store never catches up": {
			local: []api.ServiceContainer{newCtr("new", "web")}, drain: false, maxDuration: time.Second,
		},
		"no local sibling": {drain: false, maxDuration: 50 * time.Millisecond, maxCalls: 1},
		"local sibling still starting": {
			local: []api.ServiceContainer{newCtr("new", "web", withHealth(container.Starting))},
			drain: false, maxDuration: 50 * time.Millisecond, maxCalls: 1,
		},
		"local sibling stopped": {
			local: []api.ServiceContainer{newCtr("new", "web", stopped())},
			drain: false, maxDuration: 50 * time.Millisecond, maxCalls: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			docker := newFakeDocker(log, append([]api.ServiceContainer{newCtr("x", "web")}, tc.local...)...)
			var mu sync.Mutex
			calls := 0
			st := &fakeStore{log: log, services: func(_ context.Context, opts store.ListOptions) ([]store.ContainerRecord, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if tc.catchUpAt > 0 && calls >= tc.catchUpAt {
					return []store.ContainerRecord{siblingRecord(newCtr("new", "web"))}, nil
				}
				return nil, nil
			}}
			d := newTestDrainer(docker, st, log, nil)
			d.siblingWait = 200 * time.Millisecond
			d.siblingPoll = 10 * time.Millisecond

			start := time.Now()
			require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
			elapsed := time.Since(start)
			assert.Less(t, elapsed, tc.maxDuration)
			if tc.drain {
				assert.Equal(t, []string{
					"write x healthy=false", "wait 3s", "stop x timeout=nil", "write x healthy=false",
				}, log.list())
			} else {
				assert.Equal(t, []string{"stop x timeout=nil"}, log.list(), "no mark, no wait")
			}
			if name == "store never catches up" {
				assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond, "waited siblingWait for the store")
			}
			if tc.maxCalls > 0 {
				mu.Lock()
				assert.Equal(t, tc.maxCalls, calls, "no polling without a local sibling")
				mu.Unlock()
			}
		})
	}
}

// The first inspect is bounded: if Docker doesn't answer, the container is stopped without a drain (by the ID sent).
func TestDrainStop_InitialInspectBounded(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	docker := newFakeDocker(log, newCtr("x", "web"))
	first := true
	docker.inspectHook = func(ctx context.Context, _ string) error {
		if first {
			first = false
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	st := &fakeStore{log: log}
	d := newTestDrainer(docker, st, log, nil)
	d.inspectTimeout = 50 * time.Millisecond

	start := time.Now()
	require.NoError(t, d.StopContainer(context.Background(), "x", container.StopOptions{}))
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, []string{"stop x timeout=nil"}, log.list())
}

// A stop sent by name stops the container by its resolved full ID, with or without a drain.
func TestDrainStop_StopsByFullID(t *testing.T) {
	t.Parallel()

	for name, ctr := range map[string]api.ServiceContainer{
		"drained":     newCtr("x", "web"),
		"not drained": newCtr("x", "web", withHealth(container.Unhealthy)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			docker := newFakeDocker(log, ctr)
			d := newTestDrainer(docker, &fakeStore{log: log}, log, nil)

			require.NoError(t, d.StopContainer(context.Background(), "web-x", container.StopOptions{}))
			assert.Contains(t, log.list(), "stop x timeout=nil")
			assert.NotContains(t, log.list(), "stop web-x timeout=nil")
		})
	}
}

// StartContainer doesn't touch the drain registry: a start during a pending stop leaves the mark (the stop still
// runs after the wait), and the CLI's stop-first rollback (stop completed, then start) gets the container published
// healthy by the sync that the start triggers.
func TestServerStartContainer_LeavesDrainToStop(t *testing.T) {
	t.Parallel()

	t.Run("start during a pending stop", func(t *testing.T) {
		t.Parallel()
		cli, fakeAPI := newFakeDockerAPIClient(t)
		log := &eventLog{}
		docker := newFakeDocker(log, newCtr(fullID, "web"))
		st := &fakeStore{log: log}
		var d *Drainer
		var s *Server
		d = newTestDrainer(docker, st, log, func(context.Context) error {
			_, err := s.StartContainer(context.Background(), &pb.StartContainerRequest{Id: "web-1"})
			require.NoError(t, err)
			assert.True(t, d.IsDraining(fullID), "a start must not clear the pending stop's mark")
			assert.False(t, st.latest(t, fullID).Healthy, "still published as draining")
			return nil
		})
		s = &Server{client: cli, drain: d}

		require.NoError(t, d.StopContainer(context.Background(), fullID, container.StopOptions{}))
		assert.Equal(t, []string{"POST /v1.47/containers/web-1/start?"}, fakeAPI.list(), "no inspect, no store access")
		assert.Equal(t, []string{
			"write " + fullID + " healthy=false", "wait 3s", "stop " + fullID + " timeout=nil",
			"write " + fullID + " healthy=false",
		}, log.list())
		assert.Equal(t, 0, d.drainingCount())
	})

	t.Run("stop-first rollback", func(t *testing.T) {
		t.Parallel()
		cli, _ := newFakeDockerAPIClient(t)
		docker := newFakeDocker(nil, newCtr(fullID, "web", withHealth(container.Healthy)))
		st := &fakeStore{}
		d := newTestDrainer(docker, st, nil, nil)
		s := &Server{client: cli, drain: d}
		c := &Controller{machineID: testMachineID, store: st, lister: docker, drain: d}

		require.NoError(t, d.StopContainer(context.Background(), fullID, container.StopOptions{}))
		assert.Equal(t, 0, d.drainingCount(), "the completed stop cleared its mark")
		assert.False(t, st.latest(t, fullID).Healthy, "true state: stopped")

		_, err := s.StartContainer(context.Background(), &pb.StartContainerRequest{Id: fullID})
		require.NoError(t, err)
		docker.setRunning(fullID)                                         // Docker started it
		require.NoError(t, c.syncContainersToStore(context.Background())) // the start event's sync
		assert.True(t, st.latest(t, fullID).Healthy, "rolled-back container published healthy")
	})
}
