package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/psviderski/uncloud/pkg/api"
)

const (
	// StopDrainEnvVar is the daemon environment variable that turns on drain before stop. The value is a Go duration
	// (e.g. "3s"). Unset, empty or "0" turns it off. It's read once when the daemon starts.
	StopDrainEnvVar = "UNCLOUD_STOP_DRAIN"

	// drainStoreTimeout bounds each store step of a drain: publishing the mark (waiting for the store write lock,
	// inspecting the container and writing it) and clearing it (the same, to republish the true state). If the store
	// is stuck, a stop with the drain on is delayed by at most this much before it falls back to a stop without a drain,
	// instead of hanging with the store. Clearing doesn't use the request context: the request may be cancelled.
	drainStoreTimeout = 10 * time.Second
	// drainSiblingTimeout bounds each read that looks for another healthy container of the service (the cluster
	// store, or the local Docker daemon). If it fails or times out, the container is stopped without a drain.
	drainSiblingTimeout = 2 * time.Second
	// drainSiblingWait is how long a stop waits for the cluster store to show a healthy sibling that the local Docker
	// daemon already reports (start-first replace: the CLI checks the new container's health in Docker, then stops
	// the old one at once, before the new container's record is published). If the store doesn't catch up in time,
	// the container is stopped without a drain: the last published-healthy container is never marked.
	drainSiblingWait = 2 * time.Second
	// drainSiblingPoll is the interval of the store reads while waiting for a sibling.
	drainSiblingPoll = 100 * time.Millisecond
	// drainInspectTimeout bounds the first inspect of the container to stop. On an error or timeout the container is
	// stopped without a drain.
	drainInspectTimeout = 2 * time.Second
	// MaxStopDrain is the largest accepted StopDrainEnvVar value. A larger value turns the drain off.
	MaxStopDrain = 30 * time.Second

	// caddyServiceName is the name of the caddy service. It must equal caddyconfig.CaddyServiceName (checked by a
	// test); caddyconfig can't be imported here because its tests import this package.
	caddyServiceName = "caddy"
)

// ParseStopDrain parses a StopDrainEnvVar value. Unset, empty or "0" returns 0 (off). A value that isn't a Go
// duration, is negative or is larger than MaxStopDrain is an error (the caller runs with the drain off).
func ParseStopDrain(value string) (time.Duration, error) {
	if value == "" || value == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q as a duration: %w", StopDrainEnvVar, value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s=%q must not be negative", StopDrainEnvVar, value)
	}
	if d > MaxStopDrain {
		return 0, fmt.Errorf("%s=%q must not be larger than %s", StopDrainEnvVar, value, MaxStopDrain)
	}
	return d, nil
}

// drainDocker is the subset of Docker operations the Drainer needs.
type drainDocker interface {
	InspectServiceContainer(ctx context.Context, nameOrID string) (api.ServiceContainer, error)
	ListServiceContainers(
		ctx context.Context, serviceNameOrID string, opts container.ListOptions,
	) (ListServiceContainersResult, error)
	ContainerStop(ctx context.Context, id string, opts container.StopOptions) error
}

// serviceDrainDocker implements drainDocker with the Docker service.
type serviceDrainDocker struct {
	*Service
}

func (d serviceDrainDocker) ContainerStop(ctx context.Context, id string, opts container.StopOptions) error {
	return d.Client.ContainerStop(ctx, id, opts)
}

// drainStore is the subset of the cluster store the Drainer needs: it writes this machine's container records and
// reads the records of a service on all machines.
type drainStore interface {
	CreateOrUpdateContainer(ctx context.Context, ctr api.ServiceContainer, machineID string) error
	ListContainers(ctx context.Context, opts store.ListOptions) ([]store.ContainerRecord, error)
}

