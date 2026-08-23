// Package deploy coordinates authorised, replay-safe deployment requests.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/githuboidc"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/replay"
	"github.com/aide-tools/swarrow/internal/swarm"
)

const (
	defaultQueueCapacity  = 16
	defaultReplayCapacity = 1024
)

var (
	// ErrInvalidConfiguration identifies a coordinator that cannot fail closed.
	ErrInvalidConfiguration = errors.New("invalid deployment coordinator configuration")
	// ErrQueueFull identifies a service queue that cannot accept another request.
	ErrQueueFull = errors.New("deployment queue is full")
)

// Action records what Swarrow established about the service mutation.
type Action string

const (
	ActionNotApplied    Action = "not_applied"
	ActionNoChange      Action = "no_change"
	ActionUpdated       Action = "updated"
	ActionRejected      Action = "rejected"
	ActionIndeterminate Action = "indeterminate"
)

// Request contains the authenticated caller and caller-selectable deployment input.
type Request struct {
	Deployment string
	Digest     string
	Claims     githuboidc.Claims
}

// Outcome records the mutation Swarrow made and the rollout state it observed.
type Outcome struct {
	Deployment       string
	Action           Action
	Conclusion       swarm.Conclusion
	ServiceID        string
	Image            string
	InspectedVersion uint64
	RolloutVersion   *uint64
	RolloutStartedAt *time.Time
	Message          string
	Warnings         []string
	Tasks            swarm.TaskSummary
}

type authoriser interface {
	Authorise(string, githuboidc.Claims) (policy.Deployment, error)
}

type imageUpdater interface {
	Apply(context.Context, string, string, string) (swarm.Result, error)
	Retry(context.Context, string, string, string, uint64) (swarm.Result, error)
	Inspect(context.Context, string, string, string) (swarm.Inspection, error)
}

type rolloutObserver interface {
	Observe(context.Context, swarm.Target) (swarm.Observation, error)
}

// Coordinator serialises deployment lifecycles per configured service.
type Coordinator struct {
	ctx      context.Context
	policy   authoriser
	replay   *replay.Cache[Outcome]
	updater  imageUpdater
	observer rolloutObserver
	workers  map[string]*serviceWorker
}

type job struct {
	ctx          context.Context
	deployment   policy.Deployment
	digest       string
	retryVersion *uint64
	resume       *Outcome
	previous     *Outcome
	operation    *replay.Operation[Outcome]
	stopRemoval  func() bool
}

type serviceWorker struct {
	ctx      context.Context
	capacity int
	wake     chan struct{}

	mu      sync.Mutex
	queue   []*job
	stopped bool
}

// New creates fixed-capacity workers for every validated service target.
func New(ctx context.Context, configuration config.Config, authoriser authoriser, updater imageUpdater, observer rolloutObserver) (*Coordinator, error) {
	return newCoordinator(ctx, configuration, authoriser, updater, observer, defaultQueueCapacity, defaultReplayCapacity, time.Now)
}

func newCoordinator(ctx context.Context, configuration config.Config, authoriser authoriser, updater imageUpdater, observer rolloutObserver, queueCapacity int, replayCapacity int, now func() time.Time) (*Coordinator, error) {
	if ctx == nil || authoriser == nil || updater == nil || observer == nil || queueCapacity <= 0 {
		return nil, ErrInvalidConfiguration
	}
	if err := config.Validate(configuration); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}

	replayCache, err := replay.New[Outcome](replayCapacity, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}

	coordinator := &Coordinator{
		ctx:      ctx,
		policy:   authoriser,
		replay:   replayCache,
		updater:  updater,
		observer: observer,
		workers:  make(map[string]*serviceWorker, len(configuration.Deployments)),
	}
	for _, deployment := range configuration.Deployments {
		worker := &serviceWorker{
			ctx:      ctx,
			capacity: queueCapacity,
			wake:     make(chan struct{}, 1),
		}
		coordinator.workers[deployment.Target.Service] = worker
		go coordinator.runWorker(worker)
	}

	return coordinator, nil
}

