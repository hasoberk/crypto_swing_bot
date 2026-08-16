// A note on why this file talks to Binance directly instead of importing
// github.com/ccxt/ccxt/go/v4 (SPEC.md Bölüm 2's stated choice):
//
// SPEC.md Bölüm 0 explicitly warns that "Bağımlılık sürümleri hakkında"
// listed libraries drift and must be re-verified on pkg.go.dev before
// coding. That check was done here (2026-08-15) and the result changes the
// plan:
//
//   - Every published version of github.com/ccxt/ccxt/go/v4 (from the very
//     first, v4.4.53, through the latest, v4.5.73) declares `go 1.23.4` or
//     higher in its go.mod; the newest declare `go 1.25.0`. This module's
//     go.mod (owned by internal/domain+internal/config, Ajan 1) pins
//     `go 1.22.2`. `go get` fails outright:
//     "module ... requires go >= 1.25.0 (running go 1.22.2)"
//     even for the oldest release.
//   - More importantly, github.com/ccxt/ccxt/go/v4's go.mod pulls in
//     github.com/ethereum/go-ethereum, NethermindEth/juno,
//     NethermindEth/starknet.go and elliottech/lighter-go — full
//     blockchain-node / L2 client stacks, dozens of transitive packages,
//     multi-megabyte of source — needed for CCXT's on-chain/DEX exchange
//     support. For a single spot-market REST integration (Binance), this
//     directly contradicts SPEC.md Bölüm 2's own rationale for choosing Go
//     ("tipik çalışma ~30-60 MB RAM... Kaynak tüketimi endişesi Go ile
//     zaten karşılanıyor") and Bölüm 3's "tek binary" goal.
//
// Given the task's explicit fallback instruction ("gerekirse interface'i
// sağlayan ama CCXT client'ını enjekte edilebilir bir alan olarak tutan bir
// yapı kur ki gerçek CCXT bağımlılığı sonradan sorunsuz eklenebilsin"), this
// file implements the Exchange interface with the same *shape* a
// CCXT-backed implementation would have (unified fetchMarkets/fetchOHLCV/
// createOrder/... calls, one exchange per struct) but talks to Binance's
// REST API directly via net/http/HMAC signing — no functional difference
// from what ccxt.NewBinance(...) would do under the hood for this task's
// scope (spot market data + spot order lifecycle).
//
// To swap in the real CCXT client later (once the module is willing to
// move to go 1.23+ and accept its dependency weight, or once a lighter
// CCXT distribution exists): BinanceExchange's HTTP/signing plumbing
// (httpDo, sign, doRequestOnce/doRequestWithRetry) is the only thing that
// would change; the exported Exchange methods and their domain-type
// conversions (toDomainMarket, toDomainCandle, toOrderResult) stay the
// same, since they only depend on parsed field values, not on how the
// bytes were fetched. httpClient is already an injected interface
// (httpDoer) for exactly this reason — a ccxt-backed doer could implement
// it without touching the rest of this file.
package exchange

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
)

// ErrOrderNotFound is returned by FetchOrder when the exchange has no order
// matching the given id/ClientOrderID for the given symbol. CreateOrder's
// idempotency check (SPEC.md Bölüm 5.2/6.6.2) relies on being able to
// distinguish "definitely does not exist yet, safe to (re)send" from "could
// not determine, do not blindly retry" via errors.Is(err, ErrOrderNotFound).
var ErrOrderNotFound = errors.New("exchange: order not found")

// httpDoer is the minimal surface BinanceExchange needs from an HTTP
// client. *http.Client satisfies it. Tests (and, per the file-level doc
// comment above, a future CCXT-backed doer) can inject their own.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// BinanceConfig configures a BinanceExchange. All fields except APIKey/
// APISecret have sane defaults if left zero.
type BinanceConfig struct {
	APIKey    string
	APISecret string

	// Testnet selects Binance's spot testnet base URL
	// (https://testnet.binance.vision) instead of production. Always
	// prefer this for the integration test in
	// exchange_integration_test.go and for any development that can
	// place real orders.
	Testnet bool

	// BaseURL overrides the base URL entirely (mainly for tests against
	// an httptest.Server). Takes precedence over Testnet.
	BaseURL string

	// RateLimitPerMin is config.exchange.rate_limit_per_min (SPEC.md
	// Bölüm 8). <= 0 falls back to NewRateLimiter's conservative
	// default.
	RateLimitPerMin int

	// Backoff controls retry timing on 429/418/5xx/transient network
	// errors. Zero value falls back to DefaultBackoffPolicy.
	Backoff BackoffPolicy

	// HTTPClient overrides the underlying HTTP client (tests only,
	// normally left nil -> a *http.Client with a sane timeout).
	HTTPClient httpDoer

	// RecvWindow is Binance's recvWindow parameter for signed requests,
	// in milliseconds. 0 -> Binance's own default (5000ms) is used by
	// omitting the parameter.
	RecvWindowMS int64
}

