package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/corrosion"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthyContainer returns a container with a health state carrying probe history.
func healthyContainer(failingStreak int, logs ...*container.HealthcheckResult) api.ServiceContainer {
	return api.ServiceContainer{
		Container: api.Container{
			InspectResponse: container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:      "ctr1",
					ExecIDs: []string{"exec-abc"},
					State: &container.State{
						Status:  container.StateRunning,
						Running: true,
						Health: &container.Health{
							Status:        container.Healthy,
							FailingStreak: failingStreak,
							Log:           logs,
						},
					},
				},
				Config: &container.Config{},
			},
		},
	}
}

func probe(end time.Time, exitCode int) *container.HealthcheckResult {
	return &container.HealthcheckResult{
		Start:    end.Add(-time.Second),
		End:      end,
		ExitCode: exitCode,
		Output:   "probe output",
	}
}

func TestNormaliseContainerForStore_ClearsHealthChurn(t *testing.T) {
	t.Parallel()

	ctr := healthyContainer(3, probe(time.Unix(1000, 0).UTC(), 0), probe(time.Unix(1030, 0).UTC(), 1))

	normaliseContainerForStore(&ctr)

	require.NotNil(t, ctr.State)
	require.NotNil(t, ctr.State.Health)
	assert.Nil(t, ctr.State.Health.Log, "Health.Log must be cleared")
	assert.Zero(t, ctr.State.Health.FailingStreak, "Health.FailingStreak must be cleared")
	assert.Equal(t, container.Healthy, ctr.State.Health.Status, "Health.Status must be preserved")
	assert.Nil(t, ctr.ExecIDs, "ExecIDs must be cleared")
	// Unrelated State fields must survive.
	assert.True(t, ctr.State.Running)
	assert.Equal(t, container.StateRunning, ctr.State.Status)
}

func TestNormaliseContainerForStore_DoesNotMutateCaller(t *testing.T) {
	t.Parallel()

	// The caller (docker controller) passes the container by value but State/Health are pointers into the objects
	// it keeps using, so normalisation must not clear them in place.
	original := healthyContainer(2, probe(time.Unix(2000, 0).UTC(), 1))
	originalBase := original.ContainerJSONBase
	originalState := original.State
	originalHealth := original.State.Health

	ctr := original // Copy the same way CreateOrUpdateContainer receives it.
	normaliseContainerForStore(&ctr)

	require.Len(t, original.State.Health.Log, 1, "caller's Health.Log must be untouched")
	assert.Equal(t, 1, original.State.Health.Log[0].ExitCode)
	assert.Equal(t, 2, original.State.Health.FailingStreak, "caller's Health.FailingStreak must be untouched")
	assert.Equal(t, []string{"exec-abc"}, original.ExecIDs, "caller's ExecIDs must be untouched")
	// ExecIDs and State live in the pointer-embedded ContainerJSONBase, so the whole chain must still be the caller's.
	assert.Same(t, originalBase, original.ContainerJSONBase)
	assert.Same(t, originalState, original.State)
	assert.Same(t, originalHealth, original.State.Health)

	// The normalised copy must point at different structs.
	assert.NotSame(t, originalBase, ctr.ContainerJSONBase)
	assert.NotSame(t, originalState, ctr.State)
	assert.NotSame(t, originalHealth, ctr.State.Health)
	assert.Nil(t, ctr.State.Health.Log)
	assert.Nil(t, ctr.ExecIDs)
}

func TestNormaliseContainerForStore_HealthChurnDoesNotChangeJSON(t *testing.T) {
	t.Parallel()

	// Two snapshots of the same container taken between two healthcheck probes.
	first := healthyContainer(0, probe(time.Unix(3000, 0).UTC(), 0), probe(time.Unix(3030, 0).UTC(), 0))
	second := healthyContainer(1,
		probe(time.Unix(3030, 0).UTC(), 0),
		probe(time.Unix(3060, 0).UTC(), 1),
		probe(time.Unix(3090, 0).UTC(), 1),
	)

	beforeFirst, err := json.Marshal(first)
	require.NoError(t, err)
	beforeSecond, err := json.Marshal(second)
	require.NoError(t, err)
	require.NotEqual(t, string(beforeFirst), string(beforeSecond),
		"sanity check: the probe history must differ before normalisation")

	normaliseContainerForStore(&first)
	normaliseContainerForStore(&second)

	afterFirst, err := json.Marshal(first)
	require.NoError(t, err)
	afterSecond, err := json.Marshal(second)
	require.NoError(t, err)
	assert.Equal(t, string(afterFirst), string(afterSecond),
		"containers differing only in Health.Log/FailingStreak must serialise identically")
}

func TestNormaliseContainerForStore_NilStateAndHealth(t *testing.T) {
	t.Parallel()

	t.Run("nil state", func(t *testing.T) {
		base := &container.ContainerJSONBase{ID: "ctr1", ExecIDs: []string{"exec-abc"}}
		ctr := api.ServiceContainer{
			Container: api.Container{
				InspectResponse: container.InspectResponse{
					ContainerJSONBase: base,
					Config:            &container.Config{},
				},
			},
		}
		require.NotPanics(t, func() { normaliseContainerForStore(&ctr) })
		assert.Nil(t, ctr.State)
		// ExecIDs must be cleared even for a container with no health state at all.
		assert.Nil(t, ctr.ExecIDs)
		assert.Equal(t, []string{"exec-abc"}, base.ExecIDs, "caller's ExecIDs must be untouched")
	})

	t.Run("nil health", func(t *testing.T) {
		base := &container.ContainerJSONBase{
			ID:      "ctr1",
			ExecIDs: []string{"exec-abc"},
			State:   &container.State{Status: container.StateRunning, Running: true},
		}
		ctr := api.ServiceContainer{
			Container: api.Container{
				InspectResponse: container.InspectResponse{
					ContainerJSONBase: base,
					Config:            &container.Config{},
				},
			},
		}
		state := ctr.State
		require.NotPanics(t, func() { normaliseContainerForStore(&ctr) })
		assert.Same(t, state, ctr.State, "State must be left alone when there is no health data")
		assert.Nil(t, ctr.State.Health)
		assert.Nil(t, ctr.ExecIDs)
		assert.Equal(t, []string{"exec-abc"}, base.ExecIDs, "caller's ExecIDs must be untouched")
	})
}

