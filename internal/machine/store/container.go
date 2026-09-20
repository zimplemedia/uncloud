package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/corrosion"
	"github.com/psviderski/uncloud/pkg/api"
)

const (
	// SyncStatusSynced indicates that a container record is synchronised with the Docker daemon. The record may
	// become outdated even when the status is "synced" if the machine crashes or a network partition occurs.
	// The cluster membership state of the machine should also be checked to determine if the record can be trusted.
	SyncStatusSynced = "synced"
	// SyncStatusOutdated indicates that a container record may be outdated, for example, due to being unable
	// to retrieve the container's state from the Docker daemon or when the machine is being stopped or restarted.
	SyncStatusOutdated = "outdated"

	// containerChangesCoalesceDelay is how long change events are collected before a single change signal is emitted
	// to the subscriber of the containers list.
	containerChangesCoalesceDelay = 250 * time.Millisecond
)

type ContainerRecord struct {
	Container  api.ServiceContainer
	MachineID  string
	SyncStatus string
	UpdatedAt  time.Time
}

type ListOptions struct {
	// MachineIDs filters containers by the machine IDs they are running on.
	MachineIDs      []string
	ServiceIDOrName ServiceIDOrNameOptions
}

// ServiceIDOrNameOptions filters containers by the service ID or name they are part of. If both ID and Name are
// provided, they are combined with an OR operator.
type ServiceIDOrNameOptions struct {
	ID   string
	Name string
}

type DeleteOptions struct {
	// IDs filters containers by their container IDs.
	IDs []string
	// MachineIDs filters containers by the machine IDs they are running on.
	MachineIDs []string
}

// CreateOrUpdateContainer creates a new container record or updates an existing one in the store database.
// The container is associated with the given machine ID that indicates which machine the container is running on.
func (s *Store) CreateOrUpdateContainer(ctx context.Context, ctr api.ServiceContainer, machineID string) error {
	// Stabilise the order of slices that Docker returns non-deterministically, so that byte-level
	// comparison of the serialised container does not flag spurious changes.
	normaliseContainerForStore(&ctr)

	cJSON, err := json.Marshal(ctr)
	if err != nil {
		return fmt.Errorf("marshal container: %w", err)
	}

	// Insert or update the container record if the container or machine ID has changed.
	res, err := s.corro.ExecContext(ctx, `
		INSERT INTO containers (id, container, machine_id, sync_status, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT (id) DO UPDATE SET container   = excluded.container,
									   machine_id  = excluded.machine_id,
									   sync_status = excluded.sync_status,
									   updated_at  = excluded.updated_at
		WHERE containers.container != excluded.container
		  OR containers.machine_id != excluded.machine_id`,
		ctr.ID, string(cJSON), machineID, SyncStatusSynced)
	if err != nil {
		return fmt.Errorf("upsert query: %w", err)
	}
	if res.RowsAffected > 0 {
		slog.Debug("Container record updated in store DB.", "id", ctr.ID, "machine_id", machineID)
	}

	return nil
}

// normaliseContainerForStore removes potentially sensitive data and normalises the container fields that Docker may
// return in non-deterministic order so that byte-level comparison of the serialised container does not flag spurious
// changes.
func normaliseContainerForStore(ctr *api.ServiceContainer) {
	// Remove the environment variables to avoid leaking secrets.
	ctr.Config.Env = nil
	ctr.ServiceSpec.Container.Env = nil

	// Docker returns Mounts in a non-deterministic order so sort them.
	slices.SortFunc(ctr.Mounts, func(a, b container.MountPoint) int {
		return cmp.Or(
			strings.Compare(a.Destination, b.Destination),
			strings.Compare(a.Source, b.Source),
		)
	})

	// Drop the fields that a healthcheck probe churns on every run. They would rewrite the container record on every
	// probe interval, gossip that write to every machine in the cluster, and wake every subscriber (Caddy config and
	// DNS controllers) to regenerate everything - for data nothing reads.
	//
	//   - ExecIDs: a Docker healthcheck is implemented as an exec, so this flips between ["<exec id>"] and null
	//     depending on whether the periodic sync happens to catch a probe in flight. Nothing in the codebase reads
	//     ExecIDs (verified by grep over pkg/, cmd/ and internal/).
	//   - State.Health.Log: the last few probe results with their timestamps and output, so it changes on every probe.
	//   - State.Health.FailingStreak: the count of consecutive failures.
	//
	// Health.Status is kept: it is the only health field consumed from the store, by
	// api.Container.Healthy()/HumanState(), cmd/uc/ps.go and pkg/client/container.go.
	//
	// CreateOrUpdateContainer takes the container by value, but ContainerJSONBase, State and Health are pointers that
	// the copy shares with the caller's container object. ExecIDs and State both live in ContainerJSONBase, so even
	// assigning a new State would write through the shared base. Copy each struct down the chain before clearing its
	// fields so that nothing the caller still uses is modified.
	if ctr.ContainerJSONBase != nil {
		base := *ctr.ContainerJSONBase
		base.ExecIDs = nil

		if base.State != nil && base.State.Health != nil {
			st := *base.State
			h := *st.Health
			h.Log = nil
			h.FailingStreak = 0
			st.Health = &h
			base.State = &st
		}

		ctr.ContainerJSONBase = &base
	}
}

