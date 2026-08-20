package swarm_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/swarm"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

func TestObserveMapsTerminalServiceStates(t *testing.T) {
	startedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		state      *swarmtypes.UpdateState
		image      string
		conclusion swarm.Conclusion
	}{
		"completed": {
			state:      updateState(swarmtypes.UpdateStateCompleted),
			image:      desiredImage,
			conclusion: swarm.ConclusionCompleted,
		},
		"paused": {
			state:      updateState(swarmtypes.UpdateStatePaused),
			image:      desiredImage,
			conclusion: swarm.ConclusionFailed,
		},
		"rolled back": {
			state:      updateState(swarmtypes.UpdateStateRollbackCompleted),
			image:      repository + ":previous",
			conclusion: swarm.ConclusionRolledBack,
		},
		"rollback paused": {
			state:      updateState(swarmtypes.UpdateStateRollbackPaused),
			image:      repository + ":previous",
			conclusion: swarm.ConclusionFailed,
		},
		"superseded": {
			state:      updateState(swarmtypes.UpdateStateUpdating),
			image:      repository + "@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			conclusion: swarm.ConclusionSuperseded,
		},
		"no active rollout": {
			image:      desiredImage,
			conclusion: swarm.ConclusionCompleted,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service := observedService(test.image, test.state, &startedAt)
			client := &observationFake{services: []swarmtypes.Service{service}}
			observer, err := swarm.NewObserver(client, time.Millisecond)
			if err != nil {
				t.Fatalf("NewObserver() error = %v", err)
			}

			observation, err := observer.Observe(context.Background(), swarm.Target{
				ServiceID: service.ID,
				Image:     desiredImage,
			})
			if err != nil {
				t.Fatalf("Observe() error = %v", err)
			}
			if observation.Conclusion != test.conclusion {
				t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, test.conclusion)
			}
			if test.state != nil && observation.Message != "rollout message" {
				t.Errorf("Observe() message = %q, want rollout message", observation.Message)
			}
		})
	}
}

func TestObservePollsUntilCompletion(t *testing.T) {
	startedAt := time.Now()
	updating := observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &startedAt)
	completed := observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), &startedAt)
	completed.Version.Index++
	client := &observationFake{services: []swarmtypes.Service{updating, updating, completed, completed}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionCompleted)
	}
	if client.inspectCalls != 4 {
		t.Errorf("ServiceInspect() calls = %d, want 4", client.inspectCalls)
	}
}

func TestObserveReinspectsStatusBearingSnapshots(t *testing.T) {
	firstStart := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	secondStart := firstStart.Add(time.Minute)
	completed := observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), &firstStart)
	replacement := observedService(repository+":replacement", updateState(swarmtypes.UpdateStateUpdating), &secondStart)
	replacement.Version.Index++
	client := &observationFake{services: []swarmtypes.Service{completed, replacement, replacement, replacement}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionSuperseded {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionSuperseded)
	}
	if client.inspectCalls != 4 {
		t.Errorf("ServiceInspect() calls = %d, want 4", client.inspectCalls)
	}
}

func TestObserveDetectsAReplacementRolloutForTheSameImage(t *testing.T) {
	firstStart := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	secondStart := firstStart.Add(time.Minute)
	client := &observationFake{services: []swarmtypes.Service{
		observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &firstStart),
		observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &secondStart),
	}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionSuperseded {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionSuperseded)
	}
}

func TestObserveDetectsAReplacementRolloutAfterResuming(t *testing.T) {
	firstStart := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	secondStart := firstStart.Add(time.Minute)
	client := &observationFake{services: []swarmtypes.Service{
		observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), &secondStart),
	}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID:        "service-id",
		Image:            desiredImage,
		RolloutStartedAt: &firstStart,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionSuperseded {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionSuperseded)
	}
	if observation.RolloutStartedAt == nil || !observation.RolloutStartedAt.Equal(firstStart) {
		t.Errorf("Observe() rollout start = %v, want %v", observation.RolloutStartedAt, firstStart)
	}
}

