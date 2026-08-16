package deploy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/githuboidc"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/replay"
	"github.com/aide-tools/swarrow/internal/swarm"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNewCreatesAValidatedCoordinator(t *testing.T) {
	configuration := testConfiguration("example-web")
	rootCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	coordinator, err := New(rootCtx, configuration, policy.New(configuration), &fakeUpdater{}, &fakeObserver{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if coordinator == nil {
		t.Fatal("New() coordinator = nil")
	}
}

func TestDeployAppliesAndObservesAnAuthorisedRequest(t *testing.T) {
	configuration := testConfiguration("example-web")
	updater := &fakeUpdater{
		apply: func(_ context.Context, service string, repository string, digest string) (swarm.Result, error) {
			if service != "example_web" || repository != "ghcr.io/example/example-web" || digest != testDigest {
				t.Errorf("Apply() target = (%q, %q, %q), want configured target and request digest", service, repository, digest)
			}
			return updatedResult(), nil
		},
	}
	observer := &fakeObserver{
		observe: func(_ context.Context, target swarm.Target) (swarm.Observation, error) {
			if target.ServiceID != "service-id" || target.Image != desiredImage() || target.RolloutVersion != nil || target.RolloutStartedAt != nil {
				t.Errorf("Observe() target = %#v, want established update", target)
			}
			return swarm.Observation{Conclusion: swarm.ConclusionCompleted, Message: "completed"}, nil
		},
	}
	coordinator := newTestCoordinator(t, configuration, updater, observer, 1)

	outcome, err := coordinator.Deploy(context.Background(), testRequest("token-1", "example-web", testDigest))
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if outcome.Deployment != "example-web" || outcome.Action != ActionUpdated || outcome.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Deploy() outcome = %#v, want completed update", outcome)
	}
}

func TestDeployRejectsUnauthorisedRequestsBeforeDocker(t *testing.T) {
	configuration := testConfiguration("example-web")
	updater := &fakeUpdater{}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{}, 1)
	request := testRequest("token-1", "example-web", testDigest)
	request.Claims.RepositoryID = "other"

	outcome, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("Deploy() error = %v, want ErrDenied", err)
	}
	if outcome.Action != ActionNotApplied {
		t.Errorf("Deploy() action = %q, want %q", outcome.Action, ActionNotApplied)
	}
	if updater.applyCallCount() != 0 {
		t.Errorf("Apply() calls = %d, want 0", updater.applyCallCount())
	}
}

func TestDeployRejectsRequestsAfterCoordinatorShutdown(t *testing.T) {
	configuration := testConfiguration("example-web")
	rootCtx, cancel := context.WithCancel(context.Background())
	coordinator, err := newCoordinator(rootCtx, configuration, policy.New(configuration), &fakeUpdater{}, &fakeObserver{}, 1, 32, time.Now)
	if err != nil {
		t.Fatalf("newCoordinator() error = %v", err)
	}
	cancel()

	outcome, err := coordinator.Deploy(context.Background(), testRequest("token-1", "example-web", testDigest))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want context.Canceled", err)
	}
	if outcome.Action != ActionNotApplied {
		t.Errorf("Deploy() action = %q, want not_applied", outcome.Action)
	}
}

func TestCoordinatorShutdownCancelsQueuedRequestsWithoutApplyingThem(t *testing.T) {
	configuration := testConfiguration("example-web")
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	started := make(chan struct{})
	updater := &fakeUpdater{apply: func(ctx context.Context, _ string, _ string, _ string) (swarm.Result, error) {
		close(started)
		<-ctx.Done()
		return swarm.Result{}, ctx.Err()
	}}
	coordinator, err := newCoordinator(rootCtx, configuration, policy.New(configuration), updater, &fakeObserver{}, 1, 32, time.Now)
	if err != nil {
		t.Fatalf("newCoordinator() error = %v", err)
	}

	results := make(chan Outcome, 2)
	errorsChannel := make(chan error, 2)
	go deployAsync(coordinator, context.Background(), testRequest("token-1", "example-web", testDigest), results, errorsChannel)
	<-started
	go deployAsync(coordinator, context.Background(), testRequest("token-2", "example-web", testDigest), results, errorsChannel)
	waitForQueueLength(t, coordinator.workers["example_web"], 1)

	cancelRoot()
	for range 2 {
		if outcome := <-results; outcome.Action != ActionNotApplied {
			t.Errorf("Deploy() action = %q, want not_applied", outcome.Action)
		}
		if err := <-errorsChannel; !errors.Is(err, context.Canceled) {
			t.Errorf("Deploy() error = %v, want context.Canceled", err)
		}
	}
	if updater.applyCallCount() != 1 {
		t.Errorf("Apply() calls = %d, want 1", updater.applyCallCount())
	}
}

