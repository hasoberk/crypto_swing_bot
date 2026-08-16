package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
)

// fastBackoff keeps retry-driven tests fast and deterministic-ish without
// real multi-second sleeps.
var fastBackoff = BackoffPolicy{Base: time.Millisecond, Max: 5 * time.Millisecond, MaxRetries: 5}

func newTestExchange(t *testing.T, srv *httptest.Server) *BinanceExchange {
	t.Helper()
	ex := NewBinanceExchange(BinanceConfig{
		APIKey:          "test-key",
		APISecret:       "test-secret",
		BaseURL:         srv.URL,
		RateLimitPerMin: 6000, // effectively unlimited for these tests
		Backoff:         fastBackoff,
	})
	t.Cleanup(srv.Close)
	return ex
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func binanceOrderJSON(orderID int64, clientOrderID, symbol, side, status string, qty, price string) map[string]any {
	return map[string]any{
		"symbol":              symbol,
		"orderId":             orderID,
		"clientOrderId":       clientOrderID,
		"price":               price,
		"origQty":             qty,
		"executedQty":         qty,
		"cummulativeQuoteQty": "0",
		"status":              status,
		"type":                "MARKET",
		"side":                side,
		"transactTime":        time.Now().UnixMilli(),
		"fills":               []any{},
	}
}

// --- CreateOrder idempotency: internal automatic retry ---

// TestCreateOrder_VerifiesBeforeRetryOnAmbiguousFailure covers SPEC.md
// Bölüm 5.2/6.6.2's mandatory rule: on an ambiguous failure (here: a 503
// from the exchange), CreateOrder must call FetchOrder with the
// ClientOrderID BEFORE sending a second POST, and if that turns up the
// order (i.e. the first, failed-looking attempt actually went through),
// it must return that order instead of submitting a duplicate.
func TestCreateOrder_VerifiesBeforeRetryOnAmbiguousFailure(t *testing.T) {
	const clientOrderID = "coid-ambiguous-1"
	var postCount, getCount int32
	var mu sync.Mutex
	var stored map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			n := atomic.AddInt32(&postCount, 1)
			if n > 1 {
				t.Errorf("CreateOrder must not send a second POST once FetchOrder confirms the order already exists; got POST #%d", n)
			}
			// Simulate: the exchange actually processed the order, but
			// the client will experience this as a failure (e.g. the
			// response never arrived, or arrived as a 503). We still
			// store the order server-side to model that ambiguity.
			mu.Lock()
			stored = binanceOrderJSON(555, clientOrderID, "BTCUSDT", "BUY", "FILLED", "1", "50000")
			mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":-1001,"msg":"Internal error; unable to process your request. Please try again."}`))
		case http.MethodGet:
			atomic.AddInt32(&getCount, 1)
			mu.Lock()
			defer mu.Unlock()
			q := r.URL.Query()
			if q.Get("origClientOrderId") != clientOrderID {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
				return
			}
			if stored == nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
				return
			}
			writeJSON(w, http.StatusOK, stored)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	ex := newTestExchange(t, httptest.NewServer(mux))

	req := domain.OrderRequest{
		ClientOrderID: clientOrderID,
		Symbol:        "BTC/USDT",
		Side:          domain.SideBuy,
		Type:          "market",
		Qty:           decimal.NewFromInt(1),
	}

	res, err := ex.CreateOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateOrder returned error, expected it to recover via FetchOrder verification: %v", err)
	}
	if res.Order.ClientOrderID != clientOrderID {
		t.Fatalf("expected ClientOrderID %q, got %q", clientOrderID, res.Order.ClientOrderID)
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("expected exactly 1 POST /api/v3/order, got %d", got)
	}
	if got := atomic.LoadInt32(&getCount); got < 1 {
		t.Fatalf("expected CreateOrder to call FetchOrder (GET) before any retry, got %d GETs", got)
	}
}

// --- CreateOrder idempotency: two top-level calls, same ClientOrderID ---

