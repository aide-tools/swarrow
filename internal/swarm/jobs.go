package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

// ErrJobBusy means the current execution lacks evidence that all tasks finished.
var ErrJobBusy = errors.New("previous job execution is not terminal")

type jobClient interface {
	serviceClient
	observationClient
}

// Jobs changes the image of existing single-task jobs and observes their results.
// Commands, runtime settings and restart policies remain operator-owned.
type Jobs struct {
	client   jobClient
	interval time.Duration
}

// NewJobs creates a job runner with a positive observation interval.
func NewJobs(client jobClient, interval time.Duration) (*Jobs, error) {
	if interval <= 0 {
		return nil, ErrInvalidPollInterval
	}
	return &Jobs{client: client, interval: interval}, nil
}

// Run observes a matching image, including a previously failed execution. A
// different image is applied only after the current execution has finished.
// Cancellation stops observation without stopping or restarting the job.
// Callers must authorise and serialise requests before calling Run.
func (jobs *Jobs) Run(ctx context.Context, name, repository, digest string) (Result, Observation, error) {
	image, err := buildImageReference(repository, digest)
	if err != nil {
		return Result{}, Observation{}, err
	}
	service, err := jobs.inspect(ctx, name)
	if err != nil {
		return Result{}, Observation{}, err
	}
	result := Result{ServiceID: service.ID, Image: image, InspectedVersion: service.Version.Index}
	iteration := service.JobStatus.JobIteration.Index
	if sameImage(service.Spec.TaskTemplate.ContainerSpec.Image, image) {
		result.Action = ActionNoChange
	} else {
		prior, stable, err := jobs.snapshot(ctx, service)
		if err != nil {
			return result, Observation{}, err
		}
		if !stable {
			return result, Observation{Conclusion: ConclusionSuperseded}, ErrConflict
		}
		if prior.Conclusion != ConclusionCompleted && prior.Conclusion != ConclusionFailed {
			return result, prior, ErrJobBusy
		}
		if err := ctx.Err(); err != nil {
			return result, Observation{}, err
		}
		spec := service.Spec
		container := *spec.TaskTemplate.ContainerSpec
		container.Image = image
		spec.TaskTemplate.ContainerSpec = &container
		updated, err := jobs.client.ServiceUpdate(ctx, service.ID, clienttypes.ServiceUpdateOptions{
			Version: service.Version, Spec: spec,
			RegistryAuthFrom: swarmtypes.RegistryAuthFromSpec, QueryRegistry: false,
		})
		if err != nil {
			result.Action = ActionIndeterminate
			if cerrdefs.IsConflict(err) {
				result.Action = ActionRejected
				return result, Observation{Conclusion: ConclusionSuperseded}, fmt.Errorf("%w: job changed before update", ErrConflict)
			}
			if isDefinitiveRejection(err) {
				result.Action = ActionRejected
			}
			return result, Observation{}, err
		}
		result.Action = ActionUpdated
		result.Warnings = append([]string(nil), updated.Warnings...)
	}
	observation, err := jobs.observe(ctx, service.ID, image, iteration, result.Action == ActionUpdated)
	return result, observation, err
}

func (jobs *Jobs) inspect(ctx context.Context, name string) (swarmtypes.Service, error) {
	inspected, err := jobs.client.ServiceInspect(ctx, name, clienttypes.ServiceInspectOptions{})
	if err != nil {
		return swarmtypes.Service{}, err
	}
	s := inspected.Service
	mode := s.Spec.Mode.ReplicatedJob
	restart := s.Spec.TaskTemplate.RestartPolicy
	if s.ID == "" || s.JobStatus == nil || mode == nil || mode.TotalCompletions == nil || *mode.TotalCompletions != 1 ||
		mode.MaxConcurrent == nil || *mode.MaxConcurrent != 1 || s.Spec.TaskTemplate.ContainerSpec == nil ||
		restart == nil || restart.Condition != swarmtypes.RestartPolicyConditionNone {
		return swarmtypes.Service{}, ErrUnsupportedService
	}
	return s, nil
}

func (jobs *Jobs) observe(ctx context.Context, id, image string, iteration uint64, awaitingIteration bool) (Observation, error) {
	ticker := time.NewTicker(jobs.interval)
	defer ticker.Stop()
	last := Observation{Conclusion: ConclusionInProgress}
	for {
		if ctx.Err() != nil {
			return last, nil
		}
		service, err := jobs.inspect(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return last, nil
			}
			if cerrdefs.IsNotFound(err) {
				return Observation{Conclusion: ConclusionSuperseded}, nil
			}
			return last, err
		}
		if service.ID != id || !sameImage(service.Spec.TaskTemplate.ContainerSpec.Image, image) {
			return Observation{Conclusion: ConclusionSuperseded}, nil
		}
		// Docker advances the iteration on update, but does not promise an increment of one.
		if awaitingIteration && service.JobStatus.JobIteration.Index > iteration {
			iteration = service.JobStatus.JobIteration.Index
			awaitingIteration = false
		}
		if !awaitingIteration && service.JobStatus.JobIteration.Index != iteration {
			return Observation{Conclusion: ConclusionSuperseded}, nil
		}
		if !awaitingIteration {
			observation, stable, err := jobs.snapshot(ctx, service)
			if err != nil {
				if ctx.Err() != nil {
					return last, nil
				}
				if cerrdefs.IsNotFound(err) {
					return Observation{Conclusion: ConclusionSuperseded}, nil
				}
				return last, err
			}
			if stable {
				last = observation
				if last.Conclusion != ConclusionInProgress {
					return last, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return last, nil
		case <-ticker.C:
		}
	}
}

func (jobs *Jobs) snapshot(ctx context.Context, service swarmtypes.Service) (Observation, bool, error) {
	tasks, err := jobs.client.TaskList(ctx, clienttypes.TaskListOptions{Filters: clienttypes.Filters{}.Add("service", service.ID)})
	if err != nil {
		return Observation{}, false, err
	}
	after, err := jobs.inspect(ctx, service.ID)
	if err != nil {
		return Observation{}, false, err
	}
	if after.ID != service.ID || after.Version.Index != service.Version.Index || after.JobStatus.JobIteration != service.JobStatus.JobIteration {
		return Observation{}, false, nil
	}
	observation := Observation{Conclusion: ConclusionInProgress, Message: "job execution is in progress"}
	completed, terminal, count := 0, true, 0
	for _, task := range tasks.Items {
		if task.ServiceID != service.ID || task.JobIteration == nil || task.JobIteration.Index != service.JobStatus.JobIteration.Index {
			continue
		}
		count++
		switch task.Status.State {
		case swarmtypes.TaskStateComplete:
			if task.Status.ContainerStatus != nil && task.Status.ContainerStatus.ExitCode == 0 {
				completed++
			} else {
				observation.Tasks.Failed++
			}
		case swarmtypes.TaskStateFailed, swarmtypes.TaskStateRejected, swarmtypes.TaskStateShutdown, swarmtypes.TaskStateOrphaned, swarmtypes.TaskStateRemove:
			observation.Tasks.Failed++
		case swarmtypes.TaskStateRunning:
			terminal = false
			observation.Tasks.Running++
		default:
			terminal = false
			observation.Tasks.Pending++
		}
	}
	if count == 1 && completed == 1 {
		observation.Conclusion, observation.Message = ConclusionCompleted, "job execution completed"
	} else if count > 0 && terminal {
		observation.Conclusion, observation.Message = ConclusionFailed, "job execution failed"
	}
	return observation, true, nil
}
