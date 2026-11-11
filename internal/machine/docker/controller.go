package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/psviderski/uncloud/pkg/api"
)

const (
	NetworkName = "uncloud"
	// EventsDebounceInterval defines how long to wait before processing the next Docker event. Multiple events
	// occurring within this window will be processed together as a single event to prevent system overload.
	EventsDebounceInterval = 100 * time.Millisecond
	// SyncInterval defines a regular interval to sync containers to the cluster store.
	SyncInterval = 30 * time.Second
)

// containerStore is the subset of the cluster store used to sync this machine's containers.
type containerStore interface {
	ListContainers(ctx context.Context, opts store.ListOptions) ([]store.ContainerRecord, error)
	DeleteContainers(ctx context.Context, opts store.DeleteOptions) error
	CreateOrUpdateContainer(ctx context.Context, ctr api.ServiceContainer, machineID string) error
}

// serviceContainerLister lists the service containers on this machine from the Docker daemon.
type serviceContainerLister interface {
	ListServiceContainers(
		ctx context.Context, serviceNameOrID string, opts container.ListOptions,
	) (ListServiceContainersResult, error)
}

// Controller monitors Docker events and synchronises service containers with the cluster store.
type Controller struct {
	// machineID is the ID of the machine where the managed Docker daemon is running.
	machineID string
	client    *client.Client
	service   *Service
	store     containerStore
	// lister lists the containers to sync. It's the service, replaced in tests.
	lister serviceContainerLister
	// drain applies the drain-before-stop override to the containers being drained and serialises the store writes
	// with the drain marks. A nil or disabled Drainer changes nothing.
	drain *Drainer
}

func NewController(machineID string, service *Service, store *store.Store, drain *Drainer) *Controller {
	return &Controller{
		machineID: machineID,
		client:    service.Client,
		service:   service,
		store:     store,
		lister:    service,
		drain:     drain,
	}
}

// WaitDaemonReady waits for the Docker daemon to start and be ready to serve requests.
func (c *Controller) WaitDaemonReady(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	ready, waitingLogged := false, false
	for !ready {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, err := c.client.Ping(ctx)
			if err == nil {
				ready = true
				break
			}
			if !client.IsErrConnectionFailed(err) {
				return fmt.Errorf("connect to Docker daemon: %w", err)
			}
			if !waitingLogged {
				slog.Info("Waiting for Docker daemon to start and be ready.")
				waitingLogged = true
			}
		}
	}
	return nil
}

func (c *Controller) WatchAndSyncContainers(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Filter only local container events.
	opts := events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("scope", "local"),
			filters.Arg("type", string(events.ContainerEventType)),
		),
	}

	// Subscribe to Docker events before running the initial sync to avoid missing any events.
	eventCh, errCh := c.service.Client.Events(ctx, opts)
	slog.Debug("Syncing containers to cluster store before processing Docker events.")
	if err := c.syncContainersToStore(ctx); err != nil {
		// The deferred cancel will stop the event subscription.
		return fmt.Errorf("sync containers to cluster store: %w", err)
	}

	var (
		// debouncer is used to debounce multiple Docker events into a single event sent to the debouncerCh
		// to prevent system overload.
		debouncer   *time.Timer
		debouncerCh = make(chan events.Message)
		// ticker is used to trigger a regular sync of containers to the cluster store as a fallback.
		ticker = time.NewTicker(SyncInterval)
	)
	defer ticker.Stop()

	for {
		select {
		case e := <-eventCh:
			switch e.Action {
			// Actions that may trigger a container state change or creation/deletion of a container.
			case events.ActionCreate,
				events.ActionStart,
				events.ActionStop,
				events.ActionPause,
				events.ActionUnPause,
				events.ActionKill,
				events.ActionDie,
				events.ActionOOM,
				events.ActionDestroy,
				events.ActionHealthStatusRunning,
				events.ActionHealthStatusHealthy,
				events.ActionHealthStatusUnhealthy:

				if debouncer == nil {
					debouncer = time.AfterFunc(EventsDebounceInterval, func() {
						debouncerCh <- e
					})
				}
			}
		case e := <-debouncerCh:
			debouncer = nil
			slog.Debug("Syncing containers to cluster store triggered by a Docker container event.",
				"container_id", e.Actor.ID,
				"container_name", e.Actor.Attributes["name"],
				"action", e.Action)

			if err := c.syncContainersToStore(ctx); err != nil {
				return fmt.Errorf("sync containers to cluster store: %w", err)
			}
		case <-ticker.C:
			slog.Debug("Syncing containers to cluster store triggered by a regular interval.",
				"interval", SyncInterval)
			if err := c.syncContainersToStore(ctx); err != nil {
				return fmt.Errorf("sync containers to cluster store: %w", err)
			}
		case err := <-errCh:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive Docker event: %w", err)
		}
	}
}

func (c *Controller) syncContainersToStore(ctx context.Context) error {
	// Hold the store write lock for the whole sync (list from Docker, then write) when drain before stop is on, so a
	// sync that read a container's state before a drain mark can't overwrite the mark after it. No-op when off.
	release, err := c.drain.lockWrites(ctx)
	if err != nil {
		return fmt.Errorf("wait for container store write lock: %w", err)
	}
	defer release()
	// With the drain on, bound the work done under the lock so a hung Docker, machine DB or store call can't hold it
	// forever; then retry the republishes that failed earlier. Both are no-ops when the drain is off.
	ctx, cancel := c.drain.boundSync(ctx)
	defer cancel()
	c.drain.retryPendingLocked(ctx)

	storeContainers, err := c.store.ListContainers(ctx, store.ListOptions{MachineIDs: []string{c.machineID}})
	if err != nil {
		return fmt.Errorf("list containers from store: %w", err)
	}

	// List containers for all services, including stopped ones and deployment hooks.
	result, err := c.lister.ListServiceContainers(ctx, "", container.ListOptions{All: true})
	if err != nil {
		// TODO: mark all containers as outdated in the store.
		return fmt.Errorf("list service containers: %w", err)
	}

	// Sync both regular and one-off hook containers to the store.
	containers := append(result.Containers, result.HookContainers...)

	// Delete containers from the store that are no longer present in the Docker daemon.
	var deleteIDs []string
	for _, sc := range storeContainers {
		found := false
		for i := range containers {
			if containers[i].ID == sc.Container.ID {
				found = true
				break
			}
		}
		if !found {
			deleteIDs = append(deleteIDs, sc.Container.ID)
		}
	}

	var storeErr error
	if len(deleteIDs) > 0 {
		if err = c.store.DeleteContainers(ctx, store.DeleteOptions{IDs: deleteIDs}); err != nil {
			storeErr = fmt.Errorf("delete containers from store: %w", err)
		}
	}

	// Create or update the current Docker containers in the store. A container being drained before stop is
	// written as not healthy (publishView returns the container itself otherwise).
	for _, ctr := range containers {
		if err = c.store.CreateOrUpdateContainer(ctx, c.drain.publishView(ctr), c.machineID); err != nil {
			storeErr = errors.Join(storeErr, fmt.Errorf("create or update container '%s': %w", ctr.ID, err))
		}
	}
	return storeErr
}