// ListContainers returns a list of container records from the store database that match the given options.
// The result excludes orphan containers whose machine is no longer in the cluster.
func (s *Store) ListContainers(ctx context.Context, opts ListOptions) ([]ContainerRecord, error) {
	q := sq.Select("c.id", "c.container", "c.machine_id", "c.sync_status", "c.updated_at").
		From("containers c").
		Join("machines m ON m.id = c.machine_id").
		Where(sq.Eq{"c.sync_status": SyncStatusSynced})

	if len(opts.MachineIDs) > 0 {
		q = q.Where(sq.Eq{"c.machine_id": opts.MachineIDs})
	}

	if opts.ServiceIDOrName.ID != "" || opts.ServiceIDOrName.Name != "" {
		var conditions []sq.Sqlizer
		if opts.ServiceIDOrName.ID != "" {
			conditions = append(conditions, sq.Eq{"c.service_id": opts.ServiceIDOrName.ID})
		}
		if opts.ServiceIDOrName.Name != "" {
			conditions = append(conditions, sq.Eq{"c.service_name": opts.ServiceIDOrName.Name})
		}
		q = q.Where(sq.Or(conditions))
	}

	query, args, err := q.ToSql()
	if err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}

	rows, err := s.corro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select query: %w", err)
	}
	defer rows.Close()

	var containers []ContainerRecord
	var id, cJSON, machineID, syncStatus, updatedAtStr string
	var updatedAt time.Time
	skipped := 0

	for rows.Next() {
		if err = rows.Scan(&id, &cJSON, &machineID, &syncStatus, &updatedAtStr); err != nil {
			return nil, fmt.Errorf("scan container record: %w", err)
		}

		// Skip containers with empty JSON data. This can happen during partial replication
		// when cr-sqlite has created the row but the container column hasn't been synced yet.
		if cJSON == "" || cJSON == "{}" {
			slog.Debug("Skipping container with empty data in the store (partial replication?).", "id", id)
			skipped++
			continue
		}

		var c api.ServiceContainer
		if err = json.Unmarshal([]byte(cJSON), &c); err != nil {
			return nil, fmt.Errorf("unmarshal container: %w", err)
		}
		if updatedAt, err = time.Parse(time.DateTime, updatedAtStr); err != nil {
			return nil, fmt.Errorf("parse updated_at: %w", err)
		}
		containers = append(containers, ContainerRecord{
			Container:  c,
			MachineID:  machineID,
			SyncStatus: syncStatus,
			UpdatedAt:  updatedAt,
		})
	}

	if skipped > 0 {
		slog.Warn("Listing containers from the store skipped empty records (possibly due to partial replication).",
			"skipped", skipped, "valid", len(containers))
	}

	return containers, nil
}

// DeleteContainers deletes container records from the store database that match the given options.
// If no filter is set, all container records are deleted. Filters are combined with AND.
func (s *Store) DeleteContainers(ctx context.Context, opts DeleteOptions) error {
	q := sq.Delete("containers")
	if len(opts.IDs) > 0 {
		q = q.Where(sq.Eq{"id": opts.IDs})
	}
	if len(opts.MachineIDs) > 0 {
		q = q.Where(sq.Eq{"machine_id": opts.MachineIDs})
	}

	query, args, err := q.ToSql()
	if err != nil {
		return fmt.Errorf("build query: %w", err)
	}

	res, err := s.corro.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("delete query: %w", err)
	}
	if res.RowsAffected > 0 {
		slog.Debug("Container records deleted from store DB.",
			"ids", opts.IDs, "machine_ids", opts.MachineIDs, "count", res.RowsAffected)
	}

	return nil
}