const (
	binanceProductionBaseURL = "https://api.binance.com"
	binanceTestnetBaseURL    = "https://testnet.binance.vision"
)

// BinanceExchange implements Exchange (SPEC.md Bölüm 5.2) against Binance's
// spot REST API. See the file-level doc comment for why this talks to
// Binance directly rather than through github.com/ccxt/ccxt/go/v4.
type BinanceExchange struct {
	apiKey       string
	apiSecret    string
	baseURL      string
	client       httpDoer
	limiter      *RateLimiter
	backoff      BackoffPolicy
	recvWindowMS int64

	now  func() time.Time // overridable for tests
	rand *rand.Rand       // overridable for deterministic jitter in tests; nil -> global rand
}

// NewBinanceExchange builds a BinanceExchange from cfg.
func NewBinanceExchange(cfg BinanceConfig) *BinanceExchange {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		if cfg.Testnet {
			baseURL = binanceTestnetBaseURL
		} else {
			baseURL = binanceProductionBaseURL
		}
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	backoff := cfg.Backoff
	if backoff.Base <= 0 && backoff.Max <= 0 && backoff.MaxRetries == 0 {
		backoff = DefaultBackoffPolicy
	}

	return &BinanceExchange{
		apiKey:       cfg.APIKey,
		apiSecret:    cfg.APISecret,
		baseURL:      strings.TrimRight(baseURL, "/"),
		client:       client,
		limiter:      NewRateLimiter(cfg.RateLimitPerMin),
		backoff:      backoff,
		recvWindowMS: cfg.RecvWindowMS,
		now:          time.Now,
	}
}

// Name implements Exchange.
func (b *BinanceExchange) Name() string { return "binance" }

// --- signing / low-level request plumbing ---

func (b *BinanceExchange) sign(params url.Values) {
	// Binance requires the signature to be computed over the exact query
	// string that will be sent (order matters only insofar as it must
	// match byte-for-byte); url.Values.Encode() sorts keys, which is
	// stable and reproducible, so signing its output is safe.
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(params.Encode()))
	params.Set("signature", hex.EncodeToString(mac.Sum(nil)))
}

func (b *BinanceExchange) prepareParams(params url.Values, signed bool) url.Values {
	if params == nil {
		params = url.Values{}
	}
	if signed {
		params.Set("timestamp", strconv.FormatInt(b.now().UTC().UnixMilli(), 10))
		if b.recvWindowMS > 0 {
			params.Set("recvWindow", strconv.FormatInt(b.recvWindowMS, 10))
		}
		b.sign(params)
	}
	return params
}

// binanceAPIError is Binance's standard {"code":..,"msg":".."} error body.
type binanceAPIError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (e *binanceAPIError) Error() string {
	return fmt.Sprintf("binance: %s (code %d)", e.Msg, e.Code)
}

// binanceOrderDoesNotExist is Binance's error code for "order does not
// exist" on GET/DELETE /api/v3/order.
const binanceOrderDoesNotExist = -2013

// binanceDuplicateOrder is Binance's documented error code for a
// newClientOrderId that has already been used ("Duplicate order sent.").
// isDuplicateClientOrderIDError also matches on message text as a
// belt-and-suspenders check, since exact codes have drifted across
// exchange API versions in the past and this decision (retry-as-success vs.
// hard failure) is exactly the İ5 idempotency guarantee — worth being
// permissive about detecting.
const binanceDuplicateOrder = -2010

