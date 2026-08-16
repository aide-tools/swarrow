package replay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCacheBind(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 2, func() time.Time { return now })
	binding := testBinding(now, "token-1")

	operation, owner, err := cache.Bind(binding)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if !owner {
		t.Error("Bind() owner = false, want true")
	}
	if operation.Binding() != binding {
		t.Errorf("Binding() = %#v, want %#v", operation.Binding(), binding)
	}
}

func TestCacheReturnsExistingExactOperation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	binding := testBinding(now, "token-1")
	first, _, err := cache.Bind(binding)
	if err != nil {
		t.Fatalf("first Bind() error = %v", err)
	}

	retry, owner, err := cache.Bind(binding)
	if err != nil {
		t.Fatalf("retry Bind() error = %v", err)
	}
	if owner {
		t.Error("retry Bind() owner = true, want false")
	}
	if retry != first {
		t.Error("retry Bind() returned a different operation")
	}
}

func TestCacheRejectsChangedTokenReuse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Binding)
	}{
		{
			name: "deployment",
			mutate: func(binding *Binding) {
				binding.Deployment = "other"
			},
		},
		{
			name: "digest",
			mutate: func(binding *Binding) {
				binding.Digest = "sha256:other"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cache := newTestCache[string](t, 1, func() time.Time { return now })
			binding := testBinding(now, "token-1")
			if _, _, err := cache.Bind(binding); err != nil {
				t.Fatalf("first Bind() error = %v", err)
			}
			test.mutate(&binding)

			operation, owner, err := cache.Bind(binding)
			if !errors.Is(err, ErrReused) {
				t.Fatalf("retry Bind() error = %v, want ErrReused", err)
			}
			if operation != nil || owner {
				t.Errorf("retry Bind() = (%v, %v), want (nil, false)", operation, owner)
			}
		})
	}
}

func TestCacheFailsClosedAtCapacity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	if _, _, err := cache.Bind(testBinding(now, "token-1")); err != nil {
		t.Fatalf("first Bind() error = %v", err)
	}

	operation, owner, err := cache.Bind(testBinding(now, "token-2"))
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("second Bind() error = %v, want ErrCapacity", err)
	}
	if operation != nil || owner {
		t.Errorf("second Bind() = (%v, %v), want (nil, false)", operation, owner)
	}
}

func TestCacheRemovesRecordsAfterAcceptanceWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	if _, _, err := cache.Bind(testBinding(now, "token-1")); err != nil {
		t.Fatalf("first Bind() error = %v", err)
	}

	now = now.Add(time.Minute)
	operation, owner, err := cache.Bind(testBinding(now, "token-2"))
	if err != nil {
		t.Fatalf("second Bind() error = %v", err)
	}
	if operation == nil || !owner {
		t.Errorf("second Bind() = (%v, %v), want operation owner", operation, owner)
	}
}

func TestCacheBindIsAtomic(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	binding := testBinding(now, "token-1")

	const callers = 32
	operations := make(chan *Operation[string], callers)
	owners := make(chan bool, callers)
	errorsChannel := make(chan error, callers)
	var waitGroup sync.WaitGroup
	for range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			operation, owner, err := cache.Bind(binding)
			operations <- operation
			owners <- owner
			errorsChannel <- err
		}()
	}
	waitGroup.Wait()
	close(operations)
	close(owners)
	close(errorsChannel)

	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("Bind() error = %v", err)
		}
	}
	ownerCount := 0
	for owner := range owners {
		if owner {
			ownerCount++
		}
	}
	if ownerCount != 1 {
		t.Errorf("owner count = %d, want 1", ownerCount)
	}
	var first *Operation[string]
	for operation := range operations {
		if first == nil {
			first = operation
		}
		if operation != first {
			t.Error("Bind() returned different operations")
		}
	}
}

func TestOperationRetainsOutcome(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	operation, _, err := cache.Bind(testBinding(now, "token-1"))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	wantError := errors.New("deployment failed")
	if err := operation.Complete("failed", wantError); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	result, err := operation.Wait(context.Background())
	if result != "failed" || !errors.Is(err, wantError) {
		t.Errorf("Wait() = (%q, %v), want (%q, %v)", result, err, "failed", wantError)
	}
	if err := operation.Complete("completed", nil); !errors.Is(err, ErrCompleted) {
		t.Errorf("second Complete() error = %v, want ErrCompleted", err)
	}
}

func TestCacheReplacesACompletedExactOperationOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	binding := testBinding(now, "token-1")
	previous, _, err := cache.Bind(binding)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := previous.Complete("indeterminate", nil); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	replacement, owner, err := cache.ReplaceCompleted(binding, previous)
	if err != nil {
		t.Fatalf("ReplaceCompleted() error = %v", err)
	}
	if !owner || replacement == previous {
		t.Errorf("ReplaceCompleted() = (%v, %v), want a replacement owner", replacement, owner)
	}

	retry, owner, err := cache.ReplaceCompleted(binding, previous)
	if err != nil {
		t.Fatalf("second ReplaceCompleted() error = %v", err)
	}
	if owner || retry != replacement {
		t.Errorf("second ReplaceCompleted() = (%v, %v), want current operation without ownership", retry, owner)
	}
}

func TestCacheDoesNotReplaceAnIncompleteOperation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	binding := testBinding(now, "token-1")
	operation, _, err := cache.Bind(binding)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	replacement, owner, err := cache.ReplaceCompleted(binding, operation)
	if err != nil {
		t.Fatalf("ReplaceCompleted() error = %v", err)
	}
	if owner || replacement != operation {
		t.Errorf("ReplaceCompleted() = (%v, %v), want existing operation without ownership", replacement, owner)
	}
}

func TestCacheRejectsInvalidReplacementAttempts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	binding := testBinding(now, "token-1")
	operation, _, err := cache.Bind(binding)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := operation.Complete("indeterminate", nil); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	changed := binding
	changed.Digest = "sha256:changed"
	if _, _, err := cache.ReplaceCompleted(changed, operation); !errors.Is(err, ErrReused) {
		t.Errorf("changed ReplaceCompleted() error = %v, want ErrReused", err)
	}
	if _, _, err := cache.ReplaceCompleted(binding, nil); !errors.Is(err, ErrInvalidBinding) {
		t.Errorf("nil ReplaceCompleted() error = %v, want ErrInvalidBinding", err)
	}

	unknown := testBinding(now, "token-2")
	if _, _, err := cache.ReplaceCompleted(unknown, operation); !errors.Is(err, ErrUnknownOperation) {
		t.Errorf("unknown ReplaceCompleted() error = %v, want ErrUnknownOperation", err)
	}
}

func TestOperationWaitDoesNotCancelSharedOutcome(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	cache := newTestCache[string](t, 1, func() time.Time { return now })
	operation, _, err := cache.Bind(testBinding(now, "token-1"))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := operation.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if err := operation.Complete("completed", nil); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	result, err := operation.Wait(context.Background())
	if err != nil || result != "completed" {
		t.Errorf("second Wait() = (%q, %v), want (%q, nil)", result, err, "completed")
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		now      func() time.Time
	}{
		{name: "zero capacity", capacity: 0, now: time.Now},
		{name: "negative capacity", capacity: -1, now: time.Now},
		{name: "missing clock", capacity: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cache, err := New[string](test.capacity, test.now)
			if !errors.Is(err, ErrInvalidConfiguration) || cache != nil {
				t.Fatalf("New() = (%v, %v), want (nil, ErrInvalidConfiguration)", cache, err)
			}
		})
	}
}

func TestCacheRejectsInvalidBinding(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Binding)
	}{
		{name: "missing token ID", mutate: func(binding *Binding) { binding.TokenID = "" }},
		{name: "missing deployment", mutate: func(binding *Binding) { binding.Deployment = "" }},
		{name: "missing digest", mutate: func(binding *Binding) { binding.Digest = "" }},
		{name: "expired acceptance window", mutate: func(binding *Binding) { binding.ValidUntil = now }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cache := newTestCache[string](t, 1, func() time.Time { return now })
			binding := testBinding(now, "token-1")
			test.mutate(&binding)
			operation, owner, err := cache.Bind(binding)
			if !errors.Is(err, ErrInvalidBinding) || operation != nil || owner {
				t.Fatalf("Bind() = (%v, %v, %v), want (nil, false, ErrInvalidBinding)", operation, owner, err)
			}
		})
	}
}

func newTestCache[Outcome any](t *testing.T, capacity int, now func() time.Time) *Cache[Outcome] {
	t.Helper()

	cache, err := New[Outcome](capacity, now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return cache
}

func testBinding(now time.Time, tokenID string) Binding {
	return Binding{
		TokenID:    tokenID,
		Deployment: "example-web",
		Digest:     "sha256:0123456789abcdef",
		ValidUntil: now.Add(time.Minute),
	}
}