// Drainer takes a service container out of service discovery before it is stopped (drain before stop).
//
// When StopContainer is called for a running, healthy service container and another healthy container of the same
// service can take its traffic, the Drainer:
//  1. adds the container to an in-memory drain registry and publishes its record to the cluster store as not healthy
//     (an override applied to a copy of the record, the Docker container itself is not changed). DNS and the Caddy
//     config generator on every machine only use healthy containers (api.Container.Healthy), so they drop it;
//  2. waits for the drain time once the store write has succeeded;
//  3. stops the container with the caller's options, then removes it from the registry and republishes its true state.
//
// A registry entry exists only while its stop request is in flight: every exit of StopContainer after a mark clears
// it. If the true state can't be republished at that point (store write lock busy, store or Docker error), the
// container is kept in a "needs republish" set that is retried at the start of every sync and at every later clear,
// until it succeeds or the container is gone. Starting a container does not touch the registry.
//
// While a container is in the registry, the Controller applies the same override on every sync of that container to
// the store (event-driven and periodic). All container record writes of this machine are serialised by the write lock,
// so a sync that read the Docker state before the mark can't overwrite the mark after it. With the drain on, a sync
// holds the lock for at most syncTimeout.
//
// The registry lives in memory only. If the daemon restarts, the registry is empty and the first sync after the start
// publishes the true state of every container: a container that is still running is published as healthy again. The
// stop request that was draining it fails with the daemon, so nothing stops it later.
//
// A disabled Drainer (drain time 0) or a nil *Drainer does nothing: StopContainer isn't routed through it and the
// Controller writes exactly what it wrote before.
type Drainer struct {
	drain     time.Duration
	docker    drainDocker
	store     drainStore
	machineID func() string

	// writeLock serialises all writes of this machine's container records to the store: every Controller sync and
	// every drain mark and clear. It's a channel with capacity 1 so that waiting for it can be cancelled.
	writeLock chan struct{}

	mu sync.Mutex
	// draining maps container IDs to the number of in-flight stop requests draining them.
	draining map[string]int
	// pending holds the IDs of drained containers whose true state couldn't be republished yet.
	pending map[string]struct{}

	// The timeouts below are the drain* constants and SyncInterval, replaced in tests.
	storeTimeout   time.Duration
	siblingTimeout time.Duration
	siblingWait    time.Duration
	siblingPoll    time.Duration
	inspectTimeout time.Duration
	// syncTimeout bounds a Controller sync (the work done under the write lock) when the drain is on.
	syncTimeout time.Duration
	// wait waits for the drain time or until ctx is done. Replaced in tests.
	wait func(ctx context.Context, d time.Duration) error
}

// NewDrainer creates a Drainer for the containers of the Docker service that writes container records of the machine
// with the given ID to the cluster store. A zero drain time returns a disabled Drainer.
func NewDrainer(drain time.Duration, service *Service, store *store.Store, machineID func() string) *Drainer {
	return newDrainer(drain, serviceDrainDocker{Service: service}, store, machineID)
}

func newDrainer(drain time.Duration, docker drainDocker, store drainStore, machineID func() string) *Drainer {
	return &Drainer{
		drain:     drain,
		docker:    docker,
		store:     store,
		machineID: machineID,
		writeLock: make(chan struct{}, 1),
		draining:  make(map[string]int),
		pending:   make(map[string]struct{}),

		storeTimeout:   drainStoreTimeout,
		siblingTimeout: drainSiblingTimeout,
		siblingWait:    drainSiblingWait,
		siblingPoll:    drainSiblingPoll,
		inspectTimeout: drainInspectTimeout,
		// A sync normally takes well under a second. One that runs as long as the periodic sync interval is stuck
		// (a hung Docker, machine DB or store call); cancelling it frees the write lock and the controller restarts
		// its sync loop (WatchAndSyncContainers returns the error, cluster.go retries with backoff).
		syncTimeout: SyncInterval,
		wait:        waitDuration,
	}
}

