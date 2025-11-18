# Release v0.1.1

## Fixes

### Critical: Prevent stale container upstreams in Caddy config

**Problem:** When a machine is removed from the cluster using `uc machine rm`, the Caddy reverse proxy continues to route traffic to containers that were running on the removed machine, causing 502 errors.

**Root Cause:** Container records remained in the Corrosion database after machine removal, so Caddy config generator still included them as upstreams.

**Solution:** Implemented two complementary fixes:

1. **RemoveMachine cleanup** - When removing a machine, automatically delete all its container records from the cluster store
2. **Caddy controller filtering** - Filter out containers belonging to machines that no longer exist in the cluster before generating Caddy config

**Changes:**
- `internal/machine/cluster/cluster.go`: Added container cleanup in `RemoveMachine()`
- `internal/machine/store/container.go`: Added `DeleteContainersByMachine()` method
- `internal/machine/caddyconfig/controller.go`: Enhanced `filterHealthyContainers()` to check machine existence
- `internal/cli/machine.go`: Updated install script URL to point to zimplemedia/uncloud fork
- `scripts/install.sh`: Updated GitHub URLs to point to zimplemedia/uncloud fork

**Impact:** Eliminates 502 errors after machine removal, ensures Caddy only routes to healthy upstreams on active machines.

## Breaking Changes

None

## Upgrade Instructions

### For new deployments:
```bash
# Will automatically download v0.1.1
uc machine init user@host
uc machine add user@host
```

### For existing machines:
```bash
# Download and install the new uc CLI locally
curl -fsSL https://github.com/zimplemedia/uncloud/releases/download/v0.1.1/uc_darwin_arm64.tar.gz | tar -xz
sudo install uc /usr/local/bin/uc

# Update uncloudd on each machine
uc machine ls  # Get list of machines
ssh root@<machine> 'curl -fsSL https://github.com/zimplemedia/uncloud/releases/download/v0.1.1/uncloudd_linux_amd64.tar.gz | tar -xz && install uncloudd /usr/local/bin/uncloudd && systemctl restart uncloud'
```
