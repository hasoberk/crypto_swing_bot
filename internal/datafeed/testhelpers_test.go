package datafeed

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
	"swingbot/internal/exchange"
)

// fetchCall records one FetchOHLCV invocation seen by fakeExchange, so tests
// can assert on pagination behavior (e.g. that a resumed backfill starts
// after the last stored candle, not from the beginning).
type fetchCall struct {
	symbol string
	since  time.Time
	limit  int
}

// fakeExchange is a minimal, in-memory exchange.Exchange used by every test
// in this package. It never touches the network. Only FetchMarkets and
// FetchOHLCV are exercised by datafeed; the order-lifecycle methods are
// stubbed out since datafeed never calls them (SPEC.md Bölüm 6.1's scope is
// read-only market data).
type fakeExchange struct {
	markets []domain.Market
	candles map[string][]domain.Candle // symbol -> full dataset, any order
	err     error                      // if set, FetchOHLCV always returns this error
	calls   []fetchCall
}

func newFakeExchange() *fakeExchange {
	return &fakeExchange{candles: map[string][]domain.Candle{}}
}

func (f *fakeExchange) Name() string { return "fake" }

func (f *fakeExchange) FetchMarkets(ctx context.Context) ([]domain.Market, error) {
	return f.markets, nil
}

func (f *fakeExchange) FetchOHLCV(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]domain.Candle, error) {
	f.calls = append(f.calls, fetchCall{symbol: symbol, since: since, limit: limit})
	if f.err != nil {
		return nil, f.err
	}
	all := f.candles[symbol]
	var out []domain.Candle
	for _, c := range all {
		if !c.OpenTime.Before(since) {
			out = append(out, c)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeExchange) FetchBalance(ctx context.Context) (map[string]decimal.Decimal, error) {
	return nil, errors.New("fakeExchange: FetchBalance not implemented")
}

func (f *fakeExchange) CreateOrder(ctx context.Context, req domain.OrderRequest) (exchange.OrderResult, error) {
	return exchange.OrderResult{}, errors.New("fakeExchange: CreateOrder not implemented")
}

func (f *fakeExchange) FetchOrder(ctx context.Context, symbol, id string) (exchange.OrderResult, error) {
	return exchange.OrderResult{}, errors.New("fakeExchange: FetchOrder not implemented")
}

func (f *fakeExchange) CancelOrder(ctx context.Context, symbol, id string) error {
	return errors.New("fakeExchange: CancelOrder not implemented")
}

var _ exchange.Exchange = (*fakeExchange)(nil)

// dailyCandles generates n consecutive, well-formed daily candles starting
// at start (which should already be UTC midnight), with a mild upward drift
// so close/prev_close never trips the outlier-jump check by accident.
func dailyCandles(start time.Time, n int) []domain.Candle {
	out := make([]domain.Candle, 0, n)
	price := 100.0
	for i := 0; i < n; i++ {
		open := price
		close := price * 1.001
		high := close + 0.5
		low := open - 0.5
		out = append(out, domain.Candle{
			OpenTime:    start.AddDate(0, 0, i),
			Open:        open,
			High:        high,
			Low:         low,
			Close:       close,
			Volume:      1000,
			QuoteVolume: 1000 * close,
		})
		price = close
	}
	return out
}