func isDuplicateClientOrderIDError(apiErr *binanceAPIError) bool {
	if apiErr == nil {
		return false
	}
	if apiErr.Code == binanceDuplicateOrder && strings.Contains(strings.ToLower(apiErr.Msg), "duplicate") {
		return true
	}
	return strings.Contains(strings.ToLower(apiErr.Msg), "duplicate")
}

// httpResult is the outcome of a single (non-retried) HTTP attempt.
type httpResult struct {
	status int
	body   []byte
	apiErr *binanceAPIError // non-nil if body parsed as a Binance error
	netErr error            // non-nil on transport-level failure (no response at all)
}

// retryable reports whether this attempt's outcome is safe to retry
// automatically for read-only/idempotent calls (429/418 rate limiting,
// 5xx server errors, or a transport-level error/timeout). It does NOT by
// itself decide whether CreateOrder may retry — that additionally requires
// the FetchOrder verification step (see CreateOrder below).
func (r httpResult) retryable() bool {
	if r.netErr != nil {
		return true
	}
	if r.status == http.StatusTooManyRequests /* 429 */ || r.status == 418 {
		return true
	}
	if r.status >= 500 {
		return true
	}
	return false
}

func (r httpResult) asError() error {
	if r.netErr != nil {
		return fmt.Errorf("exchange: request failed: %w", r.netErr)
	}
	if r.apiErr != nil {
		return r.apiErr
	}
	if r.status < 200 || r.status >= 300 {
		return fmt.Errorf("exchange: unexpected HTTP status %d: %s", r.status, string(r.body))
	}
	return nil
}

// doRequestOnce performs exactly one HTTP attempt: rate-limit-wait, build
// request, execute, read body. It does not retry and does not sleep on
// 429/418 — callers decide retry policy (doRequestWithRetry for safe
// idempotent calls, CreateOrder's own loop for order creation).
func (b *BinanceExchange) doRequestOnce(ctx context.Context, method, path string, params url.Values, signed bool) httpResult {
	if err := b.limiter.Wait(ctx); err != nil {
		return httpResult{netErr: err}
	}

	params = b.prepareParams(params, signed)

	var req *http.Request
	var err error
	switch method {
	case http.MethodGet, http.MethodDelete:
		u := b.baseURL + path
		if q := params.Encode(); q != "" {
			u += "?" + q
		}
		req, err = http.NewRequestWithContext(ctx, method, u, nil)
	default: // POST, PUT
		req, err = http.NewRequestWithContext(ctx, method, b.baseURL+path, bytes.NewBufferString(params.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return httpResult{netErr: err}
	}
	if signed || b.apiKey != "" {
		req.Header.Set("X-MBX-APIKEY", b.apiKey)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return httpResult{netErr: err}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// We got a status code but couldn't read the body: treat like a
		// network error since we cannot know what the exchange actually
		// said (relevant for CreateOrder's ambiguity handling).
		return httpResult{status: resp.StatusCode, netErr: readErr}
	}

	res := httpResult{status: resp.StatusCode, body: body}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr binanceAPIError
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Code != 0 {
			res.apiErr = &apiErr
		}
	}
	return res
}

// doRequestWithRetry wraps doRequestOnce with SPEC.md Bölüm 5.2's required
// "429/418 yanıtlarında üstel geri çekilme + jitter" behavior, plus retry on
// transient network errors and 5xx. It must only be used for calls that are
// safe to retry blindly (GET/DELETE reads and cancels) — never for
// CreateOrder, whose retry safety additionally requires the FetchOrder
// verification step.
func (b *BinanceExchange) doRequestWithRetry(ctx context.Context, method, path string, params url.Values, signed bool) httpResult {
	var last httpResult
	for attempt := 0; attempt <= b.backoff.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := b.backoff.sleep(ctx, attempt, b.rand); err != nil {
				return httpResult{netErr: err}
			}
		}
		last = b.doRequestOnce(ctx, method, path, cloneParams(params), signed)
		if !last.retryable() {
			return last
		}
	}
	return last
}

