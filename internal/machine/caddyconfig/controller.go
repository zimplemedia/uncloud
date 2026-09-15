package caddyconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/fs"
	"github.com/psviderski/uncloud/internal/machine/docker"
	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/psviderski/uncloud/pkg/api"
)

const (
	CaddyServiceName = "caddy"
	CaddyGroup       = "uncloud"
	VerifyPath       = "/.uncloud-verify"
)

// LocalContainerLister lists service containers on this machine directly from the Docker daemon.
// It's the source of truth for whether the caddy service is deployed on the machine: the cluster store may return
// a partial view of the machine's containers while the daemon (and its corrosion service) is starting up.
type LocalContainerLister interface {
	ListServiceContainers(
		ctx context.Context, serviceNameOrID string, opts container.ListOptions,
	) (docker.ListServiceContainersResult, error)
}

// Controller monitors container changes in the cluster store and generates a configuration file for Caddy reverse
// proxy. The generated configuration allows Caddy to route external traffic to service containers across the internal
// network.
type Controller struct {
	machineID     string
	caddyfilePath string
	generator     *CaddyfileGenerator
	client        *CaddyAdminClient
	store         *store.Store
	docker        LocalContainerLister
	log           *slog.Logger
	// lastFingerprint caches the fingerprint of the containers used to generate the latest successfully loaded
	// Caddyfile. nil means it hasn't been loaded yet or the last load failed.
	lastFingerprint []containerFingerprint
	// lastCaddyfile caches the last generated Caddyfile.
	lastCaddyfile string
}

// containerFingerprint is the subset of container data that the Caddyfile generator depends on.
// Comparing fingerprints lets the controller skip no-op regenerations.
type containerFingerprint struct {
	ID          string
	IP          netip.Addr
	Healthy     bool
	Ports       []api.PortSpec
	CaddyConfig string
}

// Equal returns whether two fingerprints describe the same container input to the Caddyfile generator.
func (f containerFingerprint) Equal(other containerFingerprint) bool {
	return f.ID == other.ID &&
		f.IP == other.IP &&
		f.Healthy == other.Healthy &&
		api.PortsEqual(f.Ports, other.Ports) &&
		f.CaddyConfig == other.CaddyConfig
}

func NewController(
	machineID, configDir, adminSock string, store *store.Store, docker LocalContainerLister,
) (*Controller, error) {
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		return nil, fmt.Errorf("create directory for Caddy configuration '%s': %w", configDir, err)
	}
	if err := fs.Chown(configDir, "", CaddyGroup); err != nil {
		return nil, fmt.Errorf("change owner of directory for Caddy configuration '%s': %w", configDir, err)
	}

	log := slog.With("component", "caddy-controller")
	client := NewCaddyAdminClient(adminSock)

	// generator is initialised by Run() once the machine name is resolved from the store.
	return &Controller{
		machineID:     machineID,
		caddyfilePath: filepath.Join(configDir, "Caddyfile"),
		client:        client,
		store:         store,
		docker:        docker,
		log:           log,
	}, nil
}