func TestObserveDetectsAStatuslessReplacementAfterResuming(t *testing.T) {
	firstStart := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	client := &observationFake{
		services: []swarmtypes.Service{observedService(desiredImage, nil, nil)},
		tasks: []swarmtypes.Task{
			observedTask("replacement", desiredImage, swarmtypes.TaskStateRunning, "", firstStart.Add(time.Minute)),
		},
	}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID:        "service-id",
		Image:            desiredImage,
		RolloutStartedAt: &firstStart,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionSuperseded {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionSuperseded)
	}
	if observation.RolloutStartedAt == nil || !observation.RolloutStartedAt.Equal(firstStart) {
		t.Errorf("Observe() rollout start = %v, want %v", observation.RolloutStartedAt, firstStart)
	}
	if !reflect.DeepEqual(observation.Tasks, swarm.TaskSummary{}) {
		t.Errorf("Observe() tasks = %#v, want no replacement task diagnostics", observation.Tasks)
	}
}

func TestObserveReturnsInProgressWhenTheContextEnds(t *testing.T) {
	startedAt := time.Now()
	client := &observationFake{services: []swarmtypes.Service{
		observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &startedAt),
	}}
	observer, err := swarm.NewObserver(client, time.Hour)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	observation, err := observer.Observe(ctx, swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionInProgress {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionInProgress)
	}
	if client.inspectCalls != 0 {
		t.Errorf("ServiceInspect() calls = %d, want 0", client.inspectCalls)
	}
}

func TestObserveReturnsTheRolloutIdentityWhenTheContextEnds(t *testing.T) {
	startedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	client := &observationFake{services: []swarmtypes.Service{
		observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &startedAt),
	}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	observation, err := observer.Observe(ctx, swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionInProgress {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionInProgress)
	}
	if observation.RolloutStartedAt == nil || !observation.RolloutStartedAt.Equal(startedAt) {
		t.Errorf("Observe() rollout start = %v, want %v", observation.RolloutStartedAt, startedAt)
	}
}

func TestObserveWaitsWhileActiveTasksNeedTheImage(t *testing.T) {
	client := &observationFake{services: []swarmtypes.Service{
		observedService(desiredImage, nil, nil),
	}, tasks: []swarmtypes.Task{
		observedTask("old", repository+":previous", swarmtypes.TaskStateRunning, "", time.Now()),
		observedTask("historical", desiredImage, swarmtypes.TaskStateFailed, "historical failure", time.Now().Add(-time.Hour)),
	}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	observation, err := observer.Observe(ctx, swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionInProgress {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionInProgress)
	}
	if observation.RolloutVersion == nil || *observation.RolloutVersion != 42 {
		t.Errorf("Observe() rollout version = %v, want 42", observation.RolloutVersion)
	}
	if !reflect.DeepEqual(observation.Tasks, swarm.TaskSummary{}) {
		t.Errorf("Observe() tasks = %#v, want no diagnostics before rollout start", observation.Tasks)
	}
}

func TestObserveDetectsAChangedPreStatusVersionAfterResuming(t *testing.T) {
	previousVersion := uint64(42)
	startedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	service := observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &startedAt)
	service.Version.Index = 43
	client := &observationFake{services: []swarmtypes.Service{service}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID:      "service-id",
		Image:          desiredImage,
		RolloutVersion: &previousVersion,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionSuperseded {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionSuperseded)
	}
	if observation.RolloutVersion == nil || *observation.RolloutVersion != previousVersion {
		t.Errorf("Observe() rollout version = %v, want %d", observation.RolloutVersion, previousVersion)
	}
}

func TestObserveAcceptsALocallyObservedRolloutStarting(t *testing.T) {
	startedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	statusless := observedService(desiredImage, nil, nil)
	updating := observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &startedAt)
	updating.Version.Index++
	completed := observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), &startedAt)
	completed.Version.Index += 2
	client := &observationFake{
		services: []swarmtypes.Service{statusless, statusless, updating, updating, completed, completed},
		tasks: []swarmtypes.Task{
			observedTask("old", repository+":previous", swarmtypes.TaskStateRunning, "", startedAt.Add(-time.Minute)),
		},
	}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionCompleted)
	}
	if observation.RolloutVersion != nil {
		t.Errorf("Observe() rollout version = %v, want nil", observation.RolloutVersion)
	}
	if observation.RolloutStartedAt == nil || !observation.RolloutStartedAt.Equal(startedAt) {
		t.Errorf("Observe() rollout start = %v, want %v", observation.RolloutStartedAt, startedAt)
	}
}

