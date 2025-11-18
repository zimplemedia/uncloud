package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/psviderski/uncloud/pkg/api"
)

// Operation represents a single atomic operation in a deployment process.
// Operations can be composed to form complex deployment strategies.
type Operation interface {
	// Execute performs the operation using the provided client.
	// TODO: Encapsulate the client in the operation as otherwise it gives an impression that different clients
	//  can be provided. But in reality, the operation is tightly coupled with the client that was used to create it.
	Execute(ctx context.Context, cli Client) error
	// Format returns a human-readable representation of the operation.
	// TODO: get rid of the resolver and assign the required names for formatting in the operation itself.
	Format(resolver NameResolver) string
	String() string
}

// NameResolver resolves machine and container IDs to their names.
type NameResolver interface {
	MachineName(machineID string) string
	ContainerName(containerID string) string
}

// TODO: pass api.ServiceContainer to operations to simplify operation formatting in the plan.

// RunContainerOperation creates and starts a new container on a specific machine.
type RunContainerOperation struct {
	ServiceID string
	Spec      api.ServiceSpec
	MachineID string
}

func (o *RunContainerOperation) Execute(ctx context.Context, cli Client) error {
	resp, err := cli.CreateContainer(ctx, o.ServiceID, o.Spec, o.MachineID)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err = cli.StartContainer(ctx, o.ServiceID, resp.ID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	// Wait for the container to become healthy before proceeding to the next operation.
	// This ensures zero-downtime deployments by keeping old containers running until new ones are ready.
	if err := o.waitForContainerHealthy(ctx, cli, resp.ID); err != nil {
		return fmt.Errorf("wait for container healthy: %w", err)
	}

	return nil
}

func (o *RunContainerOperation) Format(resolver NameResolver) string {
	machineName := resolver.MachineName(o.MachineID)
	return fmt.Sprintf("%s: Run container [image=%s]", machineName, o.Spec.Container.Image)
}

func (o *RunContainerOperation) String() string {
	return fmt.Sprintf("RunContainerOperation[machine_id=%s service_id=%s image=%s]",
		o.MachineID, o.ServiceID, o.Spec.Container.Image)
}

// waitForContainerHealthy waits for a container to pass its healthcheck before returning.
// For containers without a healthcheck, it waits briefly to ensure the container doesn't crash immediately.
// This enables zero-downtime deployments by ensuring new containers are ready before old ones are stopped.
func (o *RunContainerOperation) waitForContainerHealthy(
	ctx context.Context,
	cli Client,
	containerID string,
) error {
	const (
		pollInterval         = 2 * time.Second
		maxWaitTime          = 90 * time.Second
		noHealthcheckWaitFor = 5 * time.Second
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	timeout := time.After(maxWaitTime)
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-timeout:
			return fmt.Errorf(
				"timeout waiting for container %s to become healthy after %s",
				containerID[:12],
				maxWaitTime,
			)

		case <-ticker.C:
			// Inspect container to check its current state and health
			mc, err := cli.InspectContainer(ctx, o.ServiceID, containerID)
			if err != nil {
				return fmt.Errorf("inspect container: %w", err)
			}

			ctr := mc.Container

			// Check if container is still running
			if !ctr.State.Running {
				return fmt.Errorf(
					"container %s exited during healthcheck wait (status: %s, exit code: %d)",
					containerID[:12],
					ctr.State.Status,
					ctr.State.ExitCode,
				)
			}

			// Handle containers without healthcheck
			if ctr.State.Health == nil {
				// Wait a minimum time to catch immediate crashes
				if time.Since(startTime) < noHealthcheckWaitFor {
					continue
				}

				// Verify container is still running after the wait period
				mc, err = cli.InspectContainer(ctx, o.ServiceID, containerID)
				if err != nil {
					return fmt.Errorf("inspect container after no-healthcheck wait: %w", err)
				}

				if !mc.Container.State.Running {
					return fmt.Errorf(
						"container %s exited shortly after start (status: %s, exit code: %d)",
						containerID[:12],
						mc.Container.State.Status,
						mc.Container.State.ExitCode,
					)
				}

				// No healthcheck, container is running - consider it ready
				return nil
			}

			// Use the existing Healthy() method to check healthcheck status
			if ctr.Healthy() {
				// Container is healthy and ready to serve traffic
				return nil
			}

			// Check if explicitly unhealthy (not just starting)
			if ctr.State.Health.Status == container.Unhealthy {
				// Get last healthcheck log for error message
				lastLog := "no healthcheck logs available"
				if len(ctr.State.Health.Log) > 0 {
					lastLog = strings.TrimSpace(ctr.State.Health.Log[len(ctr.State.Health.Log)-1].Output)
					if len(lastLog) > 200 {
						lastLog = lastLog[:200] + "..."
					}
				}
				return fmt.Errorf(
					"container %s became unhealthy: %s",
					containerID[:12],
					lastLog,
				)
			}

			// Still starting or unknown status - continue waiting
			continue
		}
	}
}