func TestNormaliseContainerForStore_ExecIDsChurnDoesNotChangeJSON(t *testing.T) {
	t.Parallel()

	// A Docker healthcheck runs as an exec, so a sync that catches a probe in flight sees ExecIDs populated while
	// the next sync sees it null. Both snapshots must serialise identically after normalisation.
	probes := []*container.HealthcheckResult{probe(time.Unix(4000, 0).UTC(), 0)}
	inFlight := healthyContainer(0, probes...)
	inFlight.ExecIDs = []string{"7f3c1a2b4d5e"}
	idle := healthyContainer(0, probes...)
	idle.ExecIDs = nil

	beforeInFlight, err := json.Marshal(inFlight)
	require.NoError(t, err)
	beforeIdle, err := json.Marshal(idle)
	require.NoError(t, err)
	require.NotEqual(t, string(beforeInFlight), string(beforeIdle),
		"sanity check: ExecIDs must differ before normalisation")

	normaliseContainerForStore(&inFlight)
	normaliseContainerForStore(&idle)

	afterInFlight, err := json.Marshal(inFlight)
	require.NoError(t, err)
	afterIdle, err := json.Marshal(idle)
	require.NoError(t, err)
	assert.Equal(t, string(afterInFlight), string(afterIdle),
		"containers differing only in ExecIDs must serialise identically")
}

// recvSignal waits up to timeout for a signal and reports whether one was received.
func recvSignal(t *testing.T, signals <-chan struct{}, timeout time.Duration) bool {
	t.Helper()
	select {
	case _, ok := <-signals:
		require.True(t, ok, "signals channel closed unexpectedly")
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestCoalesceSignals_BurstProducesOneSignal(t *testing.T) {
	t.Parallel()

	const delay = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *corrosion.ChangeEvent)
	signals := coalesceSignals(ctx, events, delay)

	start := time.Now()
	for i := 0; i < 50; i++ {
		events <- &corrosion.ChangeEvent{Type: corrosion.ChangeTypeUpdate, ChangeID: uint64(i)}
	}
	require.Less(t, time.Since(start), delay, "the burst must be sent within the coalesce window")

	require.True(t, recvSignal(t, signals, 2*time.Second), "expected one signal for the burst")
	assert.GreaterOrEqual(t, time.Since(start), delay, "the signal must not be emitted before the delay elapses")
	assert.False(t, recvSignal(t, signals, 10*delay), "the burst must produce exactly one signal")

	// A second burst after a quiet period produces a second signal.
	events <- &corrosion.ChangeEvent{Type: corrosion.ChangeTypeInsert, ChangeID: 100}
	events <- &corrosion.ChangeEvent{Type: corrosion.ChangeTypeInsert, ChangeID: 101}
	assert.True(t, recvSignal(t, signals, 2*time.Second), "expected a signal for the second burst")
	assert.False(t, recvSignal(t, signals, 10*delay), "the second burst must produce exactly one signal")
}

func TestCoalesceSignals_NoChangeLostWhileSignalPending(t *testing.T) {
	t.Parallel()

	// A longer delay keeps the test deterministic: the consumer drains the pending signal well inside the window
	// opened by the second event.
	const delay = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *corrosion.ChangeEvent)
	signals := coalesceSignals(ctx, events, delay)

	// First change: let the signal land in the buffer without consuming it.
	events <- &corrosion.ChangeEvent{Type: corrosion.ChangeTypeUpdate, ChangeID: 1}
	require.Eventually(t, func() bool { return len(signals) == 1 }, 2*time.Second, time.Millisecond,
		"expected a signal to be queued for the busy consumer")

	// Second change arrives while the signal is still pending.
	events <- &corrosion.ChangeEvent{Type: corrosion.ChangeTypeUpdate, ChangeID: 2}
	// The consumer finally takes the pending signal.
	require.True(t, recvSignal(t, signals, time.Second))

	// The change that arrived while it was busy must still be signalled.
	assert.True(t, recvSignal(t, signals, 2*time.Second),
		"a change received while a signal was pending must produce a later signal")
}

func TestCoalesceSignals_ContextCancelClosesChannel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan *corrosion.ChangeEvent)
	signals := coalesceSignals(ctx, events, 20*time.Millisecond)

	cancel()

	select {
	case _, ok := <-signals:
		assert.False(t, ok, "signals channel must be closed after the context is cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("signals channel was not closed after the context was cancelled")
	}
}

func TestCoalesceSignals_ClosedEventsClosesChannel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *corrosion.ChangeEvent)
	signals := coalesceSignals(ctx, events, 20*time.Millisecond)

	close(events)

	select {
	case _, ok := <-signals:
		assert.False(t, ok, "signals channel must be closed when the events channel closes")
	case <-time.After(2 * time.Second):
		t.Fatal("signals channel was not closed after the events channel closed")
	}
}