func cloneParams(params url.Values) url.Values {
	if params == nil {
		return nil
	}
	out := make(url.Values, len(params))
	for k, v := range params {
		vv := make([]string, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}

// --- Exchange implementation ---

// FetchMarkets implements Exchange.
func (b *BinanceExchange) FetchMarkets(ctx context.Context) ([]domain.Market, error) {
	res := b.doRequestWithRetry(ctx, http.MethodGet, "/api/v3/exchangeInfo", nil, false)
	if err := res.asError(); err != nil {
		return nil, fmt.Errorf("exchange: FetchMarkets: %w", err)
	}

	var payload struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Status     string `json:"status"`
			Filters    []struct {
				FilterType     string `json:"filterType"`
				TickSize       string `json:"tickSize"`
				StepSize       string `json:"stepSize"`
				MinNotional    string `json:"minNotional"`
				MinNotionalNew string `json:"notional"` // NOTIONAL filter's field name in newer API versions
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(res.body, &payload); err != nil {
		return nil, fmt.Errorf("exchange: FetchMarkets: decode: %w", err)
	}

	markets := make([]domain.Market, 0, len(payload.Symbols))
	for _, s := range payload.Symbols {
		m := domain.Market{
			Symbol: s.BaseAsset + "/" + s.QuoteAsset,
			Base:   s.BaseAsset,
			Quote:  s.QuoteAsset,
			Active: s.Status == "TRADING",
			// ListedAt/DelistedAt are intentionally left zero:
			// exchangeInfo carries no listing-date field. SPEC.md
			// Bölüm 6.1 assigns tracking delistings (active flip +
			// delisted_at) to datafeed, by diffing this snapshot
			// against the markets table over time — this package
			// only reports what the exchange says *right now*.
		}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				m.TickSize = parseDecimalOrZero(f.TickSize)
			case "LOT_SIZE":
				m.StepSize = parseDecimalOrZero(f.StepSize)
			case "MIN_NOTIONAL":
				m.MinNotional = parseDecimalOrZero(f.MinNotional)
			case "NOTIONAL":
				if m.MinNotional.IsZero() {
					m.MinNotional = parseDecimalOrZero(f.MinNotionalNew)
				}
			}
		}
		markets = append(markets, m)
	}
	return markets, nil
}

func parseDecimalOrZero(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// FetchOHLCV implements Exchange. It does NOT filter a still-open trailing
// candle — see the Exchange interface doc comment and SPEC.md Bölüm 4.2/İ2.
func (b *BinanceExchange) FetchOHLCV(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]domain.Candle, error) {
	params := url.Values{}
	params.Set("symbol", toBinanceSymbol(symbol))
	params.Set("interval", timeframe) // Binance interval strings ("1d","4h",...) match SPEC's timeframe strings directly.
	if !since.IsZero() {
		params.Set("startTime", strconv.FormatInt(since.UTC().UnixMilli(), 10))
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}

	res := b.doRequestWithRetry(ctx, http.MethodGet, "/api/v3/klines", params, false)
	if err := res.asError(); err != nil {
		return nil, fmt.Errorf("exchange: FetchOHLCV %s %s: %w", symbol, timeframe, err)
	}

	var raw [][]json.RawMessage
	if err := json.Unmarshal(res.body, &raw); err != nil {
		return nil, fmt.Errorf("exchange: FetchOHLCV %s %s: decode: %w", symbol, timeframe, err)
	}

	candles := make([]domain.Candle, 0, len(raw))
	for _, k := range raw {
		c, err := parseKline(k)
		if err != nil {
			return nil, fmt.Errorf("exchange: FetchOHLCV %s %s: %w", symbol, timeframe, err)
		}
		candles = append(candles, c)
	}
	return candles, nil
}

// parseKline decodes a single Binance kline array:
// [openTime, open, high, low, close, volume, closeTime, quoteAssetVolume,
//
//	numTrades, takerBuyBaseVol, takerBuyQuoteVol, ignore]
func parseKline(k []json.RawMessage) (domain.Candle, error) {
	if len(k) < 8 {
		return domain.Candle{}, fmt.Errorf("kline: expected >= 8 fields, got %d", len(k))
	}
	var openTimeMs int64
	if err := json.Unmarshal(k[0], &openTimeMs); err != nil {
		return domain.Candle{}, fmt.Errorf("kline: openTime: %w", err)
	}
	open, err := unmarshalFloatString(k[1])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("kline: open: %w", err)
	}
	high, err := unmarshalFloatString(k[2])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("kline: high: %w", err)
	}
	low, err := unmarshalFloatString(k[3])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("kline: low: %w", err)
	}
	closePrice, err := unmarshalFloatString(k[4])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("kline: close: %w", err)
	}
	volume, err := unmarshalFloatString(k[5])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("kline: volume: %w", err)
	}
	quoteVolume, err := unmarshalFloatString(k[7])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("kline: quoteVolume: %w", err)
	}
	return domain.Candle{
		OpenTime:    time.UnixMilli(openTimeMs).UTC(),
		Open:        open,
		High:        high,
		Low:         low,
		Close:       closePrice,
		Volume:      volume,
		QuoteVolume: quoteVolume,
	}, nil
}

