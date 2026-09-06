package engine

import (
	"context"
	"testing"
	"time"

	"swingbot/internal/broker"
	"swingbot/internal/datafeed"
	"swingbot/internal/domain"
	"swingbot/internal/strategy"
)

func TestNextRunAt(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "before today's run time -> today",
			now:  time.Date(2026, 8, 14, 0, 1, 0, 0, time.UTC),
			want: time.Date(2026, 8, 14, 0, 5, 0, 0, time.UTC),
		},
		{
			name: "exactly at run time -> tomorrow (must be strictly in the future)",
			now:  time.Date(2026, 8, 14, 0, 5, 0, 0, time.UTC),
			want: time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC),
		},
		{
			name: "after today's run time -> tomorrow",
			now:  time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC),
		},
		{
			name: "non-UTC input is normalized",
			now:  time.Date(2026, 8, 14, 3, 0, 0, 0, time.FixedZone("X", 3*3600)), // 00:00 UTC
			want: time.Date(2026, 8, 14, 0, 5, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NextRunAt(c.now, "00:05", time.UTC)
			if err != nil {
				t.Fatalf("NextRunAt: %v", err)
			}
			if !got.Equal(c.want) {
				t.Errorf("NextRunAt(%s) = %s, want %s", c.now, got, c.want)
			}
		})
	}
}

func TestNextRunAtRejectsMalformedRunAtUTC(t *testing.T) {
	for _, bad := range []string{"", "25:00", "12:60", "not-a-time", "12"} {
		if _, err := NextRunAt(time.Now(), bad, time.UTC); err == nil {
			t.Errorf("NextRunAt with run_at_utc=%q: expected an error", bad)
		}
	}
}

// TestNextRunAt_TracksLocationDST proves NextRunAt("00:00", loc) really
// means "local midnight in loc", auto-adjusting across a DST transition —
// not a fixed UTC offset baked in once, which would drift an hour when
// Europe/Rome switches between CEST (summer, UTC+2) and CET (winter,
// UTC+1).
func TestNextRunAt_TracksLocationDST(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skipf("Europe/Rome tzdata not available: %v", err)
	}

	// Summer (CEST, UTC+2): local midnight is 22:00 UTC the day before.
	summerNow := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got, err := NextRunAt(summerNow, "00:00", rome)
	if err != nil {
		t.Fatalf("NextRunAt: %v", err)
	}
	wantSummer := time.Date(2026, 7, 10, 22, 0, 0, 0, time.UTC) // 2026-07-11 00:00 CEST
	if !got.Equal(wantSummer) {
		t.Errorf("summer: NextRunAt = %s (UTC), want %s (UTC)", got.UTC(), wantSummer)
	}

	// Winter (CET, UTC+1): local midnight is 23:00 UTC the day before —
	// one hour later than summer's, purely because of the DST switch, with
	// no config change in between.
	winterNow := time.Date(2026, 12, 10, 12, 0, 0, 0, time.UTC)
	got, err = NextRunAt(winterNow, "00:00", rome)
	if err != nil {
		t.Fatalf("NextRunAt: %v", err)
	}
	wantWinter := time.Date(2026, 12, 10, 23, 0, 0, 0, time.UTC) // 2026-12-11 00:00 CET
	if !got.Equal(wantWinter) {
		t.Errorf("winter: NextRunAt = %s (UTC), want %s (UTC)", got.UTC(), wantWinter)
	}
}

// TestRun_StopsOnContextCancel exercises the scheduling loop end to end
// with a fakeClock (Sleep advances time instantly) so this test itself
// never really sleeps: Run must still respect ctx cancellation instead of
// looping forever.
func TestRun_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	st := newTestStore(t)
	ex := &fakeExchange{}
	feed := datafeed.NewFeed(ex, st, "1d")
	notifier := newFakeNotifier()
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	eng, err := New(Config{
		Store: st, Feed: feed, Notifier: notifier, RiskGate: defaultRiskGate(),
		Strategy: fakeStrategy{StratName: "test", EvalFunc: func(in strategy.Input) ([]domain.Signal, error) {
			return nil, nil
		}},
		Costs: broker.Costs{FeeRate: 0.001, SlippageBps: 15}, InitialCash: 1000,
		RunAt: "00:05", Clock: clock,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// RunOnce will fail immediately (no candle data at all) — that is
	// fine, Run must keep looping past a failed day rather than dying, so
	// cancel a couple of iterations in in a background goroutine.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err = eng.Run(ctx)
	if err == nil {
		t.Fatal("Run: expected ctx.Err() once canceled, got nil")
	}
}

func TestConfigValidateRejectsMissingDependencies(t *testing.T) {
	base := Config{
		Store: newTestStore(t), Feed: datafeed.NewFeed(&fakeExchange{}, newTestStore(t), "1d"),
		Strategy: fakeStrategy{StratName: "test"}, Notifier: newFakeNotifier(),
		RiskGate: defaultRiskGate(), Costs: broker.Costs{FeeRate: 0.001, SlippageBps: 15}, InitialCash: 1000,
	}
	if _, err := New(base); err != nil {
		t.Fatalf("New(valid base config): %v", err)
	}

	missingStore := base
	missingStore.Store = nil
	if _, err := New(missingStore); err == nil {
		t.Error("expected an error for a nil Store")
	}

	missingCash := base
	missingCash.InitialCash = 0
	if _, err := New(missingCash); err == nil {
		t.Error("expected an error for InitialCash<=0")
	}

	zeroCosts := base
	zeroCosts.Costs = broker.Costs{}
	if _, err := New(zeroCosts); err == nil {
		t.Error("expected İ4's ErrCostsNotConfigured for zero costs")
	}
}
