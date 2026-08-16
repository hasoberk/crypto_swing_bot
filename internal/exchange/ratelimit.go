package exchange

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// RateLimiter is a token-bucket limiter sized from
// config.exchange.rate_limit_per_min (SPEC.md Bölüm 8, Bölüm 5.2 "Rate
// limit'e saygılı olmalı (token bucket)").
//
// The bucket's capacity equals the configured per-minute budget and refills
// continuously at capacity/60 tokens per second. That is the literal
// reading of "rate_limit_per_min": a caller may burst up to the full
// per-minute allotment and must then wait for it to trickle back in. If a
// smoother, tighter cadence is wanted, construct with WithBurst to cap the
// bucket below the full per-minute size.
type RateLimiter struct {
	mu           sync.Mutex
	tokens       float64
	capacity     float64
	refillPerSec float64
	lastRefill   time.Time

	now func() time.Time // overridable for tests
}

// RateLimiterOption configures a RateLimiter constructed by NewRateLimiter.
type RateLimiterOption func(*RateLimiter)

// WithBurst caps the bucket's capacity (and therefore the largest burst a
// caller can spend before waiting) below the full per-minute allotment.
func WithBurst(burst int) RateLimiterOption {
	return func(rl *RateLimiter) {
		if burst > 0 {
			rl.capacity = float64(burst)
			if rl.tokens > rl.capacity {
				rl.tokens = rl.capacity
			}
		}
	}
}

// withRateLimiterClock overrides the RateLimiter's clock; used by tests.
func withRateLimiterClock(now func() time.Time) RateLimiterOption {
	return func(rl *RateLimiter) { rl.now = now }
}

// NewRateLimiter builds a token bucket allowing ratePerMinute requests per
// minute. ratePerMinute <= 0 falls back to a conservative default (60/min,
// i.e. one request per second) rather than allowing an unbounded rate —
// a misconfigured 0 must never mean "no limit" against a real exchange.
func NewRateLimiter(ratePerMinute int, opts ...RateLimiterOption) *RateLimiter {
	if ratePerMinute <= 0 {
		ratePerMinute = 60
	}
	capacity := float64(ratePerMinute)
	rl := &RateLimiter{
		tokens:       capacity,
		capacity:     capacity,
		refillPerSec: capacity / 60.0,
		lastRefill:   time.Now(),
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(rl)
	}
	rl.lastRefill = rl.now()
	return rl
}

// refill adds tokens accrued since lastRefill. Caller must hold rl.mu.
func (rl *RateLimiter) refill() {
	now := rl.now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	rl.tokens += elapsed * rl.refillPerSec
	if rl.tokens > rl.capacity {
		rl.tokens = rl.capacity
	}
	rl.lastRefill = now
}

// Allow reports whether a token is available right now and, if so, spends
// it. It never blocks.
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.refill()
	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	return false
}

// Wait blocks until a token is available (spending it before returning) or
// ctx is done, whichever comes first.
func (rl *RateLimiter) Wait(ctx context.Context) error {
	for {
		rl.mu.Lock()
		rl.refill()
		if rl.tokens >= 1 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}
		missing := 1 - rl.tokens
		wait := time.Duration(missing / rl.refillPerSec * float64(time.Second))
		if wait <= 0 {
			wait = time.Millisecond
		}
		rl.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// --- exponential backoff + jitter (SPEC.md Bölüm 5.2: "429/418 yanıtlarında
// üstel geri çekilme (exponential backoff) + jitter uygula") ---

// BackoffPolicy controls how long CreateOrder/doRequestWithRetry wait
// between retries after a 429/418/5xx/transient network error.
type BackoffPolicy struct {
	Base       time.Duration // delay before the first retry (attempt=1)
	Max        time.Duration // upper bound on any single delay
	MaxRetries int           // number of retries after the initial attempt
}

// DefaultBackoffPolicy is a reasonable default: up to 5 retries, starting
// at 500ms and capping at 30s, doubling each attempt with full jitter.
var DefaultBackoffPolicy = BackoffPolicy{
	Base:       500 * time.Millisecond,
	Max:        30 * time.Second,
	MaxRetries: 5,
}

// delay returns the backoff duration for the given attempt (1-based: the
// delay to wait before making attempt N+1 after attempt N failed), using
// "full jitter": a uniformly random duration in [0, cap], where cap doubles
// with each attempt up to Max. rnd may be nil, in which case
// rand.Float64 (global) is used; tests inject a seeded *rand.Rand for
// determinism.
func (p BackoffPolicy) delay(attempt int, rnd *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := p.Base
	if base <= 0 {
		base = DefaultBackoffPolicy.Base
	}
	max := p.Max
	if max <= 0 {
		max = DefaultBackoffPolicy.Max
	}

	cap := base
	for i := 1; i < attempt; i++ {
		cap *= 2
		if cap >= max {
			cap = max
			break
		}
	}
	if cap > max {
		cap = max
	}

	var f float64
	if rnd != nil {
		f = rnd.Float64()
	} else {
		f = rand.Float64()
	}
	return time.Duration(f * float64(cap))
}

// sleep waits for the backoff delay of attempt, honoring ctx cancellation.
func (p BackoffPolicy) sleep(ctx context.Context, attempt int, rnd *rand.Rand) error {
	d := p.delay(attempt, rnd)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
