package deploy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/replay"
	"github.com/aide-tools/swarrow/internal/swarm"
)

type fakeJobs struct {
	runs    atomic.Int32
	run     func(context.Context, string, string, string) (swarm.Result, swarm.Observation, error)
	retry   func(context.Context, string, string, string, uint64) (swarm.Result, swarm.Observation, error)
	observe func(context.Context, swarm.Target) (swarm.Observation, error)
}

func (jobs *fakeJobs) Run(ctx context.Context, service, image, digest string) (swarm.Result, swarm.Observation, error) {
	jobs.runs.Add(1)
	return jobs.run(ctx, service, image, digest)
}
func (jobs *fakeJobs) Retry(ctx context.Context, service, image, digest string, version uint64) (swarm.Result, swarm.Observation, error) {
	return jobs.retry(ctx, service, image, digest, version)
}
func (jobs *fakeJobs) Observe(ctx context.Context, target swarm.Target) (swarm.Observation, error) {
	return jobs.observe(ctx, target)
}

func newJobCoordinator(t *testing.T, jobs *fakeJobs) *Coordinator {
	t.Helper()
	configuration := testConfiguration("example-web")
	configuration.Deployments[0].Target.Mode = "job"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	coordinator, err := newCoordinator(ctx, configuration, policy.New(configuration), &fakeUpdater{}, &fakeObserver{}, jobs, 1, 32, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func TestJobDeploymentAuthorisesBindsAndCachesResult(t *testing.T) {
	jobs := &fakeJobs{run: func(_ context.Context, service, image, digest string) (swarm.Result, swarm.Observation, error) {
		if service != "example_web" || image != "ghcr.io/example/example-web" || digest != testDigest {
			t.Errorf("wrong target: %s %s %s", service, image, digest)
		}
		return updatedResult(), swarm.Observation{Conclusion: swarm.ConclusionCompleted}, nil
	}}
	coordinator := newJobCoordinator(t, jobs)
	denied := testRequest("denied", "example-web", testDigest)
	denied.Claims.RepositoryID = "other"
	if _, err := coordinator.Deploy(context.Background(), denied); !errors.Is(err, policy.ErrDenied) || jobs.runs.Load() != 0 {
		t.Fatalf("denied=%v runs=%d", err, jobs.runs.Load())
	}
	request := testRequest("token", "example-web", testDigest)
	for range 2 {
		outcome, err := coordinator.Deploy(context.Background(), request)
		if err != nil || outcome.Action != ActionUpdated || outcome.Conclusion != swarm.ConclusionCompleted {
			t.Fatalf("outcome=%+v err=%v", outcome, err)
		}
	}
	request.Digest = "different"
	if _, err := coordinator.Deploy(context.Background(), request); !errors.Is(err, replay.ErrReused) {
		t.Fatalf("reused=%v", err)
	}
	if jobs.runs.Load() != 1 {
		t.Fatalf("runs=%d", jobs.runs.Load())
	}
}

func TestJobRetryResumesTheRecordedIteration(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "observer error"}[interrupted], func(t *testing.T) {
			iteration := uint64(9)
			jobs := &fakeJobs{run: func(context.Context, string, string, string) (swarm.Result, swarm.Observation, error) {
				var err error
				if interrupted {
					err = errors.New("connection lost")
				}
				return updatedResult(), swarm.Observation{Conclusion: swarm.ConclusionInProgress, JobIteration: &iteration}, err
			}}
			jobs.observe = func(_ context.Context, target swarm.Target) (swarm.Observation, error) {
				if target.ServiceID != "service-id" || target.Image != desiredImage() || target.JobIteration == nil || *target.JobIteration != iteration {
					t.Errorf("target=%+v", target)
				}
				return swarm.Observation{Conclusion: swarm.ConclusionSuperseded}, nil
			}
			coordinator := newJobCoordinator(t, jobs)
			request := testRequest("token", "example-web", testDigest)
			_, _ = coordinator.Deploy(context.Background(), request)
			outcome, err := coordinator.Deploy(context.Background(), request)
			if err != nil || outcome.Conclusion != swarm.ConclusionSuperseded || jobs.runs.Load() != 1 {
				t.Fatalf("retry=%+v %v runs=%d", outcome, err, jobs.runs.Load())
			}
		})
	}
}