func TestDeploySharesOneOperationAcrossConcurrentExactRetries(t *testing.T) {
	configuration := testConfiguration("example-web")
	started := make(chan struct{})
	release := make(chan struct{})
	updater := &fakeUpdater{
		apply: func(ctx context.Context, _ string, _ string, _ string) (swarm.Result, error) {
			close(started)
			select {
			case <-release:
				return updatedResult(), nil
			case <-ctx.Done():
				return swarm.Result{}, ctx.Err()
			}
		},
	}
	observer := &fakeObserver{observe: completedObservation}
	coordinator := newTestCoordinator(t, configuration, updater, observer, 2)
	request := testRequest("token-1", "example-web", testDigest)

	results := make(chan Outcome, 2)
	errorsChannel := make(chan error, 2)
	go deployAsync(coordinator, context.Background(), request, results, errorsChannel)
	<-started
	go deployAsync(coordinator, context.Background(), request, results, errorsChannel)
	close(release)

	for range 2 {
		if err := <-errorsChannel; err != nil {
			t.Fatalf("Deploy() error = %v", err)
		}
		if outcome := <-results; outcome.Conclusion != swarm.ConclusionCompleted {
			t.Errorf("Deploy() conclusion = %q, want completed", outcome.Conclusion)
		}
	}
	if updater.applyCallCount() != 1 {
		t.Errorf("Apply() calls = %d, want 1", updater.applyCallCount())
	}
}

func TestDeployRejectsChangedTokenReuse(t *testing.T) {
	configuration := testConfiguration("example-web")
	updater := &fakeUpdater{apply: func(context.Context, string, string, string) (swarm.Result, error) {
		return updatedResult(), nil
	}}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{observe: completedObservation}, 1)
	request := testRequest("token-1", "example-web", testDigest)
	if _, err := coordinator.Deploy(context.Background(), request); err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}
	request.Digest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	_, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, replay.ErrReused) {
		t.Fatalf("second Deploy() error = %v, want ErrReused", err)
	}
}

func TestDeployFailsClosedWhenAServiceQueueIsFull(t *testing.T) {
	configuration := testConfiguration("example-web")
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	updater := &fakeUpdater{apply: func(ctx context.Context, _ string, _ string, _ string) (swarm.Result, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return updatedResult(), nil
		case <-ctx.Done():
			return swarm.Result{}, ctx.Err()
		}
	}}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{observe: completedObservation}, 1)

	results := make(chan Outcome, 2)
	errorsChannel := make(chan error, 2)
	go deployAsync(coordinator, context.Background(), testRequest("token-1", "example-web", testDigest), results, errorsChannel)
	<-started
	go deployAsync(coordinator, context.Background(), testRequest("token-2", "example-web", testDigest), results, errorsChannel)
	waitForQueueLength(t, coordinator.workers["example_web"], 1)

	outcome, err := coordinator.Deploy(context.Background(), testRequest("token-3", "example-web", testDigest))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third Deploy() error = %v, want ErrQueueFull", err)
	}
	if outcome.Action != ActionNotApplied {
		t.Errorf("third Deploy() action = %q, want not_applied", outcome.Action)
	}
	close(release)
	for range 2 {
		<-results
		if err := <-errorsChannel; err != nil {
			t.Fatalf("queued Deploy() error = %v", err)
		}
	}
}