func unmarshalFloatString(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strconv.ParseFloat(s, 64)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, err
	}
	return f, nil
}

// FetchBalance implements Exchange. Reported per-asset value is
// free+locked (total holdings) — not just the spendable "free" amount —
// since callers (e.g. risk sizing, portfolio equity) need total exposure,
// not just what is currently unencumbered by open orders. Zero balances are
// omitted.
func (b *BinanceExchange) FetchBalance(ctx context.Context) (map[string]decimal.Decimal, error) {
	res := b.doRequestWithRetry(ctx, http.MethodGet, "/api/v3/account", url.Values{}, true)
	if err := res.asError(); err != nil {
		return nil, fmt.Errorf("exchange: FetchBalance: %w", err)
	}

	var payload struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(res.body, &payload); err != nil {
		return nil, fmt.Errorf("exchange: FetchBalance: decode: %w", err)
	}

	out := make(map[string]decimal.Decimal, len(payload.Balances))
	for _, bal := range payload.Balances {
		free := parseDecimalOrZero(bal.Free)
		locked := parseDecimalOrZero(bal.Locked)
		total := free.Add(locked)
		if total.IsZero() {
			continue
		}
		out[bal.Asset] = total
	}
	return out, nil
}

// CreateOrder implements Exchange. See the Exchange interface doc comment
// and SPEC.md Bölüm 5.2/6.6.2: this is the one call in this package that
// must NEVER blindly retry. On an ambiguous failure it verifies via
// FetchOrder(ctx, req.Symbol, req.ClientOrderID) that the order was not
// actually created before sending it again.
func (b *BinanceExchange) CreateOrder(ctx context.Context, req domain.OrderRequest) (OrderResult, error) {
	if req.ClientOrderID == "" {
		return OrderResult{}, errors.New("exchange: CreateOrder: OrderRequest.ClientOrderID is required (İ5)")
	}

	params, err := buildCreateOrderParams(req)
	if err != nil {
		return OrderResult{}, fmt.Errorf("exchange: CreateOrder: %w", err)
	}

	maxAttempts := b.backoff.MaxRetries + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// Ambiguous previous attempt: verify before resending
			// (İ5). This is the mandatory check — see interface doc.
			existing, ferr := b.FetchOrder(ctx, req.Symbol, req.ClientOrderID)
			if ferr == nil {
				return existing, nil // already created: return it, do not resend.
			}
			if !errors.Is(ferr, ErrOrderNotFound) {
				// Could not determine whether the order exists.
				// Do not guess — surface the uncertainty instead
				// of risking a duplicate.
				return OrderResult{}, fmt.Errorf("exchange: CreateOrder: cannot verify prior attempt for client_order_id=%s before retrying: %w", req.ClientOrderID, ferr)
			}
			if err := b.backoff.sleep(ctx, attempt-1, b.rand); err != nil {
				return OrderResult{}, err
			}
		}

		res := b.doRequestOnce(ctx, http.MethodPost, "/api/v3/order", cloneParams(params), true)
		if err := res.asError(); err == nil {
			return parseOrderResponse(res.body)
		} else if isDuplicateClientOrderIDError(res.apiErr) {
			// The exchange itself is telling us req.ClientOrderID was
			// already used (e.g. a caller-level retry after this
			// process crashed/restarted between submitting and
			// observing the response — SPEC.md Bölüm 6.6.2's restart
			// scenario). That is exactly İ5's idempotency guarantee
			// doing its job: resolve to the order that already exists
			// instead of erroring or creating a second position.
			existing, ferr := b.FetchOrder(ctx, req.Symbol, req.ClientOrderID)
			if ferr != nil {
				return OrderResult{}, fmt.Errorf("exchange: CreateOrder: client_order_id=%s rejected as duplicate but FetchOrder could not confirm it: %w", req.ClientOrderID, ferr)
			}
			return existing, nil
		} else if !res.retryable() {
			return OrderResult{}, fmt.Errorf("exchange: CreateOrder rejected (client_order_id=%s): %w", req.ClientOrderID, err)
		} else {
			lastErr = err
		}
	}
	return OrderResult{}, fmt.Errorf("exchange: CreateOrder: giving up after %d attempts (client_order_id=%s): %w", maxAttempts, req.ClientOrderID, lastErr)
}