// Deploy authorises, binds and serialises one deployment request.
func (coordinator *Coordinator) Deploy(ctx context.Context, request Request) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return notApplied(request.Deployment), err
	}
	if err := coordinator.ctx.Err(); err != nil {
		return notApplied(request.Deployment), err
	}

	deployment, err := coordinator.policy.Authorise(request.Deployment, request.Claims)
	if err != nil {
		return notApplied(request.Deployment), err
	}

	binding := replay.Binding{
		TokenID:    request.Claims.TokenID,
		Deployment: deployment.Name,
		Digest:     request.Digest,
		ValidUntil: request.Claims.ValidUntil,
	}
	operation, owner, err := coordinator.replay.Bind(binding)
	if err != nil {
		return notApplied(deployment.Name), err
	}
	if owner {
		coordinator.enqueue(ctx, deployment, request.Digest, nil, nil, nil, operation)
		return operation.Wait(ctx)
	}

	outcome, operationErr := operation.Wait(ctx)
	if ctx.Err() != nil {
		return outcome, operationErr
	}

	var retryVersion *uint64
	var resume *Outcome
	switch {
	case outcome.Action == ActionIndeterminate:
		version := outcome.InspectedVersion
		retryVersion = &version
	case outcome.Conclusion == swarm.ConclusionInProgress || observationFailed(outcome, operationErr):
		resume = &outcome
	default:
		return outcome, operationErr
	}

	resumed, resumeOwner, err := coordinator.replay.ReplaceCompleted(binding, operation)
	if err != nil {
		return outcome, err
	}
	if resumeOwner {
		coordinator.enqueue(ctx, deployment, request.Digest, retryVersion, resume, &outcome, resumed)
	}

	return resumed.Wait(ctx)
}

func (coordinator *Coordinator) enqueue(ctx context.Context, deployment policy.Deployment, digest string, retryVersion *uint64, resume *Outcome, previous *Outcome, operation *replay.Operation[Outcome]) {
	queued := &job{
		ctx:          ctx,
		deployment:   deployment,
		digest:       digest,
		retryVersion: retryVersion,
		resume:       resume,
		previous:     previous,
		operation:    operation,
	}

	if err := ctx.Err(); err != nil {
		queued.completeWithoutExecution(err)
		return
	}
	if err := coordinator.ctx.Err(); err != nil {
		queued.completeWithoutExecution(err)
		return
	}

	worker, exists := coordinator.workers[deployment.Service]
	if !exists {
		queued.completeWithoutExecution(ErrInvalidConfiguration)
		return
	}

	if err := worker.enqueue(queued); err != nil {
		queued.completeWithoutExecution(err)
	}
}

func observationFailed(outcome Outcome, err error) bool {
	return err != nil && outcome.Conclusion == "" &&
		(outcome.Action == ActionUpdated || outcome.Action == ActionNoChange)
}

func (queued *job) completeWithoutExecution(err error) {
	outcome := notApplied(queued.deployment.Name)
	if queued.previous != nil {
		outcome = *queued.previous
	}
	_ = queued.operation.Complete(outcome, err)
}

func (worker *serviceWorker) enqueue(queued *job) error {
	worker.mu.Lock()
	defer worker.mu.Unlock()

	if err := queued.ctx.Err(); err != nil {
		return err
	}
	if err := worker.ctx.Err(); err != nil {
		return err
	}
	if worker.stopped {
		return worker.ctx.Err()
	}
	worker.removeCancelledLocked()
	if len(worker.queue) == worker.capacity {
		return ErrQueueFull
	}

	worker.queue = append(worker.queue, queued)
	queued.stopRemoval = context.AfterFunc(queued.ctx, func() {
		worker.remove(queued)
	})
	select {
	case worker.wake <- struct{}{}:
	default:
	}
	return nil
}

func (worker *serviceWorker) removeCancelledLocked() {
	retained := worker.queue[:0]
	for _, queued := range worker.queue {
		if queued.ctx.Err() == nil {
			retained = append(retained, queued)
			continue
		}

		queued.stopRemoval()
		queued.completeWithoutExecution(queued.ctx.Err())
	}
	for index := len(retained); index < len(worker.queue); index++ {
		worker.queue[index] = nil
	}
	worker.queue = retained
}

