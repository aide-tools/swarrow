package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

// Limit detailed failures while preserving the complete failure count.
const maxReportedTaskFailures = 10

var ErrInvalidPollInterval = errors.New("poll interval must be positive")

// Conclusion records how rollout observation ended.
type Conclusion string

const (
	ConclusionCompleted  Conclusion = "completed"
	ConclusionFailed     Conclusion = "failed"
	ConclusionRolledBack Conclusion = "rolled_back"
	ConclusionSuperseded Conclusion = "superseded"
	ConclusionInProgress Conclusion = "in_progress"
)

// Target identifies the service image and rollout to observe.
type Target struct {
	ServiceID string
	Image     string
	// RolloutVersion identifies a statusless rollout established by an earlier observation.
	RolloutVersion *uint64
	// RolloutStartedAt identifies a rollout established by an earlier observation.
	RolloutStartedAt *time.Time
}

// TaskFailure describes one failed task for the observed image.
type TaskFailure struct {
	ID    string
	Slot  int
	State swarmtypes.TaskState
	Error string
}

// TaskSummary summarises tasks that use the observed image.
type TaskSummary struct {
	Running  int
	Pending  int
	Failed   int
	Failures []TaskFailure
}

// Observation is the conclusion reached from the latest service and task state.
type Observation struct {
	Conclusion     Conclusion
	Message        string
	ServiceVersion uint64
	// RolloutVersion identifies the service version observed before Docker published rollout status.
	RolloutVersion *uint64
	// RolloutStartedAt identifies the rollout followed by this observation.
	RolloutStartedAt *time.Time
	Tasks            TaskSummary
}

type observationClient interface {
	ServiceInspect(context.Context, string, clienttypes.ServiceInspectOptions) (clienttypes.ServiceInspectResult, error)
	TaskList(context.Context, clienttypes.TaskListOptions) (clienttypes.TaskListResult, error)
}

// Observer polls Docker until a rollout concludes or its context ends.
type Observer struct {
	client   observationClient
	interval time.Duration
}

// NewObserver creates a rollout observer with a fixed polling interval.
func NewObserver(client observationClient, interval time.Duration) (*Observer, error) {
	if interval <= 0 {
		return nil, ErrInvalidPollInterval
	}

	return &Observer{client: client, interval: interval}, nil
}

// Observe follows one service image until Docker reports a conclusion.
func (observer *Observer) Observe(ctx context.Context, target Target) (Observation, error) {
	resumedFromRolloutVersion := target.RolloutVersion != nil
	rolloutVersion := cloneUint64(target.RolloutVersion)
	rolloutStartedAt := cloneTime(target.RolloutStartedAt)
	last := Observation{
		RolloutVersion:   cloneUint64(rolloutVersion),
		RolloutStartedAt: cloneTime(rolloutStartedAt),
	}
	ticker := time.NewTicker(observer.interval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			last.Conclusion = ConclusionInProgress
			return last, nil
		}

		snapshot, err := observer.inspect(ctx, target)
		if err != nil {
			if ctx.Err() != nil {
				last.Conclusion = ConclusionInProgress
				return last, nil
			}

			return Observation{}, err
		}
		if snapshot.changed {
			select {
			case <-ctx.Done():
				last.Conclusion = ConclusionInProgress
				return last, nil
			case <-ticker.C:
			}
			continue
		}

		last = snapshot.observation
		if rolloutStartedAt != nil && snapshot.startedAt == nil {
			last.RolloutStartedAt = cloneTime(rolloutStartedAt)
			last.Conclusion = ConclusionSuperseded
			return last, nil
		}
		if rolloutStartedAt == nil && rolloutVersion != nil {
			versionChanged := snapshot.observation.ServiceVersion != *rolloutVersion
			if (resumedFromRolloutVersion && (snapshot.startedAt != nil || versionChanged)) ||
				(!resumedFromRolloutVersion && snapshot.startedAt == nil && versionChanged) {
				last.RolloutVersion = cloneUint64(rolloutVersion)
				last.Conclusion = ConclusionSuperseded
				return last, nil
			}
		}
		if snapshot.startedAt != nil {
			if rolloutStartedAt != nil && !snapshot.startedAt.Equal(*rolloutStartedAt) {
				last.RolloutStartedAt = cloneTime(rolloutStartedAt)
				last.Tasks = summariseTasks(snapshot.tasks, target.Image, rolloutStartedAt, snapshot.startedAt)
				last.Conclusion = ConclusionSuperseded
				return last, nil
			}

			rolloutStartedAt = cloneTime(snapshot.startedAt)
			rolloutVersion = nil
		} else if snapshot.rolloutRequired && rolloutVersion == nil {
			version := snapshot.observation.ServiceVersion
			rolloutVersion = &version
		}
		last.RolloutVersion = cloneUint64(rolloutVersion)
		last.RolloutStartedAt = cloneTime(rolloutStartedAt)
		if rolloutStartedAt != nil {
			last.Tasks = summariseTasks(snapshot.tasks, target.Image, rolloutStartedAt, nil)
		}

		conclusion, terminal, err := conclude(snapshot, target)
		if err != nil {
			return Observation{}, err
		}
		last.Conclusion = conclusion
		if terminal {
			return last, nil
		}

		select {
		case <-ctx.Done():
			last.Conclusion = ConclusionInProgress
			return last, nil
		case <-ticker.C:
		}
	}
}