func TestServiceWorkerReclaimsCancelledEntriesBeforeCheckingCapacity(t *testing.T) {
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	t.Cleanup(cancelRoot)
	cache, err := replay.New[Outcome](2, time.Now)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	firstOperation, _, err := cache.Bind(replay.Binding{
		TokenID: "token-1", Deployment: "example-web", Digest: testDigest, ValidUntil: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("first Bind() error = %v", err)
	}
	secondOperation, _, err := cache.Bind(replay.Binding{
		TokenID: "token-2", Deployment: "example-web", Digest: testDigest, ValidUntil: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second Bind() error = %v", err)
	}

	cancelledCtx, cancelQueued := context.WithCancel(context.Background())
	cancelQueued()
	cancelled := &job{
		ctx:         cancelledCtx,
		deployment:  policy.Deployment{Name: "example-web"},
		operation:   firstOperation,
		stopRemoval: func() bool { return true },
	}
	replacementCtx, cancelReplacement := context.WithCancel(context.Background())
	t.Cleanup(cancelReplacement)
	replacement := &job{
		ctx:        replacementCtx,
		deployment: policy.Deployment{Name: "example-web"},
		operation:  secondOperation,
	}
	worker := &serviceWorker{
		ctx:      rootCtx,
		capacity: 1,
		wake:     make(chan struct{}, 1),
		queue:    []*job{cancelled},
	}

	if err := worker.enqueue(replacement); err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.queue) != 1 || worker.queue[0] != replacement {
		t.Errorf("queue = %#v, want only replacement", worker.queue)
	}
}

func TestDeployDoesNotApplyAQueuedRequestAfterItsContextExpires(t *testing.T) {
	configuration := testConfiguration("example-web")
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	updater := &fakeUpdater{apply: func(ctx context.Context, _ string, _ string, _ string) (swarm.Result, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return updatedResult(), nil
		case <-ctx.Done():
			return swarm.Result{}, ctx.Err()
		}
	}}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{observe: completedObservation}, 1)

	firstDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Deploy(context.Background(), testRequest("token-1", "example-web", testDigest))
		firstDone <- err
	}()
	<-started
	queuedCtx, cancel := context.WithCancel(context.Background())
	queuedDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Deploy(queuedCtx, testRequest("token-2", "example-web", testDigest))
		queuedDone <- err
	}()
	waitForQueueLength(t, coordinator.workers["example_web"], 1)
	cancel()
	if err := <-queuedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued Deploy() error = %v, want context.Canceled", err)
	}
	waitForQueueLength(t, coordinator.workers["example_web"], 0)

	thirdDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Deploy(context.Background(), testRequest("token-3", "example-web", testDigest))
		thirdDone <- err
	}()
	waitForQueueLength(t, coordinator.workers["example_web"], 1)
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}
	if err := <-thirdDone; err != nil {
		t.Fatalf("third Deploy() error = %v", err)
	}
	waitForApplyCalls(t, updater, 2)
}

func TestDeployEstablishesAnAcceptedIndeterminateUpdate(t *testing.T) {
	configuration := testConfiguration("example-web")
	updateError := errors.New("connection closed")
	updater := &fakeUpdater{
		apply: func(context.Context, string, string, string) (swarm.Result, error) {
			result := updatedResult()
			result.Action = swarm.ActionIndeterminate
			return result, updateError
		},
		inspect: func(context.Context, string, string, string) (swarm.Inspection, error) {
			return swarm.Inspection{ServiceID: "service-id", Image: desiredImage(), Desired: true, Version: 43}, nil
		},
	}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{observe: completedObservation}, 1)

	outcome, err := coordinator.Deploy(context.Background(), testRequest("token-1", "example-web", testDigest))
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if outcome.Action != ActionUpdated || outcome.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("Deploy() outcome = %#v, want established completed update", outcome)
	}
}

func TestDeployPreservesADefinitiveUpdateRejection(t *testing.T) {
	configuration := testConfiguration("example-web")
	updateError := errors.New("invalid update")
	updater := &fakeUpdater{apply: func(context.Context, string, string, string) (swarm.Result, error) {
		result := updatedResult()
		result.Action = swarm.ActionRejected
		return result, updateError
	}}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{}, 1)

	outcome, err := coordinator.Deploy(context.Background(), testRequest("token-1", "example-web", testDigest))
	if !errors.Is(err, updateError) {
		t.Fatalf("Deploy() error = %v, want update error", err)
	}
	if outcome.Action != ActionRejected {
		t.Errorf("Deploy() action = %q, want %q", outcome.Action, ActionRejected)
	}
}

