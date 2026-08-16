package exchange

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock used to make RateLimiter tests
// deterministic and instant (no real sleeping).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRateLimiter_AllowsBurstUpToCapacityThenBlocks(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	rl := NewRateLimiter(60, withRateLimiterClock(clock.Now)) // 60/min == 1/sec, capacity 60

	for i := 0; i < 60; i++ {
		if !rl.Allow() {
			t.Fatalf("expected token %d/60 to be available immediately (burst up to capacity)", i+1)
		}
	}
	if rl.Allow() {
		t.Fatal("expected bucket to be empty after spending the full per-minute capacity")
	}

	// Advance the clock by exactly one second: refillPerSec = 60/60 = 1,
	// so exactly one more token should be available.
	clock.Advance(time.Second)
	if !rl.Allow() {
		t.Fatal("expected exactly one token to have refilled after 1s at 60/min")
	}
	if rl.Allow() {
		t.Fatal("expected no second token to be available after only 1s")
	}
}

func TestRateLimiter_WaitBlocksUntilTokenAvailable(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	rl := NewRateLimiter(600, withRateLimiterClock(clock.Now)) // 10/sec

	// Drain the bucket.
	for rl.Allow() {
	}

	done := make(chan error, 1)
	go func() {
		done <- rl.Wait(context.Background())
	}()

	select {
	case <-done:
		t.Fatal("Wait returned before any token could have refilled")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked, since the fake clock hasn't advanced.
	}

	clock.Advance(200 * time.Millisecond) // at 10/sec, 200ms => 2 tokens

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock after the clock advanced past the refill point")
	}
}

func TestRateLimiter_WaitRespectsContextCancellation(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	rl := NewRateLimiter(1, withRateLimiterClock(clock.Now)) // very slow refill
	for rl.Allow() {
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := rl.Wait(ctx)
	if err == nil {
		t.Fatal("expected Wait to return an error when its context is canceled while waiting")
	}
}

func TestRateLimiter_ZeroOrNegativeRateFallsBackToConservativeDefault(t *testing.T) {
	rl := NewRateLimiter(0)
	if rl.capacity <= 0 {
		t.Fatalf("expected a positive fallback capacity, got %v", rl.capacity)
	}
	rl2 := NewRateLimiter(-5)
	if rl2.capacity <= 0 {
		t.Fatalf("expected a positive fallback capacity, got %v", rl2.capacity)
	}
}

func TestRateLimiter_WithBurstCapsCapacity(t *testing.T) {
	rl := NewRateLimiter(1000, WithBurst(5))
	count := 0
	for rl.Allow() {
		count++
		if count > 5 {
			t.Fatalf("expected burst to be capped at 5, got at least %d", count)
		}
	}
	if count != 5 {
		t.Fatalf("expected exactly 5 tokens available with WithBurst(5), got %d", count)
	}
}

// --- backoff ---

func TestBackoffPolicy_DelayGrowsAndRespectsMax(t *testing.T) {
	p := BackoffPolicy{Base: 100 * time.Millisecond, Max: 800 * time.Millisecond, MaxRetries: 10}
	rnd := rand.New(rand.NewSource(1))

	// With full jitter, delay(attempt) in [0, cap(attempt)]. cap should
	// double each attempt (100,200,400,800,800,...) capped at Max.
	wantCaps := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond, // capped
	}
	for i, wantCap := range wantCaps {
		attempt := i + 1
		for trial := 0; trial < 50; trial++ {
			d := p.delay(attempt, rnd)
			if d < 0 || d > wantCap {
				t.Fatalf("attempt %d: delay %v out of expected range [0, %v]", attempt, d, wantCap)
			}
		}
	}
}

func TestBackoffPolicy_DelayIsJittered(t *testing.T) {
	p := BackoffPolicy{Base: 100 * time.Millisecond, Max: 10 * time.Second, MaxRetries: 5}
	rnd := rand.New(rand.NewSource(42))

	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[p.delay(3, rnd)] = true
	}
	if len(seen) < 2 {
		t.Fatal("expected jittered delays to vary across calls, got the same value repeatedly")
	}
}

func TestBackoffPolicy_SleepRespectsContextCancellation(t *testing.T) {
	p := BackoffPolicy{Base: 10 * time.Second, Max: 10 * time.Second, MaxRetries: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := p.sleep(ctx, 1, rand.New(rand.NewSource(1)))
	if err == nil {
		t.Fatal("expected sleep to return an error when context is canceled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("sleep did not return promptly on cancellation, took %v", elapsed)
	}
}
