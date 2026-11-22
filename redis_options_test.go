package timewheel

import (
	"context"
	"testing"
	"time"
)

func TestOptionsDefaults(t *testing.T) {
	opts, err := newRTimeWheelOptions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.tickInterval != time.Second {
		t.Errorf("tickInterval: got %v, want 1s", opts.tickInterval)
	}
	if opts.claimBatch != 2000 {
		t.Errorf("claimBatch: got %d, want 2000", opts.claimBatch)
	}
	if opts.maxSlicesPerTick != 5 {
		t.Errorf("maxSlicesPerTick: got %d, want 5", opts.maxSlicesPerTick)
	}
	if opts.maxConcurrency != 500 {
		t.Errorf("maxConcurrency: got %d, want 500", opts.maxConcurrency)
	}
	if opts.handlerTimeout != 30*time.Second {
		t.Errorf("handlerTimeout: got %v, want 30s", opts.handlerTimeout)
	}
	if opts.lease != 60*time.Second {
		t.Errorf("lease: got %v, want 60s", opts.lease)
	}
	if opts.maxAttempts != 10 {
		t.Errorf("maxAttempts: got %d, want 10", opts.maxAttempts)
	}
	if opts.backoffBase != time.Second {
		t.Errorf("backoffBase: got %v, want 1s", opts.backoffBase)
	}
	if opts.backoffMax != time.Minute {
		t.Errorf("backoffMax: got %v, want 1m", opts.backoffMax)
	}
	if opts.deadRetention != 7*24*time.Hour {
		t.Errorf("deadRetention: got %v, want 7d", opts.deadRetention)
	}
	if opts.statsInterval != 10*time.Second {
		t.Errorf("statsInterval: got %v, want 10s", opts.statsInterval)
	}
}

func TestOptionsValidation(t *testing.T) {
	tests := []struct {
		name string
		opts []RTimeWheelOption
	}{
		{"lease not greater than handler timeout", []RTimeWheelOption{WithLease(10 * time.Second)}},
		{"lease equal to handler timeout", []RTimeWheelOption{WithLease(30 * time.Second)}},
		{"zero max attempts", []RTimeWheelOption{WithMaxAttempts(0)}},
		{"negative claim batch", []RTimeWheelOption{WithClaimBatch(-1)}},
		{"zero max concurrency", []RTimeWheelOption{WithMaxConcurrency(0)}},
		{"zero tick interval", []RTimeWheelOption{WithTickInterval(0)}},
		{"zero handler timeout", []RTimeWheelOption{WithHandlerTimeout(0)}},
		{"backoff base greater than max", []RTimeWheelOption{WithBackoff(2*time.Minute, time.Minute)}},
		{"zero max slices per tick", []RTimeWheelOption{WithMaxSlicesPerTick(0)}},
		{"zero dead retention", []RTimeWheelOption{WithDeadRetention(0)}},
		{"zero stats interval", []RTimeWheelOption{WithStatsInterval(0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newRTimeWheelOptions(tt.opts...); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestOptionsValidCombination(t *testing.T) {
	hook := func(context.Context, TaskResult) {}
	stats := func(context.Context, Stats) {}
	opts, err := newRTimeWheelOptions(
		WithTickInterval(50*time.Millisecond),
		WithClaimBatch(100),
		WithMaxSlicesPerTick(2),
		WithMaxConcurrency(4),
		WithHandlerTimeout(2*time.Second),
		WithLease(3*time.Second),
		WithMaxAttempts(3),
		WithBackoff(50*time.Millisecond, 200*time.Millisecond),
		WithDeadRetention(time.Hour),
		WithStatsInterval(100*time.Millisecond),
		WithResultHook(hook),
		WithStatsHook(stats),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.resultHook == nil || opts.statsHook == nil {
		t.Errorf("hooks should be set")
	}
	if opts.backoffBase != 50*time.Millisecond || opts.backoffMax != 200*time.Millisecond {
		t.Errorf("backoff: got %v/%v, want 50ms/200ms", opts.backoffBase, opts.backoffMax)
	}
}