func (worker *serviceWorker) remove(queued *job) {
	worker.mu.Lock()
	for index, candidate := range worker.queue {
		if candidate != queued {
			continue
		}

		copy(worker.queue[index:], worker.queue[index+1:])
		worker.queue[len(worker.queue)-1] = nil
		worker.queue = worker.queue[:len(worker.queue)-1]
		worker.mu.Unlock()
		queued.completeWithoutExecution(queued.ctx.Err())
		return
	}
	worker.mu.Unlock()
}

func (worker *serviceWorker) next() (*job, bool) {
	for {
		worker.mu.Lock()
		if err := worker.ctx.Err(); err != nil {
			worker.stopped = true
			queued := worker.queue
			worker.queue = nil
			worker.mu.Unlock()
			for _, next := range queued {
				next.stopRemoval()
				next.completeWithoutExecution(err)
			}
			return nil, false
		}
		if len(worker.queue) != 0 {
			next := worker.queue[0]
			worker.queue[0] = nil
			worker.queue = worker.queue[1:]
			worker.mu.Unlock()
			next.stopRemoval()
			return next, true
		}
		worker.mu.Unlock()

		select {
		case <-worker.ctx.Done():
		case <-worker.wake:
		}
	}
}

func (coordinator *Coordinator) runWorker(worker *serviceWorker) {
	for {
		next, ok := worker.next()
		if !ok {
			return
		}

		executionCtx, cancel := context.WithCancel(next.ctx)
		stopRootCancellation := context.AfterFunc(worker.ctx, cancel)
		if worker.ctx.Err() != nil {
			cancel()
		}
		next.ctx = executionCtx
		outcome, err := coordinator.execute(*next)
		stopRootCancellation()
		cancel()
		_ = next.operation.Complete(outcome, err)
	}
}

func (coordinator *Coordinator) execute(next job) (Outcome, error) {
	if err := next.ctx.Err(); err != nil {
		return notApplied(next.deployment.Name), err
	}
	if next.resume != nil {
		return coordinator.observe(next.ctx, *next.resume)
	}

	var applied swarm.Result
	var err error
	if next.retryVersion == nil {
		applied, err = coordinator.updater.Apply(next.ctx, next.deployment.Service, next.deployment.Image, next.digest)
	} else {
		applied, err = coordinator.updater.Retry(next.ctx, next.deployment.Service, next.deployment.Image, next.digest, *next.retryVersion)
	}

	outcome := outcomeFromApply(next.deployment.Name, applied)
	if err != nil {
		if applied.Action != swarm.ActionIndeterminate {
			if outcome.Action == "" {
				outcome.Action = ActionNotApplied
			}
			if errors.Is(err, swarm.ErrConflict) {
				outcome.Conclusion = swarm.ConclusionSuperseded
			}
			return outcome, err
		}

		inspection, inspectErr := coordinator.updater.Inspect(next.ctx, next.deployment.Service, next.deployment.Image, next.digest)
		if inspectErr != nil || !inspection.Desired {
			if inspectErr != nil {
				return outcome, errors.Join(err, inspectErr)
			}

			return outcome, err
		}

		outcome.Action = ActionUpdated
		outcome.ServiceID = inspection.ServiceID
		outcome.Image = inspection.Image
	}

	return coordinator.observe(next.ctx, outcome)
}

func (coordinator *Coordinator) observe(ctx context.Context, outcome Outcome) (Outcome, error) {
	observation, err := coordinator.observer.Observe(ctx, swarm.Target{
		ServiceID:        outcome.ServiceID,
		Image:            outcome.Image,
		RolloutVersion:   outcome.RolloutVersion,
		RolloutStartedAt: outcome.RolloutStartedAt,
	})
	if err != nil {
		return outcome, err
	}
	outcome.Conclusion = observation.Conclusion
	outcome.RolloutVersion = observation.RolloutVersion
	outcome.RolloutStartedAt = observation.RolloutStartedAt
	outcome.Message = observation.Message
	outcome.Tasks = observation.Tasks
	return outcome, nil
}

func outcomeFromApply(deployment string, result swarm.Result) Outcome {
	return Outcome{
		Deployment:       deployment,
		Action:           Action(result.Action),
		ServiceID:        result.ServiceID,
		Image:            result.Image,
		InspectedVersion: result.InspectedVersion,
		Warnings:         append([]string(nil), result.Warnings...),
	}
}

func notApplied(deployment string) Outcome {
	return Outcome{Deployment: deployment, Action: ActionNotApplied}
}