func TestExactRetryResumesAnIndeterminateOperationAtItsOriginalVersion(t *testing.T) {
	configuration := testConfiguration("example-web")
	updateError := errors.New("connection closed")
	updater := &fakeUpdater{
		apply: func(context.Context, string, string, string) (swarm.Result, error) {
			result := updatedResult()
			result.Action = swarm.ActionIndeterminate
			return result, updateError
		},
		inspect: func(context.Context, string, string, string) (swarm.Inspection, error) {
			return swarm.Inspection{ServiceID: "service-id", Image: desiredImage(), Desired: false, Version: 42}, nil
		},
		retry: func(_ context.Context, _ string, _ string, _ string, version uint64) (swarm.Result, error) {
			if version != 42 {
				t.Errorf("Retry() version = %d, want 42", version)
			}
			return updatedResult(), nil
		},
	}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{observe: completedObservation}, 1)
	request := testRequest("token-1", "example-web", testDigest)

	first, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, updateError) || first.Action != ActionIndeterminate {
		t.Fatalf("first Deploy() = (%#v, %v), want indeterminate update", first, err)
	}
	second, err := coordinator.Deploy(context.Background(), request)
	if err != nil {
		t.Fatalf("second Deploy() error = %v", err)
	}
	if second.Action != ActionUpdated || second.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("second Deploy() outcome = %#v, want completed retry", second)
	}
	if updater.retryCallCount() != 1 {
		t.Errorf("Retry() calls = %d, want 1", updater.retryCallCount())
	}
}

func TestExactRetryPreservesAnIndeterminateOutcomeWhenTheQueueIsFull(t *testing.T) {
	configuration := testConfiguration("example-web")
	updateError := errors.New("connection closed")
	started := make(chan struct{})
	release := make(chan struct{})
	var applyCalls atomic.Int32
	updater := &fakeUpdater{
		apply: func(ctx context.Context, _ string, _ string, _ string) (swarm.Result, error) {
			switch applyCalls.Add(1) {
			case 1:
				result := updatedResult()
				result.Action = swarm.ActionIndeterminate
				return result, updateError
			case 2:
				close(started)
				select {
				case <-release:
					return updatedResult(), nil
				case <-ctx.Done():
					return swarm.Result{}, ctx.Err()
				}
			default:
				return updatedResult(), nil
			}
		},
		inspect: func(context.Context, string, string, string) (swarm.Inspection, error) {
			return swarm.Inspection{ServiceID: "service-id", Desired: false, Version: 42}, nil
		},
		retry: func(context.Context, string, string, string, uint64) (swarm.Result, error) {
			return updatedResult(), nil
		},
	}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{observe: completedObservation}, 1)
	request := testRequest("token-1", "example-web", testDigest)

	first, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, updateError) || first.Action != ActionIndeterminate {
		t.Fatalf("first Deploy() = (%#v, %v), want indeterminate update", first, err)
	}

	results := make(chan Outcome, 2)
	errorsChannel := make(chan error, 2)
	go deployAsync(coordinator, context.Background(), testRequest("token-2", "example-web", testDigest), results, errorsChannel)
	<-started
	go deployAsync(coordinator, context.Background(), testRequest("token-3", "example-web", testDigest), results, errorsChannel)
	waitForQueueLength(t, coordinator.workers["example_web"], 1)

	second, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, ErrQueueFull) || second.Action != ActionIndeterminate {
		t.Fatalf("second Deploy() = (%#v, %v), want preserved indeterminate outcome and ErrQueueFull", second, err)
	}

	close(release)
	for range 2 {
		<-results
		if err := <-errorsChannel; err != nil {
			t.Fatalf("queued Deploy() error = %v", err)
		}
	}

	third, err := coordinator.Deploy(context.Background(), request)
	if err != nil {
		t.Fatalf("third Deploy() error = %v", err)
	}
	if third.Action != ActionUpdated || third.Conclusion != swarm.ConclusionCompleted {
		t.Errorf("third Deploy() outcome = %#v, want completed retry", third)
	}
	if updater.retryCallCount() != 1 {
		t.Errorf("Retry() calls = %d, want 1", updater.retryCallCount())
	}
}