func waitDuration(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Enabled returns true if drain before stop is turned on.
func (d *Drainer) Enabled() bool {
	return d != nil && d.drain > 0
}

// lockWrites acquires the store write lock and returns the function that releases it. It's a no-op when the Drainer
// is disabled.
func (d *Drainer) lockWrites(ctx context.Context) (func(), error) {
	if !d.Enabled() {
		return func() {}, nil
	}
	select {
	case d.writeLock <- struct{}{}:
		return func() { <-d.writeLock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// boundSync returns the context for a Controller sync that holds the write lock: bounded by syncTimeout when the
// drain is on, unchanged when it's off.
func (d *Drainer) boundSync(ctx context.Context) (context.Context, context.CancelFunc) {
	if !d.Enabled() {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d.syncTimeout)
}

// HasAny returns true if any container is in the drain registry.
func (d *Drainer) HasAny() bool {
	if !d.Enabled() {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.draining) > 0
}

// IsDraining returns true if the container with the given full ID is in the drain registry.
func (d *Drainer) IsDraining(id string) bool {
	if !d.Enabled() {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.draining[id] > 0
}

// publishView returns the container record to write to the store: the container itself when it isn't draining, or
// a copy that is not healthy when it is. The caller must hold the write lock.
func (d *Drainer) publishView(ctr api.ServiceContainer) api.ServiceContainer {
	if !d.IsDraining(ctr.ID) {
		return ctr
	}
	return withDrainOverride(ctr)
}

// withDrainOverride returns a copy of the container that api.Container.Healthy reports as not healthy: its health
// status is "unhealthy", including for a container without a health check (whose Health is nil and so counts as
// healthy). State.Running is kept, so the deploy planner (which reads State.Running from the store) sees the real
// running state. ContainerJSONBase and State are pointers shared with the caller's object, so both are copied and the
// copy gets a new Health. The caller's object is not modified.
func withDrainOverride(ctr api.ServiceContainer) api.ServiceContainer {
	if ctr.ContainerJSONBase == nil || ctr.State == nil {
		return ctr
	}
	base := *ctr.ContainerJSONBase
	st := *base.State
	st.Health = &container.Health{Status: container.Unhealthy}
	base.State = &st
	ctr.ContainerJSONBase = &base
	return ctr
}

// shouldDrain returns true if the container must be drained before it's stopped.
func shouldDrain(ctr api.ServiceContainer) bool {
	if ctr.ContainerJSONBase == nil || ctr.State == nil || ctr.Config == nil {
		return false
	}
	// Not running or not healthy: DNS and Caddy already don't use it.
	if !ctr.Container.Healthy() {
		return false
	}
	// Pre-deploy hook containers are one-off jobs that aren't in DNS or Caddy. The daemon labels them when it
	// creates them (CreateServiceContainer with the PRE_DEPLOY type).
	if ctr.IsHook() {
		return false
	}
	// Draining Caddy through DNS is meaningless and would only delay a stop-first Caddy replacement.
	if ctr.ServiceName() == caddyServiceName {
		return false
	}
	return true
}

// isHealthySibling returns true if c is another container of ctr's service that DNS would answer with
// (dns/resolver.go updateServiceIPs): not ctr itself, same service ID, not a hook, healthy (a container published as
// draining is not), with an uncloud network IP. A container this daemon is draining doesn't count either.
func (d *Drainer) isHealthySibling(c, ctr api.ServiceContainer) bool {
	if c.ID == ctr.ID || c.ServiceID() != ctr.ServiceID() || c.IsHook() {
		return false
	}
	if c.ContainerJSONBase == nil || c.State == nil || c.NetworkSettings == nil || !c.Container.Healthy() {
		return false
	}
	return c.UncloudNetworkIP().IsValid() && !d.IsDraining(c.ID)
}

// storeHasHealthySibling returns true if the cluster store has a healthy sibling of ctr on any machine. The read is
// bounded by siblingTimeout.
func (d *Drainer) storeHasHealthySibling(ctx context.Context, ctr api.ServiceContainer) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.siblingTimeout)
	defer cancel()

	records, err := d.store.ListContainers(ctx, store.ListOptions{
		ServiceIDOrName: store.ServiceIDOrNameOptions{ID: ctr.ServiceID()},
	})
	if err != nil {
		return false, fmt.Errorf("list service containers from store: %w", err)
	}
	for _, r := range records {
		if d.isHealthySibling(r.Container, ctr) {
			return true, nil
		}
	}
	return false, nil
}

// localHasHealthySibling returns true if the local Docker daemon runs a healthy sibling of ctr. The read is bounded
// by siblingTimeout.
func (d *Drainer) localHasHealthySibling(ctx context.Context, ctr api.ServiceContainer) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.siblingTimeout)
	defer cancel()

	result, err := d.docker.ListServiceContainers(ctx, ctr.ServiceID(), container.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list local service containers: %w", err)
	}
	for _, c := range result.Containers {
		if d.isHealthySibling(c, ctr) {
			return true, nil
		}
	}
	return false, nil
}

// siblingAllowsDrain decides whether draining ctr is useful, i.e. another healthy container of the service can take
// its traffic once ctr is published as not healthy:
//   - a healthy sibling in the cluster store → drain;
//   - else, if the local Docker daemon runs a healthy sibling (its record isn't published yet, e.g. the new container
//     of a start-first replace), wait up to siblingWait for the store to show a healthy sibling → drain; if it never
//     does → no drain;
//   - else → no drain (stop-first with one replica, uc stop/rm, scale to 0).
//
// An error means the check couldn't be done (the caller stops without a drain, or returns ctx.Err() when the request
// was cancelled).
func (d *Drainer) siblingAllowsDrain(ctx context.Context, ctr api.ServiceContainer) (bool, error) {
	ok, err := d.storeHasHealthySibling(ctx, ctr)
	if err != nil || ok {
		return ok, err
	}

	local, err := d.localHasHealthySibling(ctx, ctr)
	if err != nil {
		return false, err
	}
	if !local {
		slog.Info("No other healthy container of the service, stopping without a drain.",
			"id", ctr.ID, "service", ctr.ServiceName())
		return false, nil
	}

	wctx, cancel := context.WithTimeout(ctx, d.siblingWait)
	defer cancel()
	ticker := time.NewTicker(d.siblingPoll)
	defer ticker.Stop()
	for {
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			slog.Info("A healthy container of the service runs locally but isn't published in the store yet, "+
				"stopping without a drain.", "id", ctr.ID, "service", ctr.ServiceName(), "waited", d.siblingWait)
			return false, nil
		case <-ticker.C:
			// Errors are retried until the wait ends.
			if ok, err = d.storeHasHealthySibling(wctx, ctr); err == nil && ok {
				return true, nil
			}
		}
	}
}