func (c *Controller) Run(ctx context.Context) error {
	// Default the machine name to the machine ID so the Caddyfile header still carries a stable identifier if
	// the store lookup fails.
	machineName := c.machineID
	if m, err := c.store.GetMachine(ctx, c.machineID); err != nil {
		c.log.Error("Failed to get machine from store, Caddy configuration will use machine ID as the name.",
			"machine_id", c.machineID, "err", err)
	} else {
		machineName = m.Name
	}
	c.generator = NewCaddyfileGenerator(c.machineID, machineName, c.client, c.log)

	containers, changes, err := c.store.SubscribeContainers(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to container changes: %w", err)
	}
	c.log.Info("Subscribed to container changes in the cluster to generate Caddy configuration.")

	containers = filterHookContainers(containers)
	c.generateAndLoadCaddyfile(ctx, containers)

	// TODO: left for backward compatibility, remove later.
	if err = c.generateJSONConfig(filterHealthyContainers(containers)); err != nil {
		c.log.Error("Failed to generate Caddy JSON configuration to disk.", "err", err)
	}

	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return fmt.Errorf("subscription to container changes in cluster store failed")
			}
			c.log.Debug("Cluster containers changed, regenerating Caddy configuration.")

			containers, err = c.store.ListContainers(ctx, store.ListOptions{})
			if err != nil {
				c.log.Error("Failed to list containers.", "err", err)
				continue
			}
			containers = filterHookContainers(containers)
			c.generateAndLoadCaddyfile(ctx, containers)

			// TODO: left for backward compatibility, remove later.
			if err = c.generateJSONConfig(filterHealthyContainers(containers)); err != nil {
				c.log.Error("Failed to generate Caddy JSON configuration to disk.", "err", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// filterHookContainers filters out hook containers which must never affect the generated Caddy configuration.
func filterHookContainers(containers []store.ContainerRecord) []store.ContainerRecord {
	filtered := make([]store.ContainerRecord, 0, len(containers))
	for _, cr := range containers {
		if cr.Container.IsHook() {
			continue
		}
		filtered = append(filtered, cr)
	}
	return filtered
}

// filterHealthyContainers filters out unhealthy containers.
// TODO: Filters out containers from this machine that are likely unavailable. The availability can be determined
// by the cluster membership state of the machine that the container is running on. Implement machine membership
// check using Corrossion Admin client.
func filterHealthyContainers(containers []store.ContainerRecord) []store.ContainerRecord {
	healthy := make([]store.ContainerRecord, 0, len(containers))
	for _, cr := range containers {
		if cr.Container.Healthy() {
			healthy = append(healthy, cr)
		}
	}
	return healthy
}

// generateAndLoadCaddyfile regenerates the Caddyfile from the given containers and loads it into the local Caddy
// if available.
func (c *Controller) generateAndLoadCaddyfile(ctx context.Context, containers []store.ContainerRecord) {
	if !c.client.IsAvailable() {
		// Caddy is not running on this machine (or not accessible via the admin socket). When it starts, it boots
		// from the Caddyfile on disk, so the config loaded via the admin API last time no longer determines what
		// Caddy will run. Invalidate the fingerprint cache to force a full regeneration and load on the next
		// container change once Caddy is back.
		c.lastFingerprint = nil

		if c.caddyDeployedLocally(ctx) {
			if _, err := os.Stat(c.caddyfilePath); err == nil {
				// The caddy service is deployed on this machine, so Caddy is likely restarting or starting up and
				// will boot from the existing Caddyfile. A Caddyfile generated now would not include user-defined
				// configs (they can't be validated without a running Caddy). Overwriting the existing Caddyfile
				// with it would make the restarting Caddy boot without user-defined configs, e.g. a global options
				// block from the caddy service spec that configures certificate storage. The existing Caddyfile was
				// successfully loaded into Caddy before (hence valid), so keep it for Caddy to boot from.
				c.log.Debug("Caddy is not running on this machine, keeping the existing Caddyfile for it to boot from.",
					"path", c.caddyfilePath)
				return
			}
		}

		// Caddy is not deployed on this machine (or no Caddyfile exists yet). Write a config without user-defined
		// configs so that when Caddy is deployed on this machine, it can pick it up.
		caddyfile, err := c.generator.Generate(ctx, containers, false)
		if err != nil {
			c.log.Error("Failed to generate Caddyfile configuration.", "err", err)
			return
		}
		if err = c.writeCaddyfileIfChanged(caddyfile); err != nil {
			c.log.Error("Failed to write Caddyfile to disk.", "err", err)
			return
		}
		c.log.Debug("Caddy is not running on this machine, wrote a Caddyfile without user-defined configs.",
			"path", c.caddyfilePath)
		return
	}

	// Skip regeneration when the containers haven't changed since the last successful load.
	fingerprint := fingerprintContainers(containers)
	if slices.EqualFunc(fingerprint, c.lastFingerprint, containerFingerprint.Equal) {
		c.log.Debug("Caddy configuration is unchanged.", "path", c.caddyfilePath)
		return
	}

	caddyfile, err := c.generator.Generate(ctx, containers, true)
	if err != nil {
		c.log.Error("Failed to generate Caddyfile configuration.", "err", err)
		return
	}

	// Skip the load if the generated config matches the last written Caddyfile. Caddy already runs it: it was
	// either loaded via the admin API or Caddy booted from it on disk after a restart.
	if caddyfileBody(caddyfile) == caddyfileBody(c.lastCaddyfile) {
		c.lastFingerprint = fingerprint
		c.log.Debug("Generated Caddy configuration matches the running one, skipping load.", "path", c.caddyfilePath)
		return
	}

	// Caddy is available, try to load the config which may fail if the config is invalid. Generally, a config can
	// pass the adaptation/validation step but still fail to load, for example, if it references resources that are
	// not available.
	if err = c.client.Load(ctx, caddyfile); err != nil {
		c.log.Error("Failed to load new Caddy configuration into local Caddy instance.",
			"err", err, "path", c.caddyfilePath)
		// Mark the cache stale so the next container change retries the load even if the container set is unchanged.
		c.lastFingerprint = nil
		// Don't write invalid config to disk.
		return
	}
	c.lastFingerprint = fingerprint

	// Config loaded successfully, now write it to disk.
	if err = c.writeCaddyfileIfChanged(caddyfile); err != nil {
		c.log.Error("Failed to write Caddyfile to disk after successful load.", "err", err)
		// Config is already loaded in Caddy, so this is not critical. The next regeneration retries the disk write.
		return
	}

	c.log.Info("New Caddy configuration loaded into local Caddy instance.", "path", c.caddyfilePath)
}

// caddyDeployedLocally returns whether a container of the caddy service exists on this machine, regardless of its
// state: a restarting or just created caddy container still means Caddy is deployed here. The Docker daemon is
// queried directly rather than the cluster store because the store may return a partial view of the machine's
// containers while the daemon is starting up. If Docker can't be queried, the caddy service is assumed to be
// deployed as keeping the existing Caddyfile is the safe choice.
func (c *Controller) caddyDeployedLocally(ctx context.Context) bool {
	if c.docker == nil {
		return true
	}

	result, err := c.docker.ListServiceContainers(ctx, CaddyServiceName, container.ListOptions{All: true})
	if err != nil {
		c.log.Warn("Failed to list caddy service containers from Docker, assuming the caddy service is deployed "+
			"on this machine.", "err", err)
		return true
	}

	return len(result.Containers) > 0
}

// fingerprintContainers returns a fingerprint of containers that the Caddyfile generator depends on.
func fingerprintContainers(containers []store.ContainerRecord) []containerFingerprint {
	fingerprints := make([]containerFingerprint, len(containers))
	for i, cr := range containers {
		// Ignore ports parsing error as not much we can do about it. The generator just logs them and continues.
		ports, _ := cr.Container.ServicePorts()
		fingerprints[i] = containerFingerprint{
			ID:          cr.Container.ID,
			IP:          cr.Container.UncloudNetworkIP(),
			Healthy:     cr.Container.Healthy(),
			Ports:       ports,
			CaddyConfig: cr.Container.ServiceSpec.CaddyConfig(),
		}
	}
	slices.SortFunc(fingerprints, func(a, b containerFingerprint) int {
		return strings.Compare(a.ID, b.ID)
	})

	return fingerprints
}

// writeCaddyfileIfChanged writes the Caddyfile content to disk with proper permissions only if its body differs
// from the last successfully written content. The first line of the Caddyfile carries a generation timestamp that
// changes on every regeneration, so it's excluded from the comparison to avoid redundant writes.
func (c *Controller) writeCaddyfileIfChanged(caddyfile string) error {
	if caddyfileBody(caddyfile) == caddyfileBody(c.lastCaddyfile) {
		return nil
	}

	if err := os.WriteFile(c.caddyfilePath, []byte(caddyfile), 0o640); err != nil {
		return fmt.Errorf("write Caddyfile to file '%s': %w", c.caddyfilePath, err)
	}
	if err := fs.Chown(c.caddyfilePath, "", CaddyGroup); err != nil {
		return fmt.Errorf("change owner of Caddyfile '%s': %w", c.caddyfilePath, err)
	}
	c.lastCaddyfile = caddyfile

	return nil
}

// caddyfileBody returns the Caddyfile content without its first line, which carries a generation timestamp that
// rotates on every regeneration.
func caddyfileBody(caddyfile string) string {
	if _, after, ok := strings.Cut(caddyfile, "\n"); ok {
		return after
	}
	return caddyfile
}

func (c *Controller) generateJSONConfig(containers []store.ContainerRecord) error {
	serviceContainers := make([]api.ServiceContainer, len(containers))
	for i, cr := range containers {
		serviceContainers[i] = cr.Container
	}

	config, err := GenerateJSONConfig(serviceContainers, c.machineID)
	if err != nil {
		return err
	}

	configBytes, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal Caddy configuration: %w", err)
	}
	configPath := filepath.Join(filepath.Dir(c.caddyfilePath), "caddy.json")

	if err = os.WriteFile(configPath, configBytes, 0o640); err != nil {
		return fmt.Errorf("write Caddy configuration to file '%s': %w", configPath, err)
	}
	if err = fs.Chown(configPath, "", CaddyGroup); err != nil {
		return fmt.Errorf("change owner of Caddy configuration file '%s': %w", configPath, err)
	}

	return nil
}