// TestCreateOrder_SameClientOrderID_CalledTwice_CreatesOnlyOneOrder is the
// idempotency acceptance test: calling CreateOrder twice with the same
// ClientOrderID (simulating a caller that itself retries, e.g. after a
// process restart per SPEC.md Bölüm 6.6.2) must result in exactly one order
// on the exchange, never two positions.
func TestCreateOrder_SameClientOrderID_CalledTwice_CreatesOnlyOneOrder(t *testing.T) {
	const clientOrderID = "coid-duplicate-1"

	var mu sync.Mutex
	orders := map[string]map[string]any{} // clientOrderId -> order
	nextID := int64(1000)
	var postCount int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			atomic.AddInt32(&postCount, 1)
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			coid := values.Get("newClientOrderId")

			mu.Lock()
			defer mu.Unlock()
			if existing, ok := orders[coid]; ok {
				_ = existing
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2010, "msg": "Duplicate order sent."})
				return
			}
			nextID++
			o := binanceOrderJSON(nextID, coid, values.Get("symbol"), values.Get("side"), "FILLED", values.Get("quantity"), "50000")
			orders[coid] = o
			writeJSON(w, http.StatusOK, o)
		case http.MethodGet:
			mu.Lock()
			defer mu.Unlock()
			coid := r.URL.Query().Get("origClientOrderId")
			o, ok := orders[coid]
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
				return
			}
			writeJSON(w, http.StatusOK, o)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	ex := newTestExchange(t, httptest.NewServer(mux))

	req := domain.OrderRequest{
		ClientOrderID: clientOrderID,
		Symbol:        "ETH/USDT",
		Side:          domain.SideBuy,
		Type:          "market",
		Qty:           decimal.NewFromInt(2),
	}

	first, err := ex.CreateOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("first CreateOrder call failed: %v", err)
	}
	second, err := ex.CreateOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateOrder call (same ClientOrderID) failed: %v", err)
	}

	if first.Order.ID != second.Order.ID {
		t.Fatalf("expected both calls to resolve to the same order id, got %q and %q", first.Order.ID, second.Order.ID)
	}

	mu.Lock()
	n := len(orders)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected exactly 1 order to exist after 2 CreateOrder calls with the same ClientOrderID, got %d", n)
	}
	if got := atomic.LoadInt32(&postCount); got != 2 {
		t.Fatalf("expected both calls to reach the exchange (2 POSTs, second rejected as duplicate), got %d", got)
	}
}

// TestCreateOrder_DefinitiveRejectionDoesNotRetry ensures a business-level
// rejection (e.g. insufficient balance) is returned immediately and never
// triggers the retry-with-FetchOrder path (there's nothing ambiguous about
// it — the exchange plainly refused the order).
func TestCreateOrder_DefinitiveRejectionDoesNotRetry(t *testing.T) {
	var postCount, getCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			atomic.AddInt32(&postCount, 1)
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2010, "msg": "Account has insufficient balance for requested action."})
		case http.MethodGet:
			atomic.AddInt32(&getCount, 1)
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
		}
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	req := domain.OrderRequest{
		ClientOrderID: "coid-insufficient-balance",
		Symbol:        "BTC/USDT",
		Side:          domain.SideBuy,
		Type:          "market",
		Qty:           decimal.NewFromInt(1),
	}
	_, err := ex.CreateOrder(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error for a definitive (non-ambiguous) rejection")
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("expected exactly 1 POST for a definitive rejection (no retry), got %d", got)
	}
	if got := atomic.LoadInt32(&getCount); got != 0 {
		t.Fatalf("expected no FetchOrder verification call for a definitive rejection, got %d", got)
	}
}

// --- rate limit / 429 backoff ---