// StopContainer stops the container, draining it first when it's a running, healthy service container and another
// healthy container of the same service can take its traffic (siblingAllowsDrain). Otherwise, or when a check fails or
// the not-healthy mark can't be written to the store, the container is stopped at once, as without the Drainer.
// After the first inspect, the container is stopped by its full ID. If ctx is cancelled before the stop, the
// container is not stopped and ctx.Err() is returned.
func (d *Drainer) StopContainer(ctx context.Context, id string, opts container.StopOptions) error {
	ictx, cancel := context.WithTimeout(ctx, d.inspectTimeout)
	ctr, err := d.docker.InspectServiceContainer(ictx, id)
	cancel()
	if err != nil {
		// Not a service container, not found, or Docker didn't answer in time: stop as without the Drainer.
		return d.docker.ContainerStop(ctx, id, opts)
	}
	ctrID := ctr.ID
	if !shouldDrain(ctr) {
		return d.docker.ContainerStop(ctx, ctrID, opts)
	}

	drain, err := d.siblingAllowsDrain(ctx, ctr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("Failed to check for other healthy containers of the service, stopping without a drain.",
			"id", ctrID, "service", ctr.ServiceName(), "err", err)
		return d.docker.ContainerStop(ctx, ctrID, opts)
	}
	if !drain {
		return d.docker.ContainerStop(ctx, ctrID, opts)
	}

	marked, err := d.mark(ctx, ctrID)
	if err != nil {
		if ctx.Err() != nil {
			// The request was cancelled before the stop. mark() has already cleared what it set.
			return ctx.Err()
		}
		slog.Warn("Failed to publish container as not healthy before stop, stopping it without a drain.",
			"id", ctrID, "err", err)
		return d.docker.ContainerStop(ctx, ctrID, opts)
	}
	if !marked {
		// The container stopped being healthy, or became a container that's not drained, since the first inspect.
		return d.docker.ContainerStop(ctx, ctrID, opts)
	}

	slog.Info("Draining container before stop: published as not healthy, waiting.",
		"id", ctrID, "service", ctr.ServiceName(), "drain", d.drain)

	if err = d.wait(ctx, d.drain); err != nil {
		d.clear(ctrID, "request cancelled before stop")
		return err
	}

	err = d.docker.ContainerStop(ctx, ctrID, opts)
	reason := "stopped"
	if err != nil {
		reason = "stop failed"
	}
	d.clear(ctrID, reason)
	return err
}

