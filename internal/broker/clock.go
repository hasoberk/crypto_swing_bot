package broker

import "time"

// SystemClock is the wall-clock Clock used by paper and live trading.
type SystemClock struct{}

func (SystemClock) Now() time.Time        { return time.Now().UTC() }
func (SystemClock) Sleep(d time.Duration) { time.Sleep(d) }

// BacktestClock is a virtual Clock the backtest engine advances one bar at
// a time via Advance. Sleep is a no-op: a backtest must replay years of
// history in seconds, not real time.
type BacktestClock struct {
	now time.Time
}

// NewBacktestClock returns a BacktestClock initialized to start.
func NewBacktestClock(start time.Time) *BacktestClock {
	return &BacktestClock{now: start}
}

func (c *BacktestClock) Now() time.Time      { return c.now }
func (c *BacktestClock) Sleep(time.Duration) {}

// Advance moves the clock forward to t. The backtest engine calls this
// once per simulated day, immediately before asking PaperBroker to settle
// that day's fills (see PaperBroker.Advance).
func (c *BacktestClock) Advance(t time.Time) { c.now = t }