// TestFetchOrder_RetriesWithBackoffOn429 simulates a rate-limited exchange
// (SPEC.md Bölüm 5.2: 429/418 -> exponential backoff + jitter) and asserts
// the call eventually succeeds after transient 429s, without the caller
// having to do anything.
func TestFetchOrder_RetriesWithBackoffOn429(t *testing.T) {
	var attempts int32
	const failuresBeforeSuccess = 3

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= failuresBeforeSuccess {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":-1003,"msg":"Too many requests."}`))
			return
		}
		writeJSON(w, http.StatusOK, binanceOrderJSON(42, "coid-429", "BTCUSDT", "BUY", "FILLED", "1", "50000"))
	})

	ex := newTestExchange(t, httptest.NewServer(mux))

	start := time.Now()
	res, err := ex.FetchOrder(context.Background(), "BTC/USDT", "42")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected FetchOrder to succeed after retrying past 429s, got error: %v", err)
	}
	if res.Order.ID != "42" {
		t.Fatalf("expected order id 42, got %s", res.Order.ID)
	}
	if got := atomic.LoadInt32(&attempts); got != failuresBeforeSuccess+1 {
		t.Fatalf("expected %d attempts (%d failures + 1 success), got %d", failuresBeforeSuccess+1, failuresBeforeSuccess, got)
	}
	// Sanity: backoff actually introduced some delay (not a busy-loop),
	// but the fast test policy keeps it well under a second.
	if elapsed > 2*time.Second {
		t.Fatalf("retry+backoff took implausibly long for a test: %v", elapsed)
	}
}

// TestFetchOrder_418AlsoTriggersBackoffRetry covers Binance's IP-ban status
// code 418 (distinct from 429) which SPEC.md Bölüm 5.2 explicitly calls out
// alongside 429.
func TestFetchOrder_418AlsoTriggersBackoffRetry(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(418)
			_, _ = w.Write([]byte(`{"code":-1003,"msg":"IP banned."}`))
			return
		}
		writeJSON(w, http.StatusOK, binanceOrderJSON(7, "coid-418", "BTCUSDT", "BUY", "FILLED", "1", "50000"))
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	_, err := ex.FetchOrder(context.Background(), "BTC/USDT", "7")
	if err != nil {
		t.Fatalf("expected FetchOrder to recover after a 418, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected 2 attempts (1 x 418 + 1 success), got %d", got)
	}
}

// TestFetchOrder_GivesUpAfterMaxRetries ensures a persistently failing
// exchange eventually surfaces an error rather than retrying forever.
func TestFetchOrder_GivesUpAfterMaxRetries(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":-1003,"msg":"Too many requests."}`))
	})
	ex := newTestExchange(t, httptest.NewServer(mux))
	ex.backoff = BackoffPolicy{Base: time.Millisecond, Max: 2 * time.Millisecond, MaxRetries: 2}

	_, err := ex.FetchOrder(context.Background(), "BTC/USDT", "1")
	if err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 { // 1 initial + 2 retries
		t.Fatalf("expected 3 attempts (1 + MaxRetries=2), got %d", got)
	}
}

// --- FetchOrder: not-found mapping ---

func TestFetchOrder_NotFoundMapsToErrOrderNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	_, err := ex.FetchOrder(context.Background(), "BTC/USDT", "does-not-exist")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("expected errors.Is(err, ErrOrderNotFound), got: %v", err)
	}
}

// --- FetchMarkets / FetchOHLCV / FetchBalance / CancelOrder smoke tests ---

func TestFetchMarkets_ParsesFiltersAndActiveFlag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/exchangeInfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"symbols": []map[string]any{
				{
					"symbol": "BTCUSDT", "baseAsset": "BTC", "quoteAsset": "USDT", "status": "TRADING",
					"filters": []map[string]any{
						{"filterType": "PRICE_FILTER", "tickSize": "0.01"},
						{"filterType": "LOT_SIZE", "stepSize": "0.00001"},
						{"filterType": "NOTIONAL", "notional": "5.00"},
					},
				},
				{
					"symbol": "OLDCOINUSDT", "baseAsset": "OLDCOIN", "quoteAsset": "USDT", "status": "BREAK",
					"filters": []map[string]any{},
				},
			},
		})
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	markets, err := ex.FetchMarkets(context.Background())
	if err != nil {
		t.Fatalf("FetchMarkets: %v", err)
	}
	if len(markets) != 2 {
		t.Fatalf("expected 2 markets, got %d", len(markets))
	}
	btc := markets[0]
	if btc.Symbol != "BTC/USDT" || !btc.Active {
		t.Fatalf("unexpected BTC market: %+v", btc)
	}
	if !btc.TickSize.Equal(decimal.RequireFromString("0.01")) {
		t.Fatalf("unexpected TickSize: %v", btc.TickSize)
	}
	if !btc.StepSize.Equal(decimal.RequireFromString("0.00001")) {
		t.Fatalf("unexpected StepSize: %v", btc.StepSize)
	}
	if !btc.MinNotional.Equal(decimal.RequireFromString("5.00")) {
		t.Fatalf("unexpected MinNotional: %v", btc.MinNotional)
	}
	if markets[1].Active {
		t.Fatalf("expected OLDCOIN (status BREAK) to be Active=false")
	}
}