// mark adds the container to the drain registry and publishes it to the store as not healthy, under the write lock.
// It re-inspects the container under the lock so the published record is the latest Docker state. It returns
// marked=false without an error if the container no longer needs a drain. On an error, the registry entry is removed
// and the container's true state is republished (a failed or cancelled write may still have been applied).
// The whole mark is bounded by storeTimeout.
func (d *Drainer) mark(reqCtx context.Context, id string) (marked bool, err error) {
	ctx, cancel := context.WithTimeout(reqCtx, d.storeTimeout)
	defer cancel()

	release, err := d.lockWrites(ctx)
	if err != nil {
		return false, err
	}
	defer release()

	machineID := d.machineID()
	if machineID == "" {
		return false, errors.New("machine ID is unknown")
	}

	ctr, err := d.docker.InspectServiceContainer(ctx, id)
	if err != nil {
		return false, fmt.Errorf("inspect container: %w", err)
	}
	if !shouldDrain(ctr) {
		return false, nil
	}

	d.add(ctr.ID)
	if err = d.store.CreateOrUpdateContainer(ctx, withDrainOverride(ctr), machineID); err != nil {
		if d.remove(ctr.ID, false) {
			rctx, rcancel := context.WithTimeout(context.Background(), d.storeTimeout)
			defer rcancel()
			d.restoreLocked(rctx, ctr.ID)
		}
		return false, fmt.Errorf("write container record: %w", err)
	}
	return true, nil
}

// clear removes one drain of the container from the registry. When no drain of it is left, it republishes the
// container's true state to the store under the write lock. If that isn't possible now, the container is kept for
// a retry (see retryPendingLocked). Every clear also retries the earlier failed republishes.
func (d *Drainer) clear(id, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), d.storeTimeout)
	defer cancel()

	release, err := d.lockWrites(ctx)
	if err != nil {
		if d.remove(id, false) {
			d.addPending(id)
			slog.Warn("Timed out waiting to republish drained container, will retry on the next sync.",
				"id", id, "reason", reason, "err", err)
		}
		return
	}
	defer release()

	// Earlier failures first, so a republish that fails now waits for a later clear or sync instead of being retried
	// at once.
	d.retryPendingLocked(ctx)
	if d.remove(id, false) {
		slog.Info("Drain mark cleared, republishing the container's true state.", "id", id, "reason", reason)
		d.restoreLocked(ctx, id)
	}
}

// Forget removes the container from the drain registry and the retry set without republishing. It's called when the
// container is removed: the sync triggered by the removal deletes its record from the store.
func (d *Drainer) Forget(id string) {
	if !d.Enabled() {
		return
	}
	d.remove(id, true)
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

// restoreLocked republishes the container's true state and keeps it in the retry set until that succeeds or the
// container is gone. A container that is being drained again is left to that drain. The caller must hold the write
// lock.
func (d *Drainer) restoreLocked(ctx context.Context, id string) {
	if d.IsDraining(id) {
		d.deletePending(id)
		return
	}
	if err := d.republishLocked(ctx, id); err != nil {
		d.addPending(id)
		slog.Warn("Failed to republish drained container's true state, will retry.", "id", id, "err", err)
		return
	}
	d.deletePending(id)
}

// retryPendingLocked retries republishing the true state of the containers whose earlier republish failed. It's
// called at the start of every sync and at every clear. The caller must hold the write lock.
func (d *Drainer) retryPendingLocked(ctx context.Context) {
	if !d.Enabled() {
		return
	}
	d.mu.Lock()
	ids := make([]string, 0, len(d.pending))
	for id := range d.pending {
		ids = append(ids, id)
	}
	d.mu.Unlock()

	for _, id := range ids {
		d.restoreLocked(ctx, id)
	}
}

// republishLocked writes the container's true state to the store. A container that no longer exists needs nothing
// (the sync deletes its record). The caller must hold the write lock.
func (d *Drainer) republishLocked(ctx context.Context, id string) error {
	ctr, err := d.docker.InspectServiceContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect container: %w", err)
	}
	if err = d.store.CreateOrUpdateContainer(ctx, ctr, d.machineID()); err != nil {
		return fmt.Errorf("write container record: %w", err)
	}
	return nil
}

func (d *Drainer) add(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draining[id]++
}

// remove removes one drain (or all when all is true) of the container from the registry. It returns true if the
// container was in the registry and no drain of it is left.
func (d *Drainer) remove(id string, all bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.draining[id]
	if !ok {
		return false
	}
	if all || n <= 1 {
		delete(d.draining, id)
		return true
	}
	d.draining[id] = n - 1
	return false
}

func (d *Drainer) addPending(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[id] = struct{}{}
}

func (d *Drainer) deletePending(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pending, id)
}

// drainingCount returns the number of containers in the drain registry.
func (d *Drainer) drainingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.draining)
}

// pendingCount returns the number of containers waiting for a republish retry.
func (d *Drainer) pendingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}
