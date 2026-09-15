package caddyconfig

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContainerFingerprint_EqualCoversAllFields is a guard: when a field is added to containerFingerprint,
// Equal must also compare it. A mutation of any single field should flip equality to false. If this test fails
// after adding a field, update Equal to include it.
func TestContainerFingerprint_EqualCoversAllFields(t *testing.T) {
	t.Parallel()

	base := containerFingerprint{
		ID:      "container-1",
		IP:      netip.MustParseAddr("10.210.0.2"),
		Healthy: true,
		Ports: []api.PortSpec{{
			Hostname:      "app.example.com",
			ContainerPort: 8080,
			Protocol:      api.ProtocolHTTP,
			Mode:          api.PortModeIngress,
		}},
		CaddyConfig: "caddy-config",
	}

	assert.True(t, base.Equal(base), "base fingerprint must be equal to itself")

	rt := reflect.TypeOf(base)
	for field := range rt.Fields() {
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			mutated := base
			mutated.Ports = append([]api.PortSpec(nil), base.Ports...)
			v := reflect.ValueOf(&mutated).Elem().FieldByName(field.Name)

			switch field.Name {
			case "ID", "CaddyConfig":
				v.SetString(v.String() + "-changed")
			case "IP":
				v.Set(reflect.ValueOf(netip.MustParseAddr("10.210.0.99")))
			case "Healthy":
				v.SetBool(!v.Bool())
			case "Ports":
				mutated.Ports = []api.PortSpec{{
					Hostname:      "different.example.com",
					ContainerPort: 9090,
					Protocol:      api.ProtocolHTTP,
					Mode:          api.PortModeIngress,
				}}
			default:
				t.Fatalf("containerFingerprint has a new field %q without a mutation case in this test. "+
					"Add a case here and make sure Equal() compares it.", field.Name)
			}

			assert.False(t, base.Equal(mutated),
				"changing %q must flip Equal to false. Update containerFingerprint.Equal to compare it.",
				field.Name)
		})
	}
}

// fakeCaddyAdmin is a fake Caddy admin API served over a Unix socket. It accepts any Caddyfile on /adapt and
// records the Caddyfiles that get loaded via /load.
type fakeCaddyAdmin struct {
	socketPath string
	server     *http.Server

	mu sync.Mutex
	// lastAdapted is the Caddyfile received by the most recent /adapt request.
	lastAdapted string
	// loaded contains the Caddyfile for every successful /load request (captured from the preceding /adapt
	// request since CaddyAdminClient.Load posts the adapted JSON config to /load).
	loaded []string
}

// newTestSocketPath returns a socket path in a dedicated short temp directory. Unix socket paths are limited to
// ~104 characters on macOS and t.TempDir() paths embedding the test name can exceed that.
func newTestSocketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "uc-caddy-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "admin.sock")
}

func startFakeCaddyAdmin(t *testing.T, socketPath string) *fakeCaddyAdmin {
	t.Helper()

	f := &fakeCaddyAdmin{socketPath: socketPath}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /adapt", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		f.mu.Lock()
		f.lastAdapted = string(body)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result": {}}`))
	})
	mux.HandleFunc("POST /load", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.loaded = append(f.loaded, f.lastAdapted)
		f.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	})
	f.server = &http.Server{Handler: mux}

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	go f.server.Serve(listener) //nolint:errcheck // Serve always returns a non-nil error on Close.
	t.Cleanup(f.stop)

	return f
}

// stop shuts down the fake admin API and removes the socket file to simulate Caddy not running.
func (f *fakeCaddyAdmin) stop() {
	_ = f.server.Close()
	_ = os.Remove(f.socketPath)
}

func (f *fakeCaddyAdmin) loadedCaddyfiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.loaded...)
}

// fakeLocalContainerLister is a fake Docker daemon view of the caddy service on this machine.
type fakeLocalContainerLister struct {
	// caddyDeployed reports a caddy service container (in any state) on this machine.
	caddyDeployed bool
	err           error
}

func (f *fakeLocalContainerLister) ContainerList(
	_ context.Context, options container.ListOptions,
) ([]container.Summary, error) {
	if f.err != nil {
		return nil, f.err
	}
	caddyFilter := options.All &&
		options.Filters.ExactMatch("label", api.LabelServiceName+"="+CaddyServiceName)
	if f.caddyDeployed && caddyFilter {
		return []container.Summary{{ID: "caddy-container"}}, nil
	}
	return nil, nil
}

// newTestController creates a Controller wired to the fake Caddy admin socket with the Caddyfile stored in dir.
// The fake Docker daemon reports a caddy service container on this machine.
func newTestController(dir, socketPath string) *Controller {
	client := NewCaddyAdminClient(socketPath)
	c := &Controller{
		machineID:     "test-machine-id",
		caddyfilePath: filepath.Join(dir, "Caddyfile"),
		client:        client,
		docker:        &fakeLocalContainerLister{caddyDeployed: true},
	}
	c.log = slog.Default()
	c.generator = NewCaddyfileGenerator(c.machineID, "test-machine", client, c.log)
	return c
}