func TestExactRetryResumesObservationAfterATransientError(t *testing.T) {
	configuration := testConfiguration("example-web")
	updater := &fakeUpdater{apply: func(context.Context, string, string, string) (swarm.Result, error) {
		return updatedResult(), nil
	}}
	observationError := errors.New("inspect service: connection closed")
	var observationCalls int
	observer := &fakeObserver{observe: func(context.Context, swarm.Target) (swarm.Observation, error) {
		observationCalls++
		if observationCalls == 1 {
			return swarm.Observation{}, observationError
		}
		return swarm.Observation{Conclusion: swarm.ConclusionCompleted}, nil
	}}
	coordinator := newTestCoordinator(t, configuration, updater, observer, 1)
	request := testRequest("token-1", "example-web", testDigest)

	first, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, observationError) || first.Action != ActionUpdated || first.Conclusion != "" {
		t.Fatalf("first Deploy() = (%#v, %v), want accepted update with observation error", first, err)
	}
	second, err := coordinator.Deploy(context.Background(), request)
	if err != nil || second.Conclusion != swarm.ConclusionCompleted {
		t.Fatalf("second Deploy() = (%#v, %v), want completed observation", second, err)
	}
	if updater.applyCallCount() != 1 {
		t.Errorf("Apply() calls = %d, want 1", updater.applyCallCount())
	}
	if observationCalls != 2 {
		t.Errorf("Observe() calls = %d, want 2", observationCalls)
	}
}

func TestExactRetryPreservesRolloutIdentityAcrossATransientObservationError(t *testing.T) {
	configuration := testConfiguration("example-web")
	updater := &fakeUpdater{apply: func(context.Context, string, string, string) (swarm.Result, error) {
		return updatedResult(), nil
	}}
	rolloutStartedAt := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	observationError := errors.New("inspect service: connection closed")
	var observationCalls int
	observer := &fakeObserver{observe: func(_ context.Context, target swarm.Target) (swarm.Observation, error) {
		observationCalls++
		switch observationCalls {
		case 1:
			return swarm.Observation{
				Conclusion:       swarm.ConclusionInProgress,
				RolloutStartedAt: &rolloutStartedAt,
			}, nil
		case 2:
			if target.RolloutStartedAt == nil || !target.RolloutStartedAt.Equal(rolloutStartedAt) {
				t.Errorf("second Observe() rollout start = %v, want %v", target.RolloutStartedAt, rolloutStartedAt)
			}
			return swarm.Observation{}, observationError
		default:
			if target.RolloutStartedAt == nil || !target.RolloutStartedAt.Equal(rolloutStartedAt) {
				t.Errorf("third Observe() rollout start = %v, want %v", target.RolloutStartedAt, rolloutStartedAt)
			}
			return swarm.Observation{Conclusion: swarm.ConclusionCompleted}, nil
		}
	}}
	coordinator := newTestCoordinator(t, configuration, updater, observer, 1)
	request := testRequest("token-1", "example-web", testDigest)

	first, err := coordinator.Deploy(context.Background(), request)
	if err != nil || first.Conclusion != swarm.ConclusionInProgress {
		t.Fatalf("first Deploy() = (%#v, %v), want in-progress rollout", first, err)
	}
	second, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, observationError) || second.Conclusion != swarm.ConclusionInProgress ||
		second.RolloutStartedAt == nil || !second.RolloutStartedAt.Equal(rolloutStartedAt) {
		t.Fatalf("second Deploy() = (%#v, %v), want preserved in-progress rollout", second, err)
	}
	third, err := coordinator.Deploy(context.Background(), request)
	if err != nil || third.Conclusion != swarm.ConclusionCompleted {
		t.Fatalf("third Deploy() = (%#v, %v), want completed observation", third, err)
	}
	if updater.applyCallCount() != 1 {
		t.Errorf("Apply() calls = %d, want 1", updater.applyCallCount())
	}
}

