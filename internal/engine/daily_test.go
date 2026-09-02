package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"swingbot/internal/broker"
	"swingbot/internal/config"
	"swingbot/internal/datafeed"
	"swingbot/internal/domain"
	"swingbot/internal/notify"
	"swingbot/internal/risk"
	"swingbot/internal/store"
	"swingbot/internal/strategy"
	"swingbot/internal/universe"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func defaultRiskGate() *risk.Gate {
	cfg := config.RiskConfig{RiskPerTrade: 0.01, MaxPositions: 5, MaxExposure: 0.8, MaxPositionPct: 0.5, CooldownHours: 24}
	return risk.NewGate(cfg, risk.NewSizer(cfg))
}

// --- happy path: proposal approved end to end ------------------------------

func TestRunOnce_ApprovedEntryIsSubmittedAndSummaryNotified(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	candles := map[string][]domain.Candle{"AAA/USDT": flatCandles(0, 1, 100)}
	seedMarketsAndCandles(t, ctx, st, candles, nil, "USDT")

	ex := &fakeExchange{markets: []domain.Market{mustMarket("AAA/USDT", "AAA", "USDT")}}
	feed := datafeed.NewFeed(ex, st, "1d", datafeed.WithQuoteFilter("USDT"))

	notifier := newFakeNotifier()
	notifier.autoApprove = func(p notify.Proposal) (bool, bool) { return true, true }

	strat := fakeStrategy{StratName: "test", EvalFunc: func(in strategy.Input) ([]domain.Signal, error) {
		if !in.AsOf.Equal(day(0)) {
			return nil, nil
		}
		if _, open := in.Portfolio.Positions["AAA/USDT"]; open {
			return nil, nil
		}
		return []domain.Signal{{
			AsOf: in.AsOf, Symbol: "AAA/USDT", Kind: domain.SignalEnter,
			RefPrice: 100, StopPrice: 90, Reason: "test entry",
		}}, nil
	}}

	clock := newFakeClock(day(1).Add(5 * time.Minute)) // -> candleDay = day(0)

	eng, err := New(Config{
		Store: st, Feed: feed, Strategy: strat, Notifier: notifier,
		RiskGate: defaultRiskGate(), BreakerCfg: config.BreakerConfig{},
		Costs: broker.Costs{FeeRate: 0.001, SlippageBps: 15}, InitialCash: 100000,
		UniverseParams: universe.FilterParams{Quote: "USDT"},
		Timeframe:      "1d", Quote: "USDT",
		ApprovalTTL: time.Hour, RunAtUTC: "00:05", PollInterval: time.Millisecond, Clock: clock,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(notifier.proposals) != 1 {
		t.Fatalf("ProposeTrade calls = %d, want 1", len(notifier.proposals))
	}
	propID := notifier.proposals[0].ID

	p, ok, err := st.GetProposal(ctx, propID)
	if err != nil || !ok {
		t.Fatalf("GetProposal: ok=%v err=%v", ok, err)
	}
	if p.Status != store.ProposalSubmitted {
		t.Errorf("proposal status = %s, want SUBMITTED", p.Status)
	}
	if p.OrderID == "" {
		t.Error("proposal order_id is empty, want a broker order id")
	}

	snaps, err := st.ListEquitySnapshots(ctx, "paper")
	if err != nil {
		t.Fatalf("ListEquitySnapshots: %v", err)
	}
	if len(snaps) != 1 || !snaps[0].TS.Equal(day(0)) {
		t.Fatalf("equity snapshots = %+v, want exactly one row at %s", snaps, day(0))
	}

	if !notifier.hasNotificationContaining(notify.LevelInfo, "Günlük özet") {
		t.Error("expected a daily-summary notification (SPEC.md Bölüm 6.7 adım 15)")
	}

	// Idempotency: calling RunOnce again for the SAME day must be a no-op
	// (no second proposal, no second snapshot) — SPEC.md Bölüm 6.7's daily
	// cadence must never double-process a calendar day.
	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(notifier.proposals) != 1 {
		t.Errorf("ProposeTrade calls after a second RunOnce = %d, want still 1 (idempotent)", len(notifier.proposals))
	}
}

// --- restart resilience: PENDING proposals -----------------------------

func TestResumePending_ExpiresPastDeadline(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	ex := &fakeExchange{}
	feed := datafeed.NewFeed(ex, st, "1d")
	notifier := newFakeNotifier() // never sends a decision

	eng, err := New(Config{
		Store: st, Feed: feed, Strategy: fakeStrategy{StratName: "test"}, Notifier: notifier,
		RiskGate: defaultRiskGate(), Costs: broker.Costs{FeeRate: 0.001, SlippageBps: 15}, InitialCash: 1000,
		Clock: newFakeClock(day(0).Add(2 * time.Hour)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stop := 90.0
	if err := st.InsertProposal(ctx, store.Proposal{
		ID: "p-expired", CreatedAt: day(0), AsOf: day(0), Symbol: "AAA/USDT", Side: "long", Strategy: "test",
		RefPrice: 100, StopPrice: &stop, Qty: "1", RiskAmount: 10, Reason: "x", MetricsJSON: "{}",
		Status: store.ProposalPending, ExpiresAt: day(0).Add(time.Hour), // already in the past relative to clock
	}); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}

	if err := eng.ResumePending(ctx); err != nil {
		t.Fatalf("ResumePending: %v", err)
	}

	p, ok, err := st.GetProposal(ctx, "p-expired")
	if err != nil || !ok {
		t.Fatalf("GetProposal: ok=%v err=%v", ok, err)
	}
	if p.Status != store.ProposalExpired {
		t.Errorf("status = %s, want EXPIRED", p.Status)
	}
}

func TestResumePending_ApprovesAndSubmitsViaReplay(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	candles := map[string][]domain.Candle{"AAA/USDT": flatCandles(0, 5, 100)}
	seedMarketsAndCandles(t, ctx, st, candles, nil, "USDT")
	ex := &fakeExchange{markets: []domain.Market{mustMarket("AAA/USDT", "AAA", "USDT")}}
	feed := datafeed.NewFeed(ex, st, "1d", datafeed.WithQuoteFilter("USDT"))

	notifier := newFakeNotifier()

	clock := newFakeClock(day(0).Add(time.Hour)) // well before the proposal's deadline

	eng, err := New(Config{
		Store: st, Feed: feed, Strategy: fakeStrategy{StratName: "test"}, Notifier: notifier,
		RiskGate: defaultRiskGate(), Costs: broker.Costs{FeeRate: 0.001, SlippageBps: 15}, InitialCash: 100000,
		Timeframe: "1d", Quote: "USDT", Clock: clock,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stop := 90.0
	if err := st.InsertProposal(ctx, store.Proposal{
		ID: "p-resume", CreatedAt: day(0), AsOf: day(0), Symbol: "AAA/USDT", Side: "long", Strategy: "test",
		RefPrice: 100, StopPrice: &stop, Qty: "1", RiskAmount: 10, Reason: "resume test", MetricsJSON: "{}",
		Status: store.ProposalPending, ExpiresAt: day(0).Add(4 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}

	// A decision is already sitting on the channel, as if it arrived just
	// before the process died — this is exactly what a restarted
	// TelegramNotifier would redeliver via Telegram's own update queue.
	notifier.approvals <- notify.Decision{ProposalID: "p-resume", Approved: true, At: day(0).Add(90 * time.Minute)}

	if err := eng.ResumePending(ctx); err != nil {
		t.Fatalf("ResumePending: %v", err)
	}

	p, ok, err := st.GetProposal(ctx, "p-resume")
	if err != nil || !ok {
		t.Fatalf("GetProposal: ok=%v err=%v", ok, err)
	}
	if p.Status != store.ProposalSubmitted {
		t.Errorf("status = %s, want SUBMITTED", p.Status)
	}
}

// --- breaker: blocks new entries, never blocks exits (İ7) ------------------

func TestBreakerTrip_BlocksNewEntryButAllowsExit(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	aaa := []domain.Candle{
		{OpenTime: day(0), Open: 100, High: 101, Low: 99, Close: 100, Volume: 1000, QuoteVolume: 100000},
		{OpenTime: day(1), Open: 100, High: 101, Low: 99, Close: 100, Volume: 1000, QuoteVolume: 100000}, // entry fills here (open)
		{OpenTime: day(2), Open: 95, High: 96, Low: 80, Close: 85, Volume: 1000, QuoteVolume: 100000},    // gap through the stop (90) -> loss
	}
	bbb := []domain.Candle{
		{OpenTime: day(0), Open: 50, High: 51, Low: 49, Close: 50, Volume: 1000, QuoteVolume: 50000},
		{OpenTime: day(1), Open: 50, High: 51, Low: 49, Close: 50, Volume: 1000, QuoteVolume: 50000}, // entry fills here
		{OpenTime: day(2), Open: 50, High: 51, Low: 49, Close: 50, Volume: 1000, QuoteVolume: 50000}, // flat; exit signal submitted today
	}
	ccc := []domain.Candle{
		{OpenTime: day(0), Open: 40, High: 41, Low: 39, Close: 40, Volume: 1000, QuoteVolume: 40000},
		{OpenTime: day(1), Open: 40, High: 41, Low: 39, Close: 40, Volume: 1000, QuoteVolume: 40000},
		{OpenTime: day(2), Open: 40, High: 41, Low: 39, Close: 40, Volume: 1000, QuoteVolume: 40000},
	}
	candles := map[string][]domain.Candle{"AAA/USDT": aaa, "BBB/USDT": bbb, "CCC/USDT": ccc}
	seedMarketsAndCandles(t, ctx, st, candles, nil, "USDT")

	ex := &fakeExchange{markets: []domain.Market{
		mustMarket("AAA/USDT", "AAA", "USDT"), mustMarket("BBB/USDT", "BBB", "USDT"), mustMarket("CCC/USDT", "CCC", "USDT"),
	}}
	feed := datafeed.NewFeed(ex, st, "1d", datafeed.WithQuoteFilter("USDT"))

	// Pre-decided history: AAA and BBB were both entered on day(0) — this
	// is exactly what a normal RunOnce(day0) call would have persisted
	// once approved; seeding it directly lets this test focus on day(2)'s
	// breaker behavior without a full multi-day approval round trip.
	aaaStop, bbbStop := 90.0, 1.0
	for _, p := range []store.Proposal{
		{ID: "aaa-entry", CreatedAt: day(0), AsOf: day(0), Symbol: "AAA/USDT", Side: "long", Strategy: "test",
			RefPrice: 100, StopPrice: &aaaStop, Qty: "1", RiskAmount: 10, Reason: "aaa entry", MetricsJSON: "{}",
			Status: store.ProposalApproved, ExpiresAt: day(0).Add(4 * time.Hour), DecidedAt: day(0)},
		{ID: "bbb-entry", CreatedAt: day(0), AsOf: day(0), Symbol: "BBB/USDT", Side: "long", Strategy: "test",
			RefPrice: 50, StopPrice: &bbbStop, Qty: "1", RiskAmount: 10, Reason: "bbb entry", MetricsJSON: "{}",
			Status: store.ProposalApproved, ExpiresAt: day(0).Add(4 * time.Hour), DecidedAt: day(0)},
	} {
		if err := st.InsertProposal(ctx, p); err != nil {
			t.Fatalf("seed proposal %s: %v", p.ID, err)
		}
	}

	notifier := newFakeNotifier()
	notifier.autoApprove = func(p notify.Proposal) (bool, bool) { return true, true } // must never fire for CCC

	strat := fakeStrategy{StratName: "test", EvalFunc: func(in strategy.Input) ([]domain.Signal, error) {
		if !in.AsOf.Equal(day(2)) {
			return nil, nil
		}
		var out []domain.Signal
		if _, open := in.Portfolio.Positions["BBB/USDT"]; open {
			out = append(out, domain.Signal{AsOf: in.AsOf, Symbol: "BBB/USDT", Kind: domain.SignalExit, RefPrice: 50, Reason: "exit while breaker open"})
		}
		out = append(out, domain.Signal{AsOf: in.AsOf, Symbol: "CCC/USDT", Kind: domain.SignalEnter, RefPrice: 40, StopPrice: 35, Reason: "should be blocked"})
		return out, nil
	}}

	breakerCfg := config.BreakerConfig{MaxConsecutiveLosses: 1, MaxDrawdown: 1, MaxDailyLoss: 1, MaxOrderErrors24h: 1000}
	clock := newFakeClock(day(3).Add(5 * time.Minute)) // -> candleDay = day(2)

	eng, err := New(Config{
		Store: st, Feed: feed, Strategy: strat, Notifier: notifier,
		RiskGate: defaultRiskGate(), BreakerCfg: breakerCfg,
		Costs: broker.Costs{FeeRate: 0.001, SlippageBps: 15}, InitialCash: 100000,
		UniverseParams: universe.FilterParams{Quote: "USDT"},
		Timeframe:      "1d", Quote: "USDT",
		ApprovalTTL: time.Hour, PollInterval: time.Millisecond, Clock: clock,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Breaker must have tripped and been persisted (SPEC.md Bölüm 6.5.3).
	raw, ok, err := st.GetState(ctx, "breaker")
	if err != nil || !ok {
		t.Fatalf("GetState(breaker): ok=%v err=%v", ok, err)
	}
	var state risk.State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("unmarshal breaker state: %v", err)
	}
	if !state.Open || state.Reason != risk.BreakerReasonConsecutiveLosses {
		t.Fatalf("breaker state = %+v, want Open=true Reason=%s", state, risk.BreakerReasonConsecutiveLosses)
	}

	// CCC's entry must be rejected specifically because the breaker is
	// open (İ7) — and never reach Telegram.
	rejected, err := st.ListProposalsByStatus(ctx, store.ProposalRejected)
	if err != nil {
		t.Fatalf("ListProposalsByStatus(REJECTED): %v", err)
	}
	foundCCC := false
	for _, p := range rejected {
		if p.Symbol == "CCC/USDT" {
			foundCCC = true
			if !contains(p.Reason, risk.ReasonBreakerOpen) {
				t.Errorf("CCC rejection reason = %q, want it to contain %q", p.Reason, risk.ReasonBreakerOpen)
			}
		}
	}
	if !foundCCC {
		t.Error("expected a REJECTED proposal for CCC/USDT")
	}
	for _, p := range notifier.proposals {
		if p.Symbol == "CCC/USDT" {
			t.Error("CCC/USDT must never reach notify.ProposeTrade once the breaker is open")
		}
	}

	// BBB's exit must have gone through anyway (İ7: exits/stops keep
	// working while the breaker blocks new entries).
	submitted, err := st.ListProposalsByStatus(ctx, store.ProposalSubmitted)
	if err != nil {
		t.Fatalf("ListProposalsByStatus(SUBMITTED): %v", err)
	}
	foundBBBExit := false
	for _, p := range submitted {
		if p.Symbol == "BBB/USDT" && p.Side == "exit" {
			foundBBBExit = true
		}
	}
	if !foundBBBExit {
		t.Error("expected BBB/USDT's exit to be submitted despite the open breaker")
	}

	if !notifier.hasNotificationContaining(notify.LevelCritical, "Devre kesici") {
		t.Error("expected İ7's mandatory breaker-trip critical notification")
	}
	if !notifier.hasNotificationContaining(notify.LevelWarning, "AAA/USDT") {
		t.Error("expected a stop-triggered notification for AAA/USDT")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