// TestControllerCaddyRestartPreservesCustomConfig reproduces github.com/psviderski/uncloud/issues/412: a config
// regeneration triggered while the caddy container is restarting must not lose the user-defined global config
// deployed with the caddy service (uc caddy deploy --caddyfile), neither in the Caddyfile on disk that Caddy boots
// from nor in the config loaded via the admin API after Caddy is back.
func TestControllerCaddyRestartPreservesCustomConfig(t *testing.T) {
	t.Parallel()

	const globalConfig = `{
	storage redis {
		host my-redis
	}
}`
	dir := t.TempDir()
	socketPath := newTestSocketPath(t)
	admin := startFakeCaddyAdmin(t, socketPath)
	c := newTestController(dir, socketPath)
	ctx := context.Background()

	caddyRecord := newContainerRecordWithCaddyConfig("caddy", "10.210.0.1", globalConfig, "test-machine-id", time.Now())
	appRecord := newContainerRecordWithPorts("app", "10.210.0.2", []string{"app.example.com:8080/http"}, "test-machine-id")

	// 1. Initial generation with a healthy caddy container: the global config is loaded and written to disk.
	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{caddyRecord, appRecord})

	caddyfile, err := os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "storage redis", "initial Caddyfile must contain the global config")
	require.Len(t, admin.loadedCaddyfiles(), 1)
	assert.Contains(t, admin.loadedCaddyfiles()[0], "storage redis", "loaded config must contain the global config")
	assert.NotNil(t, c.lastFingerprint)

	// 2. Caddy container is restarting: the container is not running and the admin socket is not accepting
	// connections. The Caddyfile on disk must be preserved as Caddy boots from it, and the fingerprint cache
	// must be invalidated as the previously loaded config no longer determines what Caddy will run.
	admin.stop()
	stoppedCaddyRecord := caddyRecord
	stoppedState := *caddyRecord.Container.State
	stoppedState.Running = false
	stoppedCaddyRecord.Container.State = &stoppedState

	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{stoppedCaddyRecord, appRecord})

	caddyfile, err = os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "storage redis",
		"Caddyfile must not lose the global config while Caddy is restarting as Caddy boots from it")
	assert.Nil(t, c.lastFingerprint, "fingerprint cache must be invalidated when Caddy is unavailable")

	// 3. Caddy is back with the same container set: the regeneration must not be skipped as unchanged, and the
	// loaded config must contain the global config again.
	admin = startFakeCaddyAdmin(t, socketPath)

	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{caddyRecord, appRecord})

	caddyfile, err = os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "storage redis")
	assert.NotNil(t, c.lastFingerprint, "regeneration must not be skipped after Caddy restart")
	// Caddy booted from the preserved Caddyfile which matches the regenerated config, so a load is not required.
	// But if one happened, it must contain the global config.
	for _, loaded := range admin.loadedCaddyfiles() {
		assert.Contains(t, loaded, "storage redis")
	}
}

// TestControllerCaddyRestartLoadsCustomConfigWhileUnhealthy verifies that a regeneration triggered while the caddy
// container hasn't become healthy yet (e.g. it's starting up and the admin socket is already accepting connections)
// doesn't load a config without the user-defined global config into the running Caddy.
func TestControllerCaddyRestartLoadsCustomConfigWhileUnhealthy(t *testing.T) {
	t.Parallel()

	const globalConfig = `{
	storage redis {
		host my-redis
	}
}`
	dir := t.TempDir()
	socketPath := newTestSocketPath(t)
	admin := startFakeCaddyAdmin(t, socketPath)
	c := newTestController(dir, socketPath)
	ctx := context.Background()

	caddyRecord := newContainerRecordWithCaddyConfig("caddy", "10.210.0.1", globalConfig, "test-machine-id", time.Now())
	appRecord := newContainerRecordWithPorts("app", "10.210.0.2", []string{"web.example.com:8080/http"}, "test-machine-id")
	stoppedAppRecord := newContainerRecordWithPorts(
		"stopped-app", "10.210.0.3", []string{"stopped.example.com:8080/http"}, "test-machine-id")
	stoppedAppRecord.Container.State = &container.State{Running: false}

	// The caddy container is restarting but the admin socket is already available.
	restartingCaddyRecord := caddyRecord
	restartingState := *caddyRecord.Container.State
	restartingState.Restarting = true
	restartingCaddyRecord.Container.State = &restartingState

	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{restartingCaddyRecord, appRecord, stoppedAppRecord})

	loaded := admin.loadedCaddyfiles()
	require.Len(t, loaded, 1)
	assert.Contains(t, loaded[0], "storage redis",
		"config loaded while the caddy container is unhealthy must still contain the global config from its spec")
	assert.Contains(t, loaded[0], "web.example.com")
	assert.NotContains(t, loaded[0], "stopped.example.com",
		"ports of unhealthy containers must not generate sites")
}

