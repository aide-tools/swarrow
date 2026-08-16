// Package replay binds token identifiers to deployment requests and retains their outcomes.
package replay

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrCapacity is returned when accepting another token would require evicting
	// a record that must still be retained.
	ErrCapacity = errors.New("replay cache capacity exhausted")
	// ErrReused is returned when a token identifier is reused for another request.
	ErrReused = errors.New("token identifier already bound to another request")
	// ErrInvalidConfiguration is returned when a cache cannot fail closed.
	ErrInvalidConfiguration = errors.New("invalid replay cache configuration")
	// ErrInvalidBinding is returned when a binding cannot be retained safely.
	ErrInvalidBinding = errors.New("invalid replay binding")
	// ErrCompleted is returned when an operation outcome is completed twice.
	ErrCompleted = errors.New("replay operation already completed")
	// ErrUnknownOperation is returned when an operation cannot be replaced.
	ErrUnknownOperation = errors.New("replay operation not found")
)

// Binding identifies the only deployment request permitted to use a token.
type Binding struct {
	TokenID    string
	Deployment string
	Digest     string
	ValidUntil time.Time
}

// Cache atomically binds token identifiers and retains typed operation outcomes.
type Cache[Outcome any] struct {
	mu       sync.Mutex
	capacity int
	now      func() time.Time
	entries  map[string]entry[Outcome]
}

type entry[Outcome any] struct {
	binding   Binding
	operation *Operation[Outcome]
}

// New creates a fixed-capacity replay cache using now for expiry decisions.
func New[Outcome any](capacity int, now func() time.Time) (*Cache[Outcome], error) {
	if capacity <= 0 || now == nil {
		return nil, ErrInvalidConfiguration
	}

	return &Cache[Outcome]{
		capacity: capacity,
		now:      now,
		entries:  make(map[string]entry[Outcome], capacity),
	}, nil
}

// Bind atomically claims a token identifier or returns the shared operation
// for an exact retry. The boolean result is true only for the caller that owns
// a new operation.
func (cache *Cache[Outcome]) Bind(binding Binding) (*Operation[Outcome], bool, error) {
	now := cache.now()
	if !validBinding(binding, now) {
		return nil, false, ErrInvalidBinding
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	cache.removeExpired(now)
	if existing, found := cache.entries[binding.TokenID]; found {
		if existing.binding.Deployment != binding.Deployment || existing.binding.Digest != binding.Digest {
			return nil, false, ErrReused
		}

		return existing.operation, false, nil
	}

	if len(cache.entries) >= cache.capacity {
		return nil, false, ErrCapacity
	}

	operation := newOperation[Outcome](binding)
	cache.entries[binding.TokenID] = entry[Outcome]{
		binding:   binding,
		operation: operation,
	}

	return operation, true, nil
}

// ReplaceCompleted atomically replaces previous when it is still the current
// completed operation for the exact binding. The boolean result is true only
// for the caller that owns the replacement.
func (cache *Cache[Outcome]) ReplaceCompleted(binding Binding, previous *Operation[Outcome]) (*Operation[Outcome], bool, error) {
	now := cache.now()
	if previous == nil || !validBinding(binding, now) {
		return nil, false, ErrInvalidBinding
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	cache.removeExpired(now)
	existing, found := cache.entries[binding.TokenID]
	if !found {
		return nil, false, ErrUnknownOperation
	}
	if existing.binding.Deployment != binding.Deployment || existing.binding.Digest != binding.Digest {
		return nil, false, ErrReused
	}
	if existing.operation != previous || !previous.isCompleted() {
		return existing.operation, false, nil
	}

	operation := newOperation[Outcome](binding)
	cache.entries[binding.TokenID] = entry[Outcome]{
		binding:   binding,
		operation: operation,
	}

	return operation, true, nil
}

func validBinding(binding Binding, now time.Time) bool {
	return binding.TokenID != "" &&
		binding.Deployment != "" &&
		binding.Digest != "" &&
		now.Before(binding.ValidUntil)
}

func (cache *Cache[Outcome]) removeExpired(now time.Time) {
	for tokenID, record := range cache.entries {
		if !now.Before(record.binding.ValidUntil) {
			delete(cache.entries, tokenID)
		}
	}
}

// Operation is the shared outcome of one bound deployment request.
type Operation[Outcome any] struct {
	binding Binding
	done    chan struct{}

	mu        sync.Mutex
	completed bool
	result    Outcome
	err       error
}

func newOperation[Outcome any](binding Binding) *Operation[Outcome] {
	return &Operation[Outcome]{
		binding: binding,
		done:    make(chan struct{}),
	}
}

// Binding returns a copy of the request binding associated with this operation.
func (operation *Operation[Outcome]) Binding() Binding {
	return operation.binding
}

// Complete records and publishes this operation's outcome exactly once.
func (operation *Operation[Outcome]) Complete(result Outcome, err error) error {
	operation.mu.Lock()
	defer operation.mu.Unlock()

	if operation.completed {
		return ErrCompleted
	}

	operation.result = result
	operation.err = err
	operation.completed = true
	close(operation.done)
	return nil
}

// Wait blocks until the operation completes or ctx is cancelled. Cancelling
// ctx stops only this caller's wait.
func (operation *Operation[Outcome]) Wait(ctx context.Context) (Outcome, error) {
	select {
	case <-operation.done:
		return operation.outcome()
	default:
	}

	select {
	case <-operation.done:
		return operation.outcome()
	case <-ctx.Done():
		select {
		case <-operation.done:
			return operation.outcome()
		default:
			var zero Outcome
			return zero, ctx.Err()
		}
	}
}

func (operation *Operation[Outcome]) outcome() (Outcome, error) {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	return operation.result, operation.err
}

func (operation *Operation[Outcome]) isCompleted() bool {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	return operation.completed
}