func TestExactRetryResumesInProgressRolloutObservation(t *testing.T) {
	rolloutVersion := uint64(42)
	rolloutStartedAt := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		first        swarm.Observation
		assertResume func(*testing.T, swarm.Target)
	}{
		"service version": {
			first: swarm.Observation{Conclusion: swarm.ConclusionInProgress, RolloutVersion: &rolloutVersion},
			assertResume: func(t *testing.T, target swarm.Target) {
				if target.RolloutVersion == nil || *target.RolloutVersion != rolloutVersion || target.RolloutStartedAt != nil {
					t.Errorf("rollout identity = (%v, %v), want version %d", target.RolloutVersion, target.RolloutStartedAt, rolloutVersion)
				}
			},
		},
		"rollout start": {
			first: swarm.Observation{Conclusion: swarm.ConclusionInProgress, RolloutStartedAt: &rolloutStartedAt},
			assertResume: func(t *testing.T, target swarm.Target) {
				if target.RolloutVersion != nil || target.RolloutStartedAt == nil || !target.RolloutStartedAt.Equal(rolloutStartedAt) {
					t.Errorf("rollout identity = (%v, %v), want start %v", target.RolloutVersion, target.RolloutStartedAt, rolloutStartedAt)
				}
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			configuration := testConfiguration("example-web")
			updater := &fakeUpdater{apply: func(context.Context, string, string, string) (swarm.Result, error) {
				return updatedResult(), nil
			}}
			var observationCalls int
			observer := &fakeObserver{observe: func(_ context.Context, target swarm.Target) (swarm.Observation, error) {
				observationCalls++
				if target.ServiceID != "service-id" || target.Image != desiredImage() {
					t.Errorf("Observe() target = %#v, want original established update", target)
				}
				if observationCalls == 1 {
					if target.RolloutVersion != nil || target.RolloutStartedAt != nil {
						t.Errorf("first Observe() rollout identity = (%v, %v), want none", target.RolloutVersion, target.RolloutStartedAt)
					}
					return test.first, nil
				}
				test.assertResume(t, target)
				return swarm.Observation{Conclusion: swarm.ConclusionCompleted}, nil
			}}
			coordinator := newTestCoordinator(t, configuration, updater, observer, 1)
			request := testRequest("token-1", "example-web", testDigest)

			first, err := coordinator.Deploy(context.Background(), request)
			if err != nil || first.Conclusion != swarm.ConclusionInProgress {
				t.Fatalf("first Deploy() = (%#v, %v), want in-progress rollout", first, err)
			}
			second, err := coordinator.Deploy(context.Background(), request)
			if err != nil || second.Conclusion != swarm.ConclusionCompleted {
				t.Fatalf("second Deploy() = (%#v, %v), want completed resumed observation", second, err)
			}
			if updater.applyCallCount() != 1 {
				t.Errorf("Apply() calls = %d, want 1", updater.applyCallCount())
			}
			if observationCalls != 2 {
				t.Errorf("Observe() calls = %d, want 2", observationCalls)
			}
		})
	}
}

func TestExactRetryDoesNotOverwriteAServiceThatAdvancedElsewhere(t *testing.T) {
	configuration := testConfiguration("example-web")
	updateError := errors.New("connection closed")
	updater := &fakeUpdater{
		apply: func(context.Context, string, string, string) (swarm.Result, error) {
			result := updatedResult()
			result.Action = swarm.ActionIndeterminate
			return result, updateError
		},
		inspect: func(context.Context, string, string, string) (swarm.Inspection, error) {
			return swarm.Inspection{ServiceID: "service-id", Desired: false, Version: 43}, nil
		},
		retry: func(context.Context, string, string, string, uint64) (swarm.Result, error) {
			return swarm.Result{ServiceID: "service-id", Image: desiredImage(), InspectedVersion: 43}, swarm.ErrConflict
		},
	}
	coordinator := newTestCoordinator(t, configuration, updater, &fakeObserver{}, 1)
	request := testRequest("token-1", "example-web", testDigest)
	if _, err := coordinator.Deploy(context.Background(), request); !errors.Is(err, updateError) {
		t.Fatalf("first Deploy() error = %v, want update error", err)
	}

	outcome, err := coordinator.Deploy(context.Background(), request)
	if !errors.Is(err, swarm.ErrConflict) {
		t.Fatalf("second Deploy() error = %v, want ErrConflict", err)
	}
	if outcome.Action != ActionNotApplied || outcome.Conclusion != swarm.ConclusionSuperseded {
		t.Errorf("second Deploy() outcome = %#v, want superseded without mutation", outcome)
	}
}

func newTestCoordinator(t *testing.T, configuration config.Config, updater *fakeUpdater, observer *fakeObserver, queueCapacity int) *Coordinator {
	t.Helper()
	rootCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	coordinator, err := newCoordinator(rootCtx, configuration, policy.New(configuration), updater, observer, queueCapacity, 32, time.Now)
	if err != nil {
		t.Fatalf("newCoordinator() error = %v", err)
	}
	return coordinator
}

