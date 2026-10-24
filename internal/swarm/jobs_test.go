package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

const jobDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const jobRepository = "ghcr.io/example/app"

type jobFake struct {
	service       swarmtypes.Service
	tasks         []swarmtypes.Task
	updates       int
	updateError   error
	accept        bool
	complete      bool
	afterList     func()
	afterUpdate   func()
	updateOptions clienttypes.ServiceUpdateOptions
	inspections   int
	inspectError  error
}

func newJobFake() *jobFake {
	one := uint64(1)
	f := &jobFake{accept: true, complete: true}
	f.service = swarmtypes.Service{
		Meta: swarmtypes.Meta{Version: swarmtypes.Version{Index: 42}},
		Spec: swarmtypes.ServiceSpec{
			Annotations: swarmtypes.Annotations{Name: "migration", Labels: map[string]string{"owner": "operator"}},
			TaskTemplate: swarmtypes.TaskSpec{
				ContainerSpec: &swarmtypes.ContainerSpec{
					Image:   "ghcr.io/example/app@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					Command: []string{"/app/bin/migrate"},
					Env:     []string{"DATABASE_HOST=db"},
					Secrets: []*swarmtypes.SecretReference{{SecretID: "database-secret", SecretName: "database"}},
				},
				Networks:  []swarmtypes.NetworkAttachmentConfig{{Target: "database"}},
				Placement: &swarmtypes.Placement{Constraints: []string{"node.labels.database == true"}},
			},
		},
	}
	f.service.ID = "migration-service"
	f.service.Spec.Mode = swarmtypes.ServiceMode{ReplicatedJob: &swarmtypes.ReplicatedJob{MaxConcurrent: &one, TotalCompletions: &one}}
	f.service.Spec.TaskTemplate.RestartPolicy = &swarmtypes.RestartPolicy{Condition: swarmtypes.RestartPolicyConditionNone}
	f.service.JobStatus = &swarmtypes.JobStatus{JobIteration: swarmtypes.Version{Index: 7}}
	f.setTask(swarmtypes.TaskStateComplete, 0)
	return f
}

func (f *jobFake) setTask(state swarmtypes.TaskState, code int) {
	f.tasks = []swarmtypes.Task{{
		ID: "task", ServiceID: f.service.ID,
		JobIteration: &swarmtypes.Version{Index: f.service.JobStatus.JobIteration.Index},
		Spec:         f.service.Spec.TaskTemplate,
		Status:       swarmtypes.TaskStatus{State: state, ContainerStatus: &swarmtypes.ContainerStatus{ExitCode: code}},
	}}
}

func (f *jobFake) ServiceInspect(context.Context, string, clienttypes.ServiceInspectOptions) (clienttypes.ServiceInspectResult, error) {
	f.inspections++
	return clienttypes.ServiceInspectResult{Service: f.service}, f.inspectError
}

func (f *jobFake) ServiceUpdate(_ context.Context, id string, options clienttypes.ServiceUpdateOptions) (clienttypes.ServiceUpdateResult, error) {
	f.updates++
	f.updateOptions = options
	if id != f.service.ID || options.Version != f.service.Version {
		return clienttypes.ServiceUpdateResult{}, cerrdefs.ErrConflict
	}
	if f.accept {
		previous := f.service.Spec
		f.service.PreviousSpec = &previous
		f.service.Spec = options.Spec
		f.service.Version.Index++
		// Deliberately do not increment by one: the API promises only advancement.
		f.service.JobStatus = &swarmtypes.JobStatus{JobIteration: swarmtypes.Version{Index: f.service.JobStatus.JobIteration.Index + 3}}
		if f.complete {
			f.setTask(swarmtypes.TaskStateComplete, 0)
		} else {
			f.setTask(swarmtypes.TaskStateRunning, 0)
		}
	}
	if f.afterUpdate != nil {
		f.afterUpdate()
	}
	return clienttypes.ServiceUpdateResult{Warnings: []string{"warning"}}, f.updateError
}

func (f *jobFake) TaskList(context.Context, clienttypes.TaskListOptions) (clienttypes.TaskListResult, error) {
	tasks := append([]swarmtypes.Task(nil), f.tasks...)
	if f.afterList != nil {
		f.afterList()
	}
	return clienttypes.TaskListResult{Items: tasks}, nil
}