type rolloutSnapshot struct {
	observation     Observation
	image           string
	updateState     *swarmtypes.UpdateState
	startedAt       *time.Time
	tasks           []swarmtypes.Task
	rolloutRequired bool
	changed         bool
}

func (observer *Observer) inspect(ctx context.Context, target Target) (rolloutSnapshot, error) {
	inspected, err := observer.client.ServiceInspect(ctx, target.ServiceID, clienttypes.ServiceInspectOptions{})
	if err != nil {
		return rolloutSnapshot{}, fmt.Errorf("inspect service %q: %w", target.ServiceID, err)
	}

	service := inspected.Service
	container := service.Spec.TaskTemplate.ContainerSpec
	if container == nil {
		return rolloutSnapshot{}, fmt.Errorf("%w: service %q does not use container tasks", ErrUnsupportedService, target.ServiceID)
	}
	if isJobService(service) {
		return rolloutSnapshot{}, fmt.Errorf("%w: service %q uses job mode", ErrUnsupportedService, target.ServiceID)
	}

	filters := make(clienttypes.Filters).Add("service", service.ID)
	listed, err := observer.client.TaskList(ctx, clienttypes.TaskListOptions{Filters: filters})
	if err != nil {
		return rolloutSnapshot{}, fmt.Errorf("list tasks for service %q: %w", target.ServiceID, err)
	}

	snapshot := rolloutSnapshot{
		observation: Observation{
			ServiceVersion: service.Version.Index,
		},
		image:           container.Image,
		tasks:           listed.Items,
		rolloutRequired: requiresImageRollout(listed.Items, target.Image),
	}

	if service.UpdateStatus != nil {
		state := service.UpdateStatus.State
		snapshot.updateState = &state
		snapshot.startedAt = service.UpdateStatus.StartedAt
		snapshot.observation.Message = service.UpdateStatus.Message
	}

	reinspected, err := observer.client.ServiceInspect(ctx, service.ID, clienttypes.ServiceInspectOptions{})
	if err != nil {
		return rolloutSnapshot{}, fmt.Errorf("reinspect service %q: %w", target.ServiceID, err)
	}
	snapshot.changed = reinspected.Service.Version.Index != service.Version.Index

	return snapshot, nil
}

func conclude(snapshot rolloutSnapshot, target Target) (Conclusion, bool, error) {
	if snapshot.updateState != nil {
		switch *snapshot.updateState {
		case swarmtypes.UpdateStateRollbackStarted:
			return ConclusionInProgress, false, nil
		case swarmtypes.UpdateStateRollbackCompleted:
			return ConclusionRolledBack, true, nil
		case swarmtypes.UpdateStateRollbackPaused:
			return ConclusionFailed, true, nil
		}
	}

	if !sameImage(snapshot.image, target.Image) {
		return ConclusionSuperseded, true, nil
	}

	if snapshot.updateState == nil {
		// SwarmKit creates UpdateStatus only when an active task still needs
		// to move to the service's desired image.
		if snapshot.rolloutRequired {
			return ConclusionInProgress, false, nil
		}

		return ConclusionCompleted, true, nil
	}

	switch *snapshot.updateState {
	case swarmtypes.UpdateStateUpdating:
		return ConclusionInProgress, false, nil
	case swarmtypes.UpdateStateCompleted:
		return ConclusionCompleted, true, nil
	case swarmtypes.UpdateStatePaused:
		return ConclusionFailed, true, nil
	default:
		return "", false, fmt.Errorf("observe service: unsupported update state %q", *snapshot.updateState)
	}
}

func summariseTasks(tasks []swarmtypes.Task, image string, startedAt *time.Time, endedBefore *time.Time) TaskSummary {
	var summary TaskSummary

	for _, task := range tasks {
		container := task.Spec.ContainerSpec
		if container == nil || !sameImage(container.Image, image) {
			continue
		}
		// Docker retains historical tasks, so use the rollout's time bounds
		// to exclude tasks created for earlier or replacement rollouts.
		if startedAt != nil && task.CreatedAt.Before(*startedAt) {
			continue
		}
		if endedBefore != nil && !task.CreatedAt.Before(*endedBefore) {
			continue
		}

		switch task.Status.State {
		case swarmtypes.TaskStateRunning:
			summary.Running++
		case swarmtypes.TaskStateFailed, swarmtypes.TaskStateRejected, swarmtypes.TaskStateOrphaned:
			summary.Failed++
			if len(summary.Failures) < maxReportedTaskFailures {
				summary.Failures = append(summary.Failures, TaskFailure{
					ID:    task.ID,
					Slot:  task.Slot,
					State: task.Status.State,
					Error: task.Status.Err,
				})
			}
		case swarmtypes.TaskStateComplete, swarmtypes.TaskStateShutdown, swarmtypes.TaskStateRemove:
		default:
			summary.Pending++
		}
	}

	return summary
}

func requiresImageRollout(tasks []swarmtypes.Task, image string) bool {
	for _, task := range tasks {
		if task.DesiredState > swarmtypes.TaskStateRunning {
			continue
		}
		container := task.Spec.ContainerSpec
		if container != nil && !sameImage(container.Image, image) {
			return true
		}
	}

	return false
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	cloned := *value
	return &cloned
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}

	cloned := *value
	return &cloned
}