// TestControllerCaddyNotDeployedOverwritesCaddyfile verifies that the Caddyfile is regenerated without
// user-defined configs when the caddy service is removed from the machine: the existing Caddyfile is only
// preserved for a caddy container that is restarting, not when Caddy is gone for good.
func TestControllerCaddyNotDeployedOverwritesCaddyfile(t *testing.T) {
	t.Parallel()

	const globalConfig = `{
	storage redis {
		host my-redis
	}
}`
	dir := t.TempDir()
	socketPath := newTestSocketPath(t)
	admin := startFakeCaddyAdmin(t, socketPath)
	c := newTestController(dir, socketPath)
	ctx := context.Background()

	caddyRecord := newContainerRecordWithCaddyConfig("caddy", "10.210.0.1", globalConfig, "test-machine-id", time.Now())
	appRecord := newContainerRecordWithPorts("app", "10.210.0.2", []string{"removed.example.com:8080/http"}, "test-machine-id")

	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{caddyRecord, appRecord})

	caddyfile, err := os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	require.Contains(t, string(caddyfile), "storage redis")

	// The caddy service is removed from the machine: no caddy container in Docker, no caddy container records
	// and the admin socket is gone.
	admin.stop()
	c.docker = &fakeLocalContainerLister{caddyDeployed: false}
	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{appRecord})

	caddyfile, err = os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.NotContains(t, string(caddyfile), "storage redis",
		"Caddyfile must be regenerated without user-defined configs when the caddy service is removed")
	assert.Contains(t, string(caddyfile), "# NOTE: User-defined configs for services were skipped")
	assert.Nil(t, c.lastFingerprint)
}

// TestControllerCaddyUnavailableTrustsDockerNotStore verifies that the decision to keep the existing Caddyfile
// while Caddy is down is based on the Docker daemon, not on the cluster store: while the daemon is starting up, the
// store may return a partial view of the machine's containers that lacks the local caddy container record.
func TestControllerCaddyUnavailableTrustsDockerNotStore(t *testing.T) {
	t.Parallel()

	const globalConfig = `{
	storage redis {
		host my-redis
	}
}`
	dir := t.TempDir()
	socketPath := newTestSocketPath(t)
	admin := startFakeCaddyAdmin(t, socketPath)
	c := newTestController(dir, socketPath)
	ctx := context.Background()

	caddyRecord := newContainerRecordWithCaddyConfig("caddy", "10.210.0.1", globalConfig, "test-machine-id", time.Now())
	appRecord := newContainerRecordWithPorts("app", "10.210.0.2", []string{"partial.example.com:8080/http"}, "test-machine-id")

	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{caddyRecord, appRecord})

	caddyfile, err := os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	require.Contains(t, string(caddyfile), "storage redis")

	// Caddy is down and the store returns a partial view without the local caddy record, but Docker still
	// reports the caddy container on this machine.
	admin.stop()
	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{appRecord})

	caddyfile, err = os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "storage redis",
		"Caddyfile must be kept when Docker reports a caddy container even if the store view lacks it")

	// If Docker can't be queried, keeping the existing Caddyfile is the safe default.
	c.docker = &fakeLocalContainerLister{err: errors.New("docker unavailable")}
	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{appRecord})

	caddyfile, err = os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "storage redis",
		"Caddyfile must be kept when Docker can't be queried")

	// A regeneration caught by the daemon shutting down (cancelled context) must not overwrite the Caddyfile
	// even if Docker reports no caddy container.
	c.docker = &fakeLocalContainerLister{caddyDeployed: false}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	c.generateAndLoadCaddyfile(cancelledCtx, []store.ContainerRecord{appRecord})

	caddyfile, err = os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "storage redis",
		"Caddyfile must be kept when the context is cancelled")
}

// TestControllerCaddyUnavailableBootstrap verifies that a bootstrap Caddyfile without user-defined configs is
// written when Caddy is not running and no Caddyfile exists on the machine yet.
func TestControllerCaddyUnavailableBootstrap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// No fake admin server is started: Caddy is not running on this machine.
	c := newTestController(dir, newTestSocketPath(t))
	ctx := context.Background()

	caddyRecord := newContainerRecordWithCaddyConfig("caddy", "10.210.0.1", "{\n\tglobal directive\n}", "test-machine-id", time.Now())
	appRecord := newContainerRecordWithPorts("app", "10.210.0.2", []string{"bootstrap.example.com:8080/http"}, "test-machine-id")

	c.generateAndLoadCaddyfile(ctx, []store.ContainerRecord{caddyRecord, appRecord})

	caddyfile, err := os.ReadFile(c.caddyfilePath)
	require.NoError(t, err)
	assert.Contains(t, string(caddyfile), "bootstrap.example.com", "bootstrap Caddyfile must contain generated sites")
	assert.NotContains(t, string(caddyfile), "global directive",
		"bootstrap Caddyfile must not contain user-defined configs as they can't be validated")
	assert.Nil(t, c.lastFingerprint)
}