func jobRunnerForTest(t *testing.T, f *jobFake) *Jobs {
	t.Helper()
	jobs, err := NewJobs(f, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestJobUpdatesOnlyImage(t *testing.T) {
	f := newJobFake()
	original := cloneJobService(t, f.service)
	result, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionCompleted || result.Action != ActionUpdated {
		t.Fatalf("run = %+v %+v %v", result, observed, err)
	}
	expected := original.Spec
	expected.TaskTemplate.ContainerSpec.Image = jobRepository + "@" + jobDigest
	if !reflect.DeepEqual(f.updateOptions.Spec, expected) {
		t.Fatal("changed operator-owned settings")
	}
	if f.updateOptions.Version != original.Version || f.updateOptions.RegistryAuthFrom != swarmtypes.RegistryAuthFromSpec || f.updateOptions.QueryRegistry {
		t.Fatalf("update options = %+v", f.updateOptions)
	}
	if result.InspectedVersion != original.Version.Index || !reflect.DeepEqual(result.Warnings, []string{"warning"}) {
		t.Fatalf("result = %+v", result)
	}
}

func TestJobMatchingImageObservesProvisionedExecution(t *testing.T) {
	for _, state := range []swarmtypes.TaskState{swarmtypes.TaskStateComplete, swarmtypes.TaskStateFailed, swarmtypes.TaskStateRunning} {
		t.Run(string(state), func(t *testing.T) {
			f := newJobFake()
			f.service.Spec.TaskTemplate.ContainerSpec.Image = jobRepository + ":release@" + jobDigest
			f.setTask(state, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			result, observed, err := jobRunnerForTest(t, f).Run(ctx, f.service.ID, jobRepository, jobDigest)
			expected := map[swarmtypes.TaskState]Conclusion{
				swarmtypes.TaskStateComplete: ConclusionCompleted,
				swarmtypes.TaskStateFailed:   ConclusionFailed,
				swarmtypes.TaskStateRunning:  ConclusionInProgress,
			}[state]
			if err != nil || observed.Conclusion != expected || result.Action != ActionNoChange || f.updates != 0 {
				t.Fatalf("run = %+v %+v %v updates=%d", result, observed, err, f.updates)
			}
		})
	}
}

func TestJobLostResponseRetryObservesWithoutAnotherUpdate(t *testing.T) {
	f := newJobFake()
	f.updateError = errors.New("lost response after acceptance")
	result, _, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err == nil || result.Action != ActionIndeterminate {
		t.Fatalf("run = %+v %v", result, err)
	}
	// A new adapter has no memory of the first request, as after a process restart.
	result, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionCompleted || result.Action != ActionNoChange || f.updates != 1 {
		t.Fatalf("retry = %+v %+v %v updates=%d", result, observed, err, f.updates)
	}
}

func TestJobTimeoutDoesNotStopOrRestartExecution(t *testing.T) {
	f := newJobFake()
	f.complete = false
	jobs := jobRunnerForTest(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, observed, err := jobs.Run(ctx, f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionInProgress || f.tasks[0].Status.State != swarmtypes.TaskStateRunning {
		t.Fatalf("run = %+v %v", observed, err)
	}
	f.setTask(swarmtypes.TaskStateComplete, 0)
	_, observed, err = jobs.Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionCompleted || f.updates != 1 {
		t.Fatalf("retry = %+v %v updates=%d", observed, err, f.updates)
	}
}

func TestJobCannotInterruptActiveOrUnknownExecution(t *testing.T) {
	for _, state := range []swarmtypes.TaskState{swarmtypes.TaskStateRunning, swarmtypes.TaskStatePending, "missing"} {
		t.Run(string(state), func(t *testing.T) {
			f := newJobFake()
			f.setTask(state, 0)
			if state == "missing" {
				f.tasks = nil
			}
			_, _, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
			if !errors.Is(err, ErrJobBusy) || f.updates != 0 {
				t.Fatalf("err=%v updates=%d", err, f.updates)
			}
		})
	}
}

func TestJobValidatesDefinitionBeforeMutation(t *testing.T) {
	cases := map[string]func(*jobFake){
		"service": func(f *jobFake) {
			f.service.Spec.Mode = swarmtypes.ServiceMode{Replicated: &swarmtypes.ReplicatedService{}}
		},
		"global job":     func(f *jobFake) { f.service.Spec.Mode = swarmtypes.ServiceMode{GlobalJob: &swarmtypes.GlobalJob{}} },
		"multiple tasks": func(f *jobFake) { two := uint64(2); f.service.Spec.Mode.ReplicatedJob.TotalCompletions = &two },
		"concurrency":    func(f *jobFake) { two := uint64(2); f.service.Spec.Mode.ReplicatedJob.MaxConcurrent = &two },
		"restart": func(f *jobFake) {
			f.service.Spec.TaskTemplate.RestartPolicy.Condition = swarmtypes.RestartPolicyConditionOnFailure
		},
		"missing restart":   func(f *jobFake) { f.service.Spec.TaskTemplate.RestartPolicy = nil },
		"missing status":    func(f *jobFake) { f.service.JobStatus = nil },
		"missing container": func(f *jobFake) { f.service.Spec.TaskTemplate.ContainerSpec = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newJobFake()
			change(f)
			_, _, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
			if !errors.Is(err, ErrUnsupportedService) || f.updates != 0 {
				t.Fatalf("err=%v updates=%d", err, f.updates)
			}
		})
	}
}

func TestJobCompletionRequiresCurrentIterationAndZeroExit(t *testing.T) {
	for _, scenario := range []string{"old task", "other service", "missing iteration", "missing tasks", "nonzero", "missing exit", "rejected", "shutdown", "multiple completions"} {
		t.Run(scenario, func(t *testing.T) {
			f := newJobFake()
			f.service.Spec.TaskTemplate.ContainerSpec.Image = jobRepository + "@" + jobDigest
			expected := ConclusionFailed
			switch scenario {
			case "old task":
				f.tasks[0].JobIteration.Index--
				expected = ConclusionInProgress
			case "other service":
				f.tasks[0].ServiceID = "other"
				expected = ConclusionInProgress
			case "missing iteration":
				f.tasks[0].JobIteration = nil
				expected = ConclusionInProgress
			case "missing tasks":
				f.tasks = nil
				expected = ConclusionInProgress
			case "nonzero":
				f.setTask(swarmtypes.TaskStateComplete, 1)
			case "missing exit":
				f.tasks[0].Status.ContainerStatus = nil
			case "rejected":
				f.setTask(swarmtypes.TaskStateRejected, 1)
			case "shutdown":
				f.setTask(swarmtypes.TaskStateShutdown, 0)
			case "multiple completions":
				f.tasks = append(f.tasks, f.tasks[0])
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, observed, err := jobRunnerForTest(t, f).Run(ctx, f.service.ID, jobRepository, jobDigest)
			if err != nil || observed.Conclusion != expected || f.updates != 0 {
				t.Fatalf("observed=%+v err=%v updates=%d", observed, err, f.updates)
			}
		})
	}
}

func TestJobConcurrentChangeBlocksUpdate(t *testing.T) {
	f := newJobFake()
	f.afterList = func() { f.service.Version.Index++ }
	_, _, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if !errors.Is(err, ErrConflict) || f.updates != 0 {
		t.Fatalf("err=%v updates=%d", err, f.updates)
	}
}

func TestJobObservationDetectsReplacement(t *testing.T) {
	for _, change := range []string{"image", "iteration", "service", "removed"} {
		t.Run(change, func(t *testing.T) {
			f := newJobFake()
			f.service.Spec.TaskTemplate.ContainerSpec.Image = jobRepository + "@" + jobDigest
			f.afterList = func() {
				f.afterList = nil
				f.service.Version.Index++
				switch change {
				case "image":
					f.service.Spec.TaskTemplate.ContainerSpec.Image = "other"
				case "iteration":
					f.service.JobStatus = &swarmtypes.JobStatus{JobIteration: swarmtypes.Version{Index: 50}}
				case "service":
					f.service.ID = "replacement"
				case "removed":
					f.inspectError = cerrdefs.ErrNotFound
				}
			}
			_, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
			if err != nil || observed.Conclusion != ConclusionSuperseded || f.updates != 0 {
				t.Fatalf("observed=%+v err=%v", observed, err)
			}
		})
	}
}

func TestJobReportsRejectedAndIndeterminateUpdates(t *testing.T) {
	for _, updateErr := range []error{cerrdefs.ErrConflict, cerrdefs.ErrInvalidArgument, errors.New("connection lost")} {
		t.Run(updateErr.Error(), func(t *testing.T) {
			f := newJobFake()
			f.accept, f.updateError = false, updateErr
			result, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
			expected := ActionRejected
			if !isDefinitiveRejection(updateErr) && !cerrdefs.IsConflict(updateErr) {
				expected = ActionIndeterminate
			}
			if err == nil || result.Action != expected || f.updates != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if cerrdefs.IsConflict(updateErr) && (!errors.Is(err, ErrConflict) || observed.Conclusion != ConclusionSuperseded) {
				t.Fatalf("conflict=%+v %v", observed, err)
			}
		})
	}
}

func TestJobInvalidDigestDoesNotReachDocker(t *testing.T) {
	f := newJobFake()
	_, _, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, "latest")
	if !errors.Is(err, ErrInvalidImage) || f.inspections != 0 || f.updates != 0 {
		t.Fatalf("err=%v inspections=%d updates=%d", err, f.inspections, f.updates)
	}
}

func cloneJobService(t *testing.T, service swarmtypes.Service) swarmtypes.Service {
	t.Helper()
	data, err := json.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	var cloned swarmtypes.Service
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestJobCanReplaceFailedExecution(t *testing.T) {
	f := newJobFake()
	f.setTask(swarmtypes.TaskStateFailed, 1)
	_, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionCompleted || f.updates != 1 {
		t.Fatalf("run = %+v %v updates=%d", observed, err, f.updates)
	}
}

func TestJobIgnoresCompletedTasksFromBeforeUpdate(t *testing.T) {
	f := newJobFake()
	oldTask := f.tasks[0]
	f.complete = false
	f.afterList = func() {
		if f.updates > 0 {
			f.tasks = append(f.tasks, oldTask)
			f.afterList = nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, observed, err := jobRunnerForTest(t, f).Run(ctx, f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionInProgress || observed.Tasks.Running != 1 {
		t.Fatalf("run = %+v %v", observed, err)
	}
}

func TestJobDiscardsMixedCompletionSnapshot(t *testing.T) {
	f := newJobFake()
	f.service.Spec.TaskTemplate.ContainerSpec.Image = jobRepository + "@" + jobDigest
	f.afterList = func() {
		f.afterList = nil
		f.service.Version.Index++
		f.setTask(swarmtypes.TaskStateFailed, 1)
	}
	_, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionFailed || f.updates != 0 {
		t.Fatalf("run = %+v %v", observed, err)
	}
}

func TestJobCancellationBeforeUpdateDoesNotMutate(t *testing.T) {
	f := newJobFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.afterList = cancel
	_, _, err := jobRunnerForTest(t, f).Run(ctx, f.service.ID, jobRepository, jobDigest)
	if !errors.Is(err, context.Canceled) || f.updates != 0 {
		t.Fatalf("err=%v updates=%d", err, f.updates)
	}
}

func TestJobRejectsReplacementBeforeFirstObservation(t *testing.T) {
	f := newJobFake()
	f.complete = false
	f.afterUpdate = func() {
		f.afterUpdate = nil
		// An operator starts a new run of the same image before Swarrow inspects it.
		spec := f.service.Spec
		spec.TaskTemplate.ForceUpdate++
		f.complete = true
		_, err := f.ServiceUpdate(context.Background(), f.service.ID, clienttypes.ServiceUpdateOptions{
			Version: f.service.Version, Spec: spec,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	result, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || result.Action != ActionUpdated || observed.Conclusion != ConclusionSuperseded || f.updates != 2 {
		t.Fatalf("run = %+v %+v %v updates=%d", result, observed, err, f.updates)
	}
}

func TestJobRequiresPreviousSpecBeforeAdoptingUpdatedIteration(t *testing.T) {
	f := newJobFake()
	f.afterUpdate = func() { f.service.PreviousSpec = nil }
	_, observed, err := jobRunnerForTest(t, f).Run(context.Background(), f.service.ID, jobRepository, jobDigest)
	if err != nil || observed.Conclusion != ConclusionSuperseded || f.updates != 1 {
		t.Fatalf("run = %+v %v updates=%d", observed, err, f.updates)
	}
}