func buildCreateOrderParams(req domain.OrderRequest) (url.Values, error) {
	if req.Symbol == "" {
		return nil, errors.New("Symbol is required")
	}
	if req.Qty.IsZero() || req.Qty.IsNegative() {
		return nil, fmt.Errorf("Qty must be positive, got %s", req.Qty)
	}

	params := url.Values{}
	params.Set("symbol", toBinanceSymbol(req.Symbol))
	params.Set("newClientOrderId", req.ClientOrderID)
	params.Set("newOrderRespType", "FULL")

	switch req.Side {
	case domain.SideBuy:
		params.Set("side", "BUY")
	case domain.SideSell:
		params.Set("side", "SELL")
	default:
		return nil, fmt.Errorf("unknown Side %q", req.Side)
	}

	switch strings.ToLower(req.Type) {
	case "market":
		params.Set("type", "MARKET")
		params.Set("quantity", req.Qty.String())
	case "limit":
		params.Set("type", "LIMIT")
		params.Set("timeInForce", "GTC")
		params.Set("quantity", req.Qty.String())
		if req.Price.IsZero() {
			return nil, errors.New("Price is required for limit orders")
		}
		params.Set("price", req.Price.String())
	default:
		return nil, fmt.Errorf("unknown order Type %q (expected \"market\" or \"limit\")", req.Type)
	}
	return params, nil
}

// FetchOrder implements Exchange. id may be either Binance's numeric
// orderId or a ClientOrderID; both are accepted so CreateOrder's
// idempotency check can call this with a ClientOrderID.
func (b *BinanceExchange) FetchOrder(ctx context.Context, symbol, id string) (OrderResult, error) {
	params := url.Values{}
	params.Set("symbol", toBinanceSymbol(symbol))
	if orderID, err := strconv.ParseInt(id, 10, 64); err == nil {
		params.Set("orderId", strconv.FormatInt(orderID, 10))
	} else {
		params.Set("origClientOrderId", id)
	}

	res := b.doRequestWithRetry(ctx, http.MethodGet, "/api/v3/order", params, true)
	if res.apiErr != nil && res.apiErr.Code == binanceOrderDoesNotExist {
		return OrderResult{}, fmt.Errorf("%w: symbol=%s id=%s: %s", ErrOrderNotFound, symbol, id, res.apiErr.Msg)
	}
	if err := res.asError(); err != nil {
		return OrderResult{}, fmt.Errorf("exchange: FetchOrder %s %s: %w", symbol, id, err)
	}
	return parseOrderResponse(res.body)
}

// CancelOrder implements Exchange. Safe to retry freely (no double-position
// risk from repeating a cancel), so it uses doRequestWithRetry.
func (b *BinanceExchange) CancelOrder(ctx context.Context, symbol, id string) error {
	params := url.Values{}
	params.Set("symbol", toBinanceSymbol(symbol))
	if orderID, err := strconv.ParseInt(id, 10, 64); err == nil {
		params.Set("orderId", strconv.FormatInt(orderID, 10))
	} else {
		params.Set("origClientOrderId", id)
	}

	res := b.doRequestWithRetry(ctx, http.MethodDelete, "/api/v3/order", params, true)
	if err := res.asError(); err != nil {
		return fmt.Errorf("exchange: CancelOrder %s %s: %w", symbol, id, err)
	}
	return nil
}

// --- response parsing ---