func TestObserveReinspectsBeforeStatuslessCompletion(t *testing.T) {
	startedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	statusless := observedService(desiredImage, nil, nil)
	updating := observedService(desiredImage, updateState(swarmtypes.UpdateStateUpdating), &startedAt)
	updating.Version.Index++
	completed := observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), &startedAt)
	completed.Version.Index += 2
	client := &observationFake{services: []swarmtypes.Service{statusless, updating, completed}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{ServiceID: "service-id", Image: desiredImage})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionCompleted)
	}
	if client.inspectCalls != 4 {
		t.Errorf("ServiceInspect() calls = %d, want 4", client.inspectCalls)
	}
}

func TestObserveCompletesAStatuslessZeroReplicaRollout(t *testing.T) {
	service := observedService(desiredImage, nil, nil)
	zero := uint64(0)
	service.Spec.Mode.Replicated.Replicas = &zero
	client := &observationFake{services: []swarmtypes.Service{service}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionCompleted)
	}
}

func TestObserveCompletesAStatuslessGlobalRolloutWithoutEligibleTasks(t *testing.T) {
	service := observedService(desiredImage, nil, nil)
	service.Spec.Mode = swarmtypes.ServiceMode{Global: &swarmtypes.GlobalService{}}
	client := &observationFake{services: []swarmtypes.Service{service}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Observe() conclusion = %q, want %q", observation.Conclusion, swarm.ConclusionCompleted)
	}
}

func TestObserveRejectsJobServices(t *testing.T) {
	tests := map[string]swarmtypes.ServiceMode{
		"replicated job": {ReplicatedJob: &swarmtypes.ReplicatedJob{}},
		"global job":     {GlobalJob: &swarmtypes.GlobalJob{}},
	}

	for name, mode := range tests {
		t.Run(name, func(t *testing.T) {
			service := observedService(desiredImage, nil, nil)
			service.Spec.Mode = mode
			client := &observationFake{services: []swarmtypes.Service{service}}
			observer, err := swarm.NewObserver(client, time.Millisecond)
			if err != nil {
				t.Fatalf("NewObserver() error = %v", err)
			}

			_, err = observer.Observe(context.Background(), swarm.Target{ServiceID: "service-id", Image: desiredImage})
			if !errors.Is(err, swarm.ErrUnsupportedService) {
				t.Fatalf("Observe() error = %v, want ErrUnsupportedService", err)
			}
		})
	}
}

func TestObserveSummarisesTasksForTheTargetImage(t *testing.T) {
	startedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	currentTask := startedAt.Add(time.Second)
	client := &observationFake{
		services: []swarmtypes.Service{
			observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), &startedAt),
		},
		tasks: []swarmtypes.Task{
			observedTask("running", desiredImage, swarmtypes.TaskStateRunning, "", currentTask),
			observedTask("pending", desiredImage, swarmtypes.TaskStatePreparing, "", currentTask),
			observedTask("failed", desiredImage, swarmtypes.TaskStateFailed, "health check failed", currentTask),
			observedTask("historical", desiredImage, swarmtypes.TaskStateFailed, "historical failure", startedAt.Add(-time.Hour)),
			observedTask("old", repository+":previous", swarmtypes.TaskStateFailed, "old failure", currentTask),
			observedTask("shutdown", desiredImage, swarmtypes.TaskStateShutdown, "", currentTask),
		},
	}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{
		ServiceID: "service-id",
		Image:     desiredImage,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}

	expected := swarm.TaskSummary{
		Running: 1,
		Pending: 1,
		Failed:  1,
		Failures: []swarm.TaskFailure{
			{ID: "failed", Slot: 1, State: swarmtypes.TaskStateFailed, Error: "health check failed"},
		},
	}
	if !reflect.DeepEqual(observation.Tasks, expected) {
		t.Errorf("Observe() tasks = %#v, want %#v", observation.Tasks, expected)
	}
	if !reflect.DeepEqual(client.taskFilters, clienttypes.Filters{"service": {"service-id": true}}) {
		t.Errorf("TaskList() filters = %#v, want service ID filter", client.taskFilters)
	}
}