func TestFetchOHLCV_ParsesKlinesAndDoesNotFilterLastCandle(t *testing.T) {
	openTime := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/klines", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			[]any{openTime, "100.0", "110.0", "95.0", "105.0", "10.5", openTime + 86399999, "1000.0", 5, "0", "0", "0"},
			// second candle simulates a "still open" trailing candle;
			// this package must return it as-is (İ2 filtering is
			// datafeed's job, not exchange's).
			[]any{openTime + 86400000, "105.0", "108.0", "104.0", "106.5", "3.2", openTime + 172799999, "340.0", 2, "0", "0", "0"},
		})
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	candles, err := ex.FetchOHLCV(context.Background(), "BTC/USDT", "1d", time.Time{}, 2)
	if err != nil {
		t.Fatalf("FetchOHLCV: %v", err)
	}
	if len(candles) != 2 {
		t.Fatalf("expected FetchOHLCV to return both candles (no filtering), got %d", len(candles))
	}
	c := candles[0]
	if c.Open != 100.0 || c.High != 110.0 || c.Low != 95.0 || c.Close != 105.0 {
		t.Fatalf("unexpected OHLC: %+v", c)
	}
	if !c.OpenTime.Equal(time.UnixMilli(openTime).UTC()) {
		t.Fatalf("unexpected OpenTime: %v", c.OpenTime)
	}
}

func TestFetchBalance_SumsFreeAndLockedOmitsZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/account", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"balances": []map[string]any{
				{"asset": "USDT", "free": "100.5", "locked": "10.0"},
				{"asset": "BTC", "free": "0", "locked": "0"},
			},
		})
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	bal, err := ex.FetchBalance(context.Background())
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if _, ok := bal["BTC"]; ok {
		t.Fatal("expected zero-balance BTC to be omitted")
	}
	want := decimal.RequireFromString("110.5")
	if !bal["USDT"].Equal(want) {
		t.Fatalf("expected USDT balance %v, got %v", want, bal["USDT"])
	}
}

func TestCancelOrder_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		writeJSON(w, http.StatusOK, binanceOrderJSON(9, "coid-cancel", "BTCUSDT", "BUY", "CANCELED", "1", "50000"))
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	if err := ex.CancelOrder(context.Background(), "BTC/USDT", "9"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
}

// --- request signing sanity ---

func TestSignedRequest_IncludesSignatureAndAPIKeyHeader(t *testing.T) {
	var gotAPIKeyHeader string
	var gotParams url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/account", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKeyHeader = r.Header.Get("X-MBX-APIKEY")
		gotParams = r.URL.Query()
		writeJSON(w, http.StatusOK, map[string]any{"balances": []any{}})
	})
	ex := newTestExchange(t, httptest.NewServer(mux))

	if _, err := ex.FetchBalance(context.Background()); err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if gotAPIKeyHeader != "test-key" {
		t.Fatalf("expected X-MBX-APIKEY header, got %q", gotAPIKeyHeader)
	}
	if gotParams.Get("signature") == "" {
		t.Fatal("expected a signature query parameter on a signed request")
	}
	if gotParams.Get("timestamp") == "" {
		t.Fatal("expected a timestamp query parameter on a signed request")
	}
}