func testConfiguration(names ...string) config.Config {
	configuration := config.Config{
		Version: 1,
		Server: config.Server{
			Listen:         "127.0.0.1:8080",
			RequestTimeout: 5 * time.Minute,
		},
		GitHub: config.GitHub{Audience: "https://deploy.example.net"},
	}
	for _, name := range names {
		configuration.Deployments = append(configuration.Deployments, config.Deployment{
			Name: name,
			Identity: config.Identity{
				RepositoryID: "123456789",
				WorkflowRef:  "example/example-web/.github/workflows/deploy.yml@refs/heads/main",
				Environment:  "production",
			},
			Target: config.Target{
				Service: "example_web",
				Image:   "ghcr.io/example/example-web",
			},
		})
	}
	return configuration
}

func testRequest(tokenID string, deployment string, digest string) Request {
	return Request{
		Deployment: deployment,
		Digest:     digest,
		Claims: githuboidc.Claims{
			TokenID:      tokenID,
			RepositoryID: "123456789",
			WorkflowRef:  "example/example-web/.github/workflows/deploy.yml@refs/heads/main",
			Environment:  "production",
			ValidUntil:   time.Now().Add(time.Hour),
		},
	}
}

func updatedResult() swarm.Result {
	return swarm.Result{
		Action:           swarm.ActionUpdated,
		ServiceID:        "service-id",
		Image:            desiredImage(),
		InspectedVersion: 42,
	}
}

func desiredImage() string {
	return "ghcr.io/example/example-web@" + testDigest
}

func completedObservation(context.Context, swarm.Target) (swarm.Observation, error) {
	return swarm.Observation{Conclusion: swarm.ConclusionCompleted}, nil
}

func deployAsync(coordinator *Coordinator, ctx context.Context, request Request, results chan<- Outcome, errorsChannel chan<- error) {
	outcome, err := coordinator.Deploy(ctx, request)
	results <- outcome
	errorsChannel <- err
}

func waitForQueueLength(t *testing.T, worker *serviceWorker, length int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		worker.mu.Lock()
		actual := len(worker.queue)
		worker.mu.Unlock()
		if actual == length {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue length = %d, want %d", actual, length)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForApplyCalls(t *testing.T, updater *fakeUpdater, calls int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for updater.applyCallCount() != calls {
		if time.Now().After(deadline) {
			t.Fatalf("Apply() calls = %d, want %d", updater.applyCallCount(), calls)
		}
		time.Sleep(time.Millisecond)
	}
}

type fakeUpdater struct {
	mu           sync.Mutex
	applyCalls   int
	retryCalls   int
	inspectCalls int
	apply        func(context.Context, string, string, string) (swarm.Result, error)
	retry        func(context.Context, string, string, string, uint64) (swarm.Result, error)
	inspect      func(context.Context, string, string, string) (swarm.Inspection, error)
}

func (updater *fakeUpdater) Apply(ctx context.Context, service string, repository string, digest string) (swarm.Result, error) {
	updater.mu.Lock()
	updater.applyCalls++
	implementation := updater.apply
	updater.mu.Unlock()
	if implementation == nil {
		return swarm.Result{}, errors.New("unexpected Apply call")
	}
	return implementation(ctx, service, repository, digest)
}

func (updater *fakeUpdater) Retry(ctx context.Context, service string, repository string, digest string, version uint64) (swarm.Result, error) {
	updater.mu.Lock()
	updater.retryCalls++
	implementation := updater.retry
	updater.mu.Unlock()
	if implementation == nil {
		return swarm.Result{}, errors.New("unexpected Retry call")
	}
	return implementation(ctx, service, repository, digest, version)
}

func (updater *fakeUpdater) Inspect(ctx context.Context, service string, repository string, digest string) (swarm.Inspection, error) {
	updater.mu.Lock()
	updater.inspectCalls++
	implementation := updater.inspect
	updater.mu.Unlock()
	if implementation == nil {
		return swarm.Inspection{}, errors.New("unexpected Inspect call")
	}
	return implementation(ctx, service, repository, digest)
}

func (updater *fakeUpdater) applyCallCount() int {
	updater.mu.Lock()
	defer updater.mu.Unlock()
	return updater.applyCalls
}

func (updater *fakeUpdater) retryCallCount() int {
	updater.mu.Lock()
	defer updater.mu.Unlock()
	return updater.retryCalls
}

type fakeObserver struct {
	observe func(context.Context, swarm.Target) (swarm.Observation, error)
}

func (observer *fakeObserver) Observe(ctx context.Context, target swarm.Target) (swarm.Observation, error) {
	if observer.observe == nil {
		return swarm.Observation{}, errors.New("unexpected Observe call")
	}
	return observer.observe(ctx, target)
}
