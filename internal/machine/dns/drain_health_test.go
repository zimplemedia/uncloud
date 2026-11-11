package dns

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/stretchr/testify/assert"
)

// A container being drained before stop is published with State.Health.Status "unhealthy" and State.Running kept
// (docker.withDrainOverride). The resolver must drop it, also when the container has no healthcheck (Health is nil
// in Docker, which counts as healthy).
func TestClusterResolver_DropsDrainingContainer(t *testing.T) {
	t.Parallel()

	keep := newRecord("svc-id-1", "web", "10.210.0.2", "mach-1")
	draining := newRecord("svc-id-1", "web", "10.210.0.3", "mach-1")
	assert.True(t, draining.Container.Healthy(), "no healthcheck counts as healthy before the override")
	draining.Container.State.Health = &container.Health{Status: container.Unhealthy}

	r := NewClusterResolver(nil)
	r.updateServiceIPs([]store.ContainerRecord{keep, draining})

	got := r.Resolve("web")
	assert.Len(t, got, 1)
	assert.Equal(t, "10.210.0.2", got[0].String())
}
