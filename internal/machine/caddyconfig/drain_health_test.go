package caddyconfig

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/stretchr/testify/assert"
)

// A container being drained before stop is published with State.Health.Status "unhealthy" and State.Running kept
// (docker.withDrainOverride). The Caddy controller must drop it from the containers it generates the config from
// and see it as changed in the fingerprint, also when the container has no healthcheck.
func TestFilterHealthyContainers_DropsDrainingContainer(t *testing.T) {
	t.Parallel()

	keep := newContainerRecordWithPorts("web", "10.210.0.2", []string{"app.example.com:8000/https"}, "mach-1")
	draining := newContainerRecordWithPorts("web", "10.210.0.3", []string{"app.example.com:8000/https"}, "mach-1")
	assert.True(t, draining.Container.Healthy(), "no healthcheck counts as healthy before the override")
	before := fingerprintContainers([]store.ContainerRecord{keep, draining})

	draining.Container.State.Health = &container.Health{Status: container.Unhealthy}

	got := filterHealthyContainers([]store.ContainerRecord{keep, draining})
	assert.Len(t, got, 1)
	assert.Equal(t, keep.Container.ID, got[0].Container.ID)
	assert.NotEqual(t, before, fingerprintContainers([]store.ContainerRecord{keep, draining}),
		"the fingerprint must change so the controller regenerates")
}