func TestJobUncertainSubmissionUsesConditionalRetry(t *testing.T) {
	jobs := &fakeJobs{run: func(context.Context, string, string, string) (swarm.Result, swarm.Observation, error) {
		result := updatedResult()
		result.Action = swarm.ActionIndeterminate
		return result, swarm.Observation{}, errors.New("lost update response")
	}}
	jobs.retry = func(_ context.Context, service, image, digest string, version uint64) (swarm.Result, swarm.Observation, error) {
		if service != "example_web" || image != "ghcr.io/example/example-web" || digest != testDigest || version != 42 {
			t.Errorf("unexpected conditional retry")
		}
		return swarm.Result{}, swarm.Observation{Conclusion: swarm.ConclusionSuperseded}, swarm.ErrConflict
	}
	coordinator := newJobCoordinator(t, jobs)
	request := testRequest("token", "example-web", testDigest)
	_, _ = coordinator.Deploy(context.Background(), request)
	outcome, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, swarm.ErrConflict) || outcome.Conclusion != swarm.ConclusionSuperseded || jobs.runs.Load() != 1 {
		t.Fatalf("retry=%+v %v", outcome, err)
	}
}

func TestJobBusyDoesNotCacheThePreviousRunsObservation(t *testing.T) {
	jobs := &fakeJobs{run: func(context.Context, string, string, string) (swarm.Result, swarm.Observation, error) {
		iteration := uint64(5)
		return swarm.Result{}, swarm.Observation{Conclusion: swarm.ConclusionInProgress, JobIteration: &iteration}, swarm.ErrJobBusy
	}}
	coordinator := newJobCoordinator(t, jobs)
	request := testRequest("token", "example-web", testDigest)
	for range 2 {
		outcome, err := coordinator.Deploy(context.Background(), request)
		if !errors.Is(err, swarm.ErrJobBusy) || outcome.Action != ActionNotApplied || outcome.Conclusion != "" || outcome.JobIteration != nil {
			t.Fatalf("outcome=%+v %v", outcome, err)
		}
	}
	if jobs.runs.Load() != 1 {
		t.Fatalf("runs=%d", jobs.runs.Load())
	}
}

func TestJobRequestsUseTheBoundedServiceQueue(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	jobs := &fakeJobs{run: func(ctx context.Context, _, _, _ string) (swarm.Result, swarm.Observation, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return updatedResult(), swarm.Observation{Conclusion: swarm.ConclusionCompleted}, nil
	}}
	coordinator := newJobCoordinator(t, jobs)
	results, errs := make(chan Outcome, 2), make(chan error, 2)
	go deployAsync(coordinator, context.Background(), testRequest("first", "example-web", testDigest), results, errs)
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go deployAsync(coordinator, ctx, testRequest("queued", "example-web", testDigest), results, errs)
	waitForQueueLength(t, coordinator.workers["example_web"], 1)
	if _, err := coordinator.Deploy(context.Background(), testRequest("excess", "example-web", testDigest)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue=%v", err)
	}
	cancel()
	outcome := <-results
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%+v %v", outcome, err)
	}
	// The cancelled caller may return before the worker removes its request.
	outcome, err := coordinator.Deploy(context.Background(), testRequest("queued", "example-web", testDigest))
	if !errors.Is(err, context.Canceled) || outcome.Action != ActionNotApplied {
		t.Fatalf("queued=%+v %v", outcome, err)
	}
	close(release)
	<-results
	<-errs
	if jobs.runs.Load() != 1 {
		t.Fatalf("runs=%d", jobs.runs.Load())
	}
}

func TestJobConfigurationRequiresARunner(t *testing.T) {
	configuration := testConfiguration("example-web")
	configuration.Deployments[0].Target = config.Target{Mode: "job", Service: "migration", Image: "ghcr.io/example/app"}
	if _, err := New(context.Background(), configuration, policy.New(configuration), &fakeUpdater{}, &fakeObserver{}, nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("New=%v", err)
	}
}
