package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
	"swingbot/internal/exchange"
	"swingbot/internal/notify"
	"swingbot/internal/store"
	"swingbot/internal/strategy"
)

// day returns 2024-01-01 + n days, UTC midnight — matches the daily candle
// grid RunOnce/reconstruct operate on.
func day(n int) time.Time {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

// --- fakeClock ---------------------------------------------------------

// fakeClock is a manually driven broker.Clock double: Sleep advances Now()
// immediately instead of really waiting, so awaitDecisions/Run's
// scheduling loop never block a test in real time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// --- fakeNotifier --------------------------------------------------------

type fakeNotification struct {
	Level notify.Level
	Title string
	Body  string
}

// fakeNotifier is a notify.Notifier double. autoApprove, if set, is
// consulted synchronously from ProposeTrade to decide whether (and how) to
// immediately push a Decision — tests that want to control timing instead
// push directly onto Approvals().
type fakeNotifier struct {
	mu            sync.Mutex
	proposals     []notify.Proposal
	notifications []fakeNotification
	approvals     chan notify.Decision
	autoApprove   func(p notify.Proposal) (approved, decide bool)
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{approvals: make(chan notify.Decision, 64)}
}

func (n *fakeNotifier) ProposeTrade(ctx context.Context, p notify.Proposal) error {
	n.mu.Lock()
	n.proposals = append(n.proposals, p)
	auto := n.autoApprove
	n.mu.Unlock()
	if auto != nil {
		if approved, decide := auto(p); decide {
			n.approvals <- notify.Decision{ProposalID: p.ID, Approved: approved, At: time.Now().UTC()}
		}
	}
	return nil
}

func (n *fakeNotifier) Notify(ctx context.Context, level notify.Level, title, body string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notifications = append(n.notifications, fakeNotification{level, title, body})
	return nil
}

func (n *fakeNotifier) Approvals() <-chan notify.Decision { return n.approvals }

func (n *fakeNotifier) hasNotificationContaining(level notify.Level, substr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, note := range n.notifications {
		if note.Level == level && (substr == "" || strings.Contains(note.Title+"\n"+note.Body, substr)) {
			return true
		}
	}
	return false
}

// --- fakeStrategy ----------------------------------------------------------

// fakeStrategy hands test-controlled signals straight through, ignoring
// Series/Universe when the test doesn't care about them — tests decide
// entirely from in.AsOf/in.Portfolio.
type fakeStrategy struct {
	StratName string
	Warmup    int
	EvalFunc  func(in strategy.Input) ([]domain.Signal, error)
}

func (s fakeStrategy) Name() string           { return s.StratName }
func (s fakeStrategy) WarmupBars() int        { return s.Warmup }
func (s fakeStrategy) Params() map[string]any { return nil }
func (s fakeStrategy) Evaluate(in strategy.Input) ([]domain.Signal, error) {
	if s.EvalFunc == nil {
		return nil, nil
	}
	return s.EvalFunc(in)
}

// --- fakeExchange ------------------------------------------------------

// fakeExchange never touches a network: FetchMarkets returns a fixed list
// (tests seed the store's candles directly, independent of this) and
// FetchOHLCV always reports "nothing new" so datafeed.Feed.Update/Verify
// are effectively no-ops against store data the test already wrote.
type fakeExchange struct {
	markets []domain.Market
}

func (f *fakeExchange) Name() string { return "fake" }
func (f *fakeExchange) FetchMarkets(ctx context.Context) ([]domain.Market, error) {
	return f.markets, nil
}
func (f *fakeExchange) FetchOHLCV(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]domain.Candle, error) {
	return nil, nil
}
func (f *fakeExchange) FetchBalance(ctx context.Context) (map[string]decimal.Decimal, error) {
	return nil, nil
}
func (f *fakeExchange) CreateOrder(ctx context.Context, req domain.OrderRequest) (exchange.OrderResult, error) {
	return exchange.OrderResult{}, exchange.ErrOrderNotFound
}
func (f *fakeExchange) FetchOrder(ctx context.Context, symbol, id string) (exchange.OrderResult, error) {
	return exchange.OrderResult{}, exchange.ErrOrderNotFound
}
func (f *fakeExchange) CancelOrder(ctx context.Context, symbol, id string) error { return nil }

// --- store seeding helpers -------------------------------------------------

func mustMarket(symbol, base, quote string) domain.Market {
	return domain.Market{
		Symbol: symbol, Base: base, Quote: quote, Active: true,
		TickSize: decimal.NewFromFloat(0.01), StepSize: decimal.NewFromFloat(0.0001),
		MinNotional: decimal.NewFromFloat(1),
	}
}

// flatCandles returns n daily candles, starting at day(from), with a fixed
// close price and enough High/Low room that a stop at stopPrice is NEVER
// touched — used for symbols a test wants to hold steady.
func flatCandles(from, n int, price float64) []domain.Candle {
	out := make([]domain.Candle, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Candle{
			OpenTime: day(from + i), Open: price, High: price * 1.02, Low: price * 0.98, Close: price,
			Volume: 1000, QuoteVolume: price * 1000,
		}
	}
	return out
}

func seedMarketsAndCandles(t *testing.T, ctx context.Context, st *store.Store, candles map[string][]domain.Candle, base map[string]string, quote string) []domain.Market {
	t.Helper()
	var markets []domain.Market
	for symbol, series := range candles {
		b := base[symbol]
		if b == "" {
			b = symbol
		}
		m := mustMarket(symbol, b, quote)
		markets = append(markets, m)
		if err := st.UpsertMarket(ctx, store.Market{
			Symbol: m.Symbol, Base: m.Base, Quote: m.Quote, Active: true,
			TickSize: m.TickSize.String(), StepSize: m.StepSize.String(), MinNotional: m.MinNotional.String(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed market %s: %v", symbol, err)
		}
		if err := st.UpsertCandles(ctx, symbol, "1d", series); err != nil {
			t.Fatalf("seed candles %s: %v", symbol, err)
		}
	}
	return markets
}