func TestObserveCapsReportedTaskFailureDetails(t *testing.T) {
	startedAt := time.Now()
	client := &observationFake{
		services: []swarmtypes.Service{
			observedService(desiredImage, updateState(swarmtypes.UpdateStatePaused), &startedAt),
		},
	}
	for index := range 12 {
		client.tasks = append(client.tasks, observedTask(string(rune('a'+index)), desiredImage, swarmtypes.TaskStateFailed, "failed", startedAt.Add(time.Second)))
	}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	observation, err := observer.Observe(context.Background(), swarm.Target{ServiceID: "service-id", Image: desiredImage})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Tasks.Failed != 12 {
		t.Errorf("Observe() failed count = %d, want 12", observation.Tasks.Failed)
	}
	if len(observation.Tasks.Failures) != 10 {
		t.Errorf("Observe() reported failures = %d, want 10", len(observation.Tasks.Failures))
	}
}

func TestObserveReturnsDockerErrors(t *testing.T) {
	inspectError := errors.New("inspect failed")
	client := &observationFake{inspectError: inspectError}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	_, err = observer.Observe(context.Background(), swarm.Target{ServiceID: "service-id", Image: desiredImage})
	if !errors.Is(err, inspectError) {
		t.Fatalf("Observe() error = %v, want inspect error", err)
	}
}

func TestObserveReturnsTaskListErrors(t *testing.T) {
	taskListError := errors.New("task list failed")
	client := &observationFake{
		services:      []swarmtypes.Service{observedService(desiredImage, updateState(swarmtypes.UpdateStateCompleted), nil)},
		taskListError: taskListError,
	}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	_, err = observer.Observe(context.Background(), swarm.Target{ServiceID: "service-id", Image: desiredImage})
	if !errors.Is(err, taskListError) {
		t.Fatalf("Observe() error = %v, want task list error", err)
	}
}

func TestObserveRejectsUnknownUpdateStates(t *testing.T) {
	startedAt := time.Now()
	unknown := swarmtypes.UpdateState("unknown")
	client := &observationFake{services: []swarmtypes.Service{
		observedService(desiredImage, &unknown, &startedAt),
	}}
	observer, err := swarm.NewObserver(client, time.Millisecond)
	if err != nil {
		t.Fatalf("NewObserver() error = %v", err)
	}

	_, err = observer.Observe(context.Background(), swarm.Target{ServiceID: "service-id", Image: desiredImage})
	if err == nil {
		t.Fatal("Observe() error = nil, want unsupported state error")
	}
}

func TestNewObserverRejectsInvalidIntervals(t *testing.T) {
	_, err := swarm.NewObserver(&observationFake{}, 0)
	if !errors.Is(err, swarm.ErrInvalidPollInterval) {
		t.Fatalf("NewObserver() error = %v, want ErrInvalidPollInterval", err)
	}
}

type observationFake struct {
	services      []swarmtypes.Service
	tasks         []swarmtypes.Task
	inspectError  error
	taskListError error
	inspectCalls  int
	taskFilters   clienttypes.Filters
}

func (client *observationFake) ServiceInspect(_ context.Context, _ string, _ clienttypes.ServiceInspectOptions) (clienttypes.ServiceInspectResult, error) {
	client.inspectCalls++
	if client.inspectError != nil {
		return clienttypes.ServiceInspectResult{}, client.inspectError
	}
	index := min(client.inspectCalls-1, len(client.services)-1)
	return clienttypes.ServiceInspectResult{Service: client.services[index]}, nil
}

func (client *observationFake) TaskList(_ context.Context, options clienttypes.TaskListOptions) (clienttypes.TaskListResult, error) {
	client.taskFilters = options.Filters.Clone()
	return clienttypes.TaskListResult{Items: client.tasks}, client.taskListError
}

func observedService(image string, state *swarmtypes.UpdateState, startedAt *time.Time) swarmtypes.Service {
	service := populatedService()
	service.Spec.TaskTemplate.ContainerSpec.Image = image
	if state != nil {
		service.UpdateStatus = &swarmtypes.UpdateStatus{
			State:     *state,
			StartedAt: startedAt,
			Message:   "rollout message",
		}
	}
	return service
}

func observedTask(id string, image string, state swarmtypes.TaskState, taskError string, createdAt time.Time) swarmtypes.Task {
	return swarmtypes.Task{
		ID:   id,
		Slot: 1,
		Meta: swarmtypes.Meta{
			CreatedAt: createdAt,
		},
		Spec: swarmtypes.TaskSpec{ContainerSpec: &swarmtypes.ContainerSpec{Image: image}},
		Status: swarmtypes.TaskStatus{
			State: state,
			Err:   taskError,
		},
	}
}

func updateState(state swarmtypes.UpdateState) *swarmtypes.UpdateState {
	return &state
}