// SubscribeContainers returns a list of containers and a channel that signals changes to the list. The channel doesn't
// receive any values, it just signals when a container(s) has been added, updated, or deleted in the database.
// The result excludes orphan containers whose machine is no longer in the cluster.
// The channel is closed when the containers are no longer subscribable: either the provided context is cancelled or
// the underlying subscription fails.
func (s *Store) SubscribeContainers(ctx context.Context) ([]ContainerRecord, <-chan struct{}, error) {
	// TODO: figure out whether we need sync_status at all (not used at the moment).
	q := sq.Select("c.id", "c.container", "c.machine_id", "c.sync_status", "c.updated_at").
		From("containers c").
		Join("machines m ON m.id = c.machine_id").
		Where(sq.Eq{"c.sync_status": SyncStatusSynced})
	query, args, err := q.ToSql()
	if err != nil {
		return nil, nil, fmt.Errorf("build query: %w", err)
	}

	sub, err := s.corro.SubscribeContext(ctx, query, args, false)
	if err != nil {
		return nil, nil, err
	}

	var containers []ContainerRecord
	var id, cJSON, updatedAtStr string
	skipped := 0

	rows := sub.Rows()
	for rows.Next() {
		var cr ContainerRecord
		if err = rows.Scan(&id, &cJSON, &cr.MachineID, &cr.SyncStatus, &updatedAtStr); err != nil {
			return nil, nil, err
		}

		// Skip containers with empty JSON data. This can happen during partial replication
		// when cr-sqlite has created the row but the container column hasn't been synced yet.
		if cJSON == "" || cJSON == "{}" {
			slog.Debug("Skipping container with empty data in the store (partial replication?).", "id", id)
			skipped++
			continue
		}

		if err = json.Unmarshal([]byte(cJSON), &cr.Container); err != nil {
			return nil, nil, fmt.Errorf("unmarshal container: %w", err)
		}
		if cr.UpdatedAt, err = time.Parse(time.DateTime, updatedAtStr); err != nil {
			return nil, nil, fmt.Errorf("parse updated_at: %w", err)
		}
		containers = append(containers, cr)
	}

	if skipped > 0 {
		slog.Warn("Container subscription skipped empty records in the store (possibly due to partial replication).",
			"skipped", skipped, "valid", len(containers))
	}

	events, err := sub.Changes()
	if err != nil {
		return nil, nil, fmt.Errorf("get subscription changes: %w", err)
	}

	// Pass the change events through a goroutine that reports a subscription failure when the events channel closes,
	// then coalesce them into change signals. Coalescing is done on the forwarded channel so that coalesceSignals
	// stays free of subscription details and can be tested on its own.
	forwarded := make(chan *corrosion.ChangeEvent)
	go func() {
		defer close(forwarded)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok {
					// events channel has been closed.
					if sub.Err() != nil {
						slog.Error("Containers subscription failed.", "id", sub.ID(), "err", sub.Err())
					}
					return
				}
				select {
				case forwarded <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return containers, coalesceSignals(ctx, forwarded, containerChangesCoalesceDelay), nil
}

// coalesceSignals converts a stream of change events into a stream of change signals, collapsing every burst of
// events that occurs within delay of the first event of the burst into a single signal. A container change typically
// arrives as several events (one per changed row) and consumers react by re-listing and regenerating everything
// from scratch, so there is nothing to gain from signalling each event separately.
//
// The returned channel has a buffer of one and is written to with a non-blocking send: if a signal is still queued,
// the consumer has not re-listed yet and that queued signal already guarantees a re-list that happens after the
// dropped change, so no change is ever missed. The channel is closed when ctx is done or events is closed.
func coalesceSignals(ctx context.Context, events <-chan *corrosion.ChangeEvent, delay time.Duration) <-chan struct{} {
	signals := make(chan struct{}, 1)

	go func() {
		defer close(signals)

		// A nil channel blocks forever, so delayC being non-nil means a burst is in progress.
		var delayC <-chan time.Time

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-events:
				if !ok {
					return
				}
				if delayC == nil {
					// First event after a quiet period: start collecting the burst.
					delayC = time.After(delay)
				}
			case <-delayC:
				delayC = nil
				select {
				case signals <- struct{}{}:
				default:
					// A signal is already queued for the consumer; it will re-list after this change too.
				}
			}
		}
	}()

	return signals
}
