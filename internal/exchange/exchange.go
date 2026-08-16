// Package exchange talks to a cryptocurrency exchange over its REST API.
//
// SPEC.md Bölüm 5.2: "Yalnızca borsayla konuşur. İş mantığı içermez." This
// package therefore:
//   - imports only internal/domain (never internal/store or
//     internal/strategy — see SPEC.md Bölüm 3's İ1 rule),
//   - never filters "closed vs. still-open" candles (that is datafeed's job,
//     SPEC.md Bölüm 4.2 / İ2),
//   - never decides whether an order is a good idea (that is risk/broker's
//     job) — it only places the order it is told to place, safely.
package exchange

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
)

// Exchange is SPEC.md Bölüm 5.2's Exchange arayüzü.
//
// Method names, parameters and the overall shape are copied verbatim from
// the spec. The one deliberate deviation is CreateOrder/FetchOrder's return
// type: the spec's snippet returns a bare domain.Order, but Bölüm 5.2 also
// requires "Tüm ham yanıtları orders.raw_json alanına yaz". domain.Order
// (owned by internal/domain) has no raw-response field, and this package
// must not add one there — domain.go is meant to stay a pure, dependency-free
// vocabulary shared by every layer (backtest's PaperBroker fills a
// domain.Order out of a simulated candle; it has no "raw JSON" to report).
// So CreateOrder/FetchOrder return OrderResult: the parsed Order plus the
// untouched raw response body, letting a caller such as broker.LiveBroker
// persist both the structured columns and orders.raw_json from one call.
type Exchange interface {
	// Name identifies the exchange, e.g. "binance". Matches
	// config.yaml's exchange.name.
	Name() string

	// FetchMarkets returns the symbols the exchange currently lists,
	// including the trading filters (tick/step/min-notional) any order
	// for that symbol must respect. It reflects "today" only — SPEC.md
	// Bölüm 6.1's delisted-symbol bookkeeping (active=0, delisted_at) is
	// datafeed's responsibility, computed by diffing successive
	// FetchMarkets snapshots against the markets table, not something
	// this package can know on its own.
	FetchMarkets(ctx context.Context) ([]domain.Market, error)

	// FetchOHLCV returns candles for symbol/timeframe starting at since,
	// up to limit bars. It MAY return a still-open (not yet closed)
	// candle as its last element — SPEC.md Bölüm 4.2/5.2 assigns
	// filtering that out to internal/datafeed (İ2: "Sinyal t, işlem
	// t+1"), not to this package.
	FetchOHLCV(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]domain.Candle, error)

	// FetchBalance returns free+locked balance per asset (e.g. "USDT",
	// "BTC"). Assets with zero balance may be omitted.
	FetchBalance(ctx context.Context) (map[string]decimal.Decimal, error)

	// CreateOrder submits req and is idempotent on req.ClientOrderID
	// (İ5). SPEC.md Bölüm 5.2/6.6.2 requirement: on an ambiguous
	// network failure (timeout, 5xx, 429/418 — anything where we cannot
	// tell whether the exchange actually received and processed the
	// request) the implementation MUST call
	// FetchOrder(ctx, req.Symbol, req.ClientOrderID) to check whether
	// the order was actually created BEFORE resubmitting. It must never
	// blindly retry a create — that is exactly the double-position risk
	// İ5 exists to prevent. A definitive rejection (e.g. insufficient
	// balance, bad params) must be returned as an error without retry.
	CreateOrder(ctx context.Context, req domain.OrderRequest) (OrderResult, error)

	// FetchOrder looks an order up by symbol and id. id may be either
	// the exchange's own order id (domain.Order.ID) or a ClientOrderID —
	// implementations must accept both, since CreateOrder's idempotency
	// check above calls this with a ClientOrderID. Returns
	// ErrOrderNotFound (use errors.Is) if the exchange has no order
	// matching id for symbol.
	FetchOrder(ctx context.Context, symbol, id string) (OrderResult, error)

	// CancelOrder cancels an open order. Canceling an already
	// closed/canceled order is not required to be an error; unlike
	// CreateOrder, repeating a cancel has no double-position risk, so
	// implementations may retry it freely on ambiguous failures.
	CancelOrder(ctx context.Context, symbol, id string) error
}

// OrderResult pairs a parsed domain.Order with the exchange's raw response
// body for that order (SPEC.md Bölüm 5.2: "Tüm ham yanıtları orders.raw_json
// alanına yaz"; Bölüm 4.1's orders.raw_json column). Raw is nil when there
// is genuinely no wire response to keep (e.g. a synthetic PaperBroker fill
// outside this package).
type OrderResult struct {
	Order domain.Order
	Raw   json.RawMessage
}