// StopContainerOperation stops a container on a specific machine.
type StopContainerOperation struct {
	ServiceID   string
	ContainerID string
	MachineID   string
}

func (o *StopContainerOperation) Execute(ctx context.Context, cli Client) error {
	if err := cli.StopContainer(ctx, o.ServiceID, o.ContainerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

func (o *StopContainerOperation) Format(resolver NameResolver) string {
	machineName := resolver.MachineName(o.MachineID)
	return fmt.Sprintf("%s: Stop container [id=%s name=%s]", machineName,
		o.ContainerID[:12], resolver.ContainerName(o.ContainerID))
}

func (o *StopContainerOperation) String() string {
	return fmt.Sprintf("StopContainerOperation[machine_id=%s service_id=%s container_id=%s]",
		o.MachineID, o.ServiceID, o.ContainerID)
}

// RemoveContainerOperation stops and removes a container from a specific machine.
type RemoveContainerOperation struct {
	MachineID string
	Container api.ServiceContainer
}

func (o *RemoveContainerOperation) Execute(ctx context.Context, cli Client) error {
	if err := cli.StopContainer(ctx, o.Container.ServiceID(), o.Container.ID, container.StopOptions{}); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	if err := cli.RemoveContainer(ctx, o.Container.ServiceID(), o.Container.ID, container.RemoveOptions{
		// Remove anonymous volumes created by the container.
		RemoveVolumes: true,
	}); err != nil {
		return fmt.Errorf("remove container: %w", err)
	}

	return nil
}

func (o *RemoveContainerOperation) Format(resolver NameResolver) string {
	machineName := resolver.MachineName(o.MachineID)
	return fmt.Sprintf("%s: Remove container [id=%s image=%s]",
		machineName, o.Container.ShortID(), o.Container.Config.Image)
}

func (o *RemoveContainerOperation) String() string {
	return fmt.Sprintf("RemoveContainerOperation[machine_id=%s service_id=%s container_id=%s]",
		o.MachineID, o.Container.ServiceID(), o.Container.ID)
}

// CreateVolumeOperation creates a volume on a specific machine.
type CreateVolumeOperation struct {
	VolumeSpec api.VolumeSpec
	MachineID  string
	// MachineName is used for formatting the operation output only.
	MachineName string
}

func (o *CreateVolumeOperation) Execute(ctx context.Context, cli Client) error {
	if o.VolumeSpec.Type != api.VolumeTypeVolume {
		return fmt.Errorf("invalid volume type: '%s', expected '%s'", o.VolumeSpec.Type, api.VolumeTypeVolume)
	}

	opts := volume.CreateOptions{
		Name: o.VolumeSpec.DockerVolumeName(),
	}
	if o.VolumeSpec.VolumeOptions != nil {
		if o.VolumeSpec.VolumeOptions.Driver != nil {
			opts.Driver = o.VolumeSpec.VolumeOptions.Driver.Name
			opts.DriverOpts = o.VolumeSpec.VolumeOptions.Driver.Options
		}
		opts.Labels = o.VolumeSpec.VolumeOptions.Labels
	}

	if _, err := cli.CreateVolume(ctx, o.MachineID, opts); err != nil {
		return fmt.Errorf("create volume: %w", err)
	}

	return nil
}

func (o *CreateVolumeOperation) Format(_ NameResolver) string {
	return fmt.Sprintf("%s: Create volume [name=%s]", o.MachineName, o.VolumeSpec.DockerVolumeName())
}

func (o *CreateVolumeOperation) String() string {
	return fmt.Sprintf("CreateVolumeOperation[machine_id=%s volume=%s]",
		o.MachineID, o.VolumeSpec.DockerVolumeName())
}

// SequenceOperation is a composite operation that executes a sequence of operations in order.
type SequenceOperation struct {
	Operations []Operation
}

func (o *SequenceOperation) Execute(ctx context.Context, cli Client) error {
	for _, op := range o.Operations {
		if err := op.Execute(ctx, cli); err != nil {
			return err
		}
	}
	return nil
}

func (o *SequenceOperation) Format(resolver NameResolver) string {
	ops := make([]string, len(o.Operations))
	for i, op := range o.Operations {
		ops[i] = "- " + op.Format(resolver)
	}

	return strings.Join(ops, "\n")
}

func (o *SequenceOperation) String() string {
	ops := make([]string, len(o.Operations))
	for i, op := range o.Operations {
		ops[i] = op.String()
	}

	return fmt.Sprintf("SequenceOperation[%s]", strings.Join(ops, ", "))
}
