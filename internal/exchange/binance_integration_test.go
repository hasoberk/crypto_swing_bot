//go:build integration

// This file is the "entegrasyon, borsanın testnet'i" test SPEC.md Bölüm 10
// requires for the exchange layer: a full order lifecycle (create -> query
// -> cancel) against Binance's real spot testnet.
//
// It is gated behind the `integration` build tag so `go test ./...` (and
// CI) never depends on network access or live credentials. Run it
// explicitly:
//
//	export EXCHANGE_API_KEY=...      # Binance SPOT TESTNET key, see
//	export EXCHANGE_API_SECRET=...   # https://testnet.binance.vision
//	export EXCHANGE_TESTNET_SYMBOL=BTC/USDT   # optional, defaults below
//	go test ./internal/exchange/... -tags=integration -run TestBinanceTestnet -v
//
// The test is skipped (not failed) if EXCHANGE_API_KEY/EXCHANGE_API_SECRET
// are not set, so it never blocks anyone who hasn't provisioned testnet
// keys. Never point this at production Binance (BaseURL is hardcoded to
// the testnet host below) and never run it with mainnet credentials.
package exchange

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
)

func TestBinanceTestnet_FullOrderLifecycle(t *testing.T) {
	apiKey := os.Getenv("EXCHANGE_API_KEY")
	apiSecret := os.Getenv("EXCHANGE_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		t.Skip("EXCHANGE_API_KEY/EXCHANGE_API_SECRET not set; skipping live testnet integration test")
	}

	symbol := os.Getenv("EXCHANGE_TESTNET_SYMBOL")
	if symbol == "" {
		symbol = "BTC/USDT"
	}

	ex := NewBinanceExchange(BinanceConfig{
		APIKey:          apiKey,
		APISecret:       apiSecret,
		Testnet:         true, // https://testnet.binance.vision — never production
		RateLimitPerMin: 1000,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	markets, err := ex.FetchMarkets(ctx)
	if err != nil {
		t.Fatalf("FetchMarkets: %v", err)
	}
	var market *domain.Market
	for i := range markets {
		if markets[i].Symbol == symbol {
			market = &markets[i]
			break
		}
	}
	if market == nil {
		t.Fatalf("symbol %s not found on testnet", symbol)
	}

	// A LIMIT BUY far below market price: it will sit as NEW and won't
	// fill during the test, which lets us exercise FetchOrder and
	// CancelOrder deterministically without racing a market fill.
	price := decimal.NewFromInt(1000) // far below any realistic BTC/USDT price
	qty := market.StepSize
	if qty.IsZero() {
		qty = decimal.RequireFromString("0.0001")
	}
	// Respect MinNotional so the exchange doesn't reject the order for
	// an unrelated reason.
	minQtyForNotional := market.MinNotional.Div(price)
	if qty.LessThan(minQtyForNotional) {
		qty = minQtyForNotional.Div(market.StepSize).Ceil().Mul(market.StepSize)
	}

	clientOrderID := "swingbot-it-" + time.Now().UTC().Format("20060102150405.000000000")
	req := domain.OrderRequest{
		ClientOrderID: clientOrderID,
		Symbol:        symbol,
		Side:          domain.SideBuy,
		Type:          "limit",
		Qty:           qty,
		Price:         price,
	}

	created, err := ex.CreateOrder(ctx, req)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if created.Order.ClientOrderID != clientOrderID {
		t.Fatalf("expected ClientOrderID %q, got %q", clientOrderID, created.Order.ClientOrderID)
	}
	if len(created.Raw) == 0 {
		t.Fatal("expected CreateOrder to return the raw exchange response for orders.raw_json")
	}
	t.Logf("created order id=%s status=%s", created.Order.ID, created.Order.Status)

	// Idempotency (İ5): re-submitting the exact same ClientOrderID must
	// resolve to the same order, not create a second one.
	dup, err := ex.CreateOrder(ctx, req)
	if err != nil {
		t.Fatalf("CreateOrder (duplicate ClientOrderID) should resolve idempotently, got error: %v", err)
	}
	if dup.Order.ID != created.Order.ID {
		t.Fatalf("expected duplicate CreateOrder to resolve to order id %s, got %s", created.Order.ID, dup.Order.ID)
	}

	fetched, err := ex.FetchOrder(ctx, symbol, created.Order.ID)
	if err != nil {
		t.Fatalf("FetchOrder: %v", err)
	}
	if fetched.Order.ClientOrderID != clientOrderID {
		t.Fatalf("FetchOrder returned unexpected order: %+v", fetched.Order)
	}

	if err := ex.CancelOrder(ctx, symbol, created.Order.ID); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	afterCancel, err := ex.FetchOrder(ctx, symbol, created.Order.ID)
	if err != nil {
		t.Fatalf("FetchOrder after cancel: %v", err)
	}
	if afterCancel.Order.Status != "CANCELED" {
		t.Fatalf("expected status CANCELED after cancel, got %q", afterCancel.Order.Status)
	}

	// A FetchOrder for a genuinely unknown id must map to ErrOrderNotFound.
	if _, err := ex.FetchOrder(ctx, symbol, "0"); err == nil || !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("expected ErrOrderNotFound for order id 0, got: %v", err)
	}
}