// binanceOrderResponse matches Binance's order object (returned by POST/GET
// /api/v3/order with newOrderRespType=FULL) closely enough for our fields.
type binanceOrderResponse struct {
	Symbol              string `json:"symbol"`
	OrderID             int64  `json:"orderId"`
	ClientOrderID       string `json:"clientOrderId"`
	Price               string `json:"price"`
	OrigQty             string `json:"origQty"`
	ExecutedQty         string `json:"executedQty"`
	CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
	Status              string `json:"status"`
	Type                string `json:"type"`
	Side                string `json:"side"`
	TransactTime        int64  `json:"transactTime"`
	Time                int64  `json:"time"`
	UpdateTime          int64  `json:"updateTime"`
	Fills               []struct {
		Price           string `json:"price"`
		Qty             string `json:"qty"`
		Commission      string `json:"commission"`
		CommissionAsset string `json:"commissionAsset"`
	} `json:"fills"`
}

func parseOrderResponse(body []byte) (OrderResult, error) {
	var o binanceOrderResponse
	if err := json.Unmarshal(body, &o); err != nil {
		return OrderResult{}, fmt.Errorf("decode order response: %w", err)
	}

	qty := parseDecimalOrZero(o.OrigQty)
	filled := parseDecimalOrZero(o.ExecutedQty)
	price := parseDecimalOrZero(o.Price)

	avgPrice := decimal.Zero
	fee := decimal.Zero
	if len(o.Fills) > 0 {
		notional := decimal.Zero
		filledFromFills := decimal.Zero
		for _, f := range o.Fills {
			fp := parseDecimalOrZero(f.Price)
			fq := parseDecimalOrZero(f.Qty)
			notional = notional.Add(fp.Mul(fq))
			filledFromFills = filledFromFills.Add(fq)
			// Commission may be charged in a different asset (e.g. BNB)
			// per fill; we sum the numeric amounts regardless of asset,
			// which is a simplification callers should be aware of —
			// the raw response (kept in OrderResult.Raw) has the
			// per-fill commissionAsset breakdown for anyone who needs
			// exact per-asset fee accounting.
			fee = fee.Add(parseDecimalOrZero(f.Commission))
		}
		if !filledFromFills.IsZero() {
			avgPrice = notional.Div(filledFromFills)
		}
	} else if !filled.IsZero() && !price.IsZero() {
		avgPrice = price
	}

	var side domain.Side
	switch o.Side {
	case "BUY":
		side = domain.SideBuy
	case "SELL":
		side = domain.SideSell
	}

	createdMs := o.TransactTime
	if createdMs == 0 {
		createdMs = o.Time
	}
	var createdAt time.Time
	if createdMs > 0 {
		createdAt = time.UnixMilli(createdMs).UTC()
	}

	order := domain.Order{
		ID:            strconv.FormatInt(o.OrderID, 10),
		ClientOrderID: o.ClientOrderID,
		Symbol:        fromBinanceSymbol(o.Symbol),
		Side:          side,
		Type:          strings.ToLower(o.Type),
		Qty:           qty,
		Price:         price,
		FilledQty:     filled,
		AvgPrice:      avgPrice,
		Fee:           fee,
		Status:        o.Status,
		CreatedAt:     createdAt,
	}
	return OrderResult{Order: order, Raw: json.RawMessage(body)}, nil
}

// --- symbol conversion: domain uses "BTC/USDT", Binance uses "BTCUSDT" ---

func toBinanceSymbol(symbol string) string {
	return strings.ReplaceAll(symbol, "/", "")
}

// fromBinanceSymbol reconstructs "BASE/QUOTE" from Binance's concatenated
// "BASEQUOTE" form. Binance's wire responses (order/klines) don't repeat
// the slash-form symbol, so this needs a quote-asset guess; we try the
// common quote assets first (longest match wins to avoid e.g. "ETHUSDT"
// mis-splitting on a base that happens to end in a quote-like substring).
var knownQuoteAssets = []string{"USDT", "USDC", "BUSD", "TUSD", "FDUSD", "BTC", "ETH", "BNB"}

func fromBinanceSymbol(symbol string) string {
	candidates := make([]string, 0, len(knownQuoteAssets))
	candidates = append(candidates, knownQuoteAssets...)
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	for _, q := range candidates {
		if strings.HasSuffix(symbol, q) && len(symbol) > len(q) {
			return symbol[:len(symbol)-len(q)] + "/" + q
		}
	}
	return symbol
}
