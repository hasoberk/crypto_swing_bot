// api.go implements SPEC.md Bölüm 7.1's JSON API: eight GET-only endpoints,
// every one of them a thin read over internal/store's existing typed
// Get*/List* methods (or, for /api/universe, internal/universe.Build —
// the exact function `swingbot universe show` already calls, see
// cmd/swingbot/universe.go, so the panel can never disagree with the CLI
// about what today's universe is). No handler in this file constructs SQL
// or mutates state.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/datafeed"
	"swingbot/internal/risk"
	"swingbot/internal/store"
	"swingbot/internal/universe"
)

func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/equity", s.handleEquity)
	mux.HandleFunc("GET /api/positions", s.handlePositions)
	mux.HandleFunc("GET /api/proposals", s.handleProposals)
	mux.HandleFunc("GET /api/trades", s.handleTrades)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRunByID)
	mux.HandleFunc("GET /api/universe", s.handleUniverse)
}

// --- response plumbing ------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// rawJSONOrNull turns a store column that is documented to hold a JSON
// string (proposals.metrics_json, runs.params_json/costs_json/metrics_json)
// into a json.RawMessage that embeds verbatim in a response — cheaper and
// less error-prone than unmarshalling into map[string]any and
// re-marshalling, and it can never subtly reformat a number (e.g. lose
// precision) on the way through. An empty column (should not occur per the
// NOT NULL schema, but defensively handled) becomes JSON null rather than
// an invalid empty-string RawMessage.
func rawJSONOrNull(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

// decimalToFloat parses a store decimal string (qty columns are kept as
// strings at the storage boundary — see store.Trade's doc comment) into a
// float64 for display/PnL arithmetic. An unparseable or empty string
// yields 0 rather than an error: a malformed qty should never take the
// whole positions view down, just that one row's PnL figure.
func decimalToFloat(s string) (float64, bool) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return 0, false
	}
	f, _ := d.Float64()
	return f, true
}

func queryOr(r *http.Request, key, fallback string) string {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return fallback
	}
	return v
}

// defaultMode is the mode (SPEC.md Bölüm 4.1: "backtest"|"paper"|"live")
// panel endpoints assume when a request omits ?mode= — the mode this
// swingbot instance is actually configured to run, falling back to
// "paper" only if config left it blank (should not happen post-Validate,
// but a panel read must never panic on it).
func (s *Server) defaultMode() string {
	if s.cfg.Mode == "" {
		return "paper"
	}
	return s.cfg.Mode
}

// --- /api/health --------------------------------------------------------

// breakerStateKey mirrors cmd/swingbot/main.go's own constant of the same
// name: the system_state key SPEC.md Bölüm 6.5.3 documents for the
// circuit breaker ("system_state['breaker'] = 'open' + gerekçe + zaman
// damgası"). Kept as a second literal (not imported from cmd/swingbot,
// which cannot be imported — it is package main) rather than introducing a
// shared constants package for one string; `swingbot breaker status`
// and this handler both read the same key by convention, not by shared
// code, and store.SetState is the only writer either needs to agree with.
const breakerStateKey = "breaker"

// handleHealth also surfaces circuit-breaker state: SPEC.md Bölüm 7.1's
// overview page needs "devre kesici durumu" and this is the cheapest
// correct place for a single system_state row to live in the eight-endpoint
// API SPEC.md Bölüm 7.1 lists (no endpoint of its own is specified for it).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbOK := true
	if err := s.st.DB().PingContext(ctx); err != nil {
		dbOK = false
	}

	breaker := map[string]any{"open": false}
	if raw, ok, err := s.st.GetState(ctx, breakerStateKey); err == nil && ok {
		var state risk.State
		if json.Unmarshal([]byte(raw), &state) == nil && state.Open {
			breaker = map[string]any{
				"open":   true,
				"reason": state.Reason,
				"detail": state.Detail,
				"at":     state.At,
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"time":          time.Now().UTC(),
		"mode":          s.cfg.Mode,
		"dbOk":          dbOK,
		"uptimeSeconds": time.Since(s.startedAt).Seconds(),
		"version":       s.version,
		"breaker":       breaker,
	})
}

// --- /api/equity ----------------------------------------------------------

// equityPointView is one equity_snapshots row (store.EquitySnapshot),
// reshaped for JSON. BenchBTC is never omitted client-side by this
// package's choice: SPEC.md Bölüm 7.2's "İmza öğesi" requires the BTC
// buy-and-hold benchmark to always be plottable alongside strategy equity,
// so app.js can rely on the field being present (null, not absent, when
// no benchmark has been recorded yet).
type equityPointView struct {
	TS         time.Time `json:"ts"`
	Equity     float64   `json:"equity"`
	Cash       float64   `json:"cash"`
	Exposure   float64   `json:"exposure"`
	BenchBTC   *float64  `json:"benchBtc"`
	BenchTop10 *float64  `json:"benchTop10"`
}

func (s *Server) handleEquity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mode := queryOr(r, "mode", s.defaultMode())

	snaps, err := s.st.ListEquitySnapshots(ctx, mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "equity okunamadı: "+err.Error())
		return
	}

	points := make([]equityPointView, 0, len(snaps))
	for _, e := range snaps {
		points = append(points, equityPointView{
			TS: e.TS, Equity: e.Equity, Cash: e.Cash, Exposure: e.Exposure,
			BenchBTC: e.BenchBTC, BenchTop10: e.BenchTop10,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "points": points})
}

// --- /api/positions -------------------------------------------------------

// positionView is one open trade (store.Trade with a zero ExitTime — SPEC.md
// Bölüm 4.1's trades table doubles as the open-positions ledger, there is
// no separate `positions` table) enriched with the most recent stored
// close price and, best-effort, the stop price of the FILLED proposal that
// most likely opened it. SPEC.md Bölüm 7.1's /positions page needs "stop
// mesafeleri, açık K/Z" — both derived here rather than stored, since
// nothing in this codebase yet persists a live trailing-stop value outside
// engine memory (internal/engine does not exist yet; see live-engine-
// notify-engineer's territory).
type positionView struct {
	Symbol           string    `json:"symbol"`
	Strategy         string    `json:"strategy"`
	Qty              string    `json:"qty"`
	EntryPrice       float64   `json:"entryPrice"`
	EntryTime        time.Time `json:"entryTime"`
	HoldDays         float64   `json:"holdDays"`
	LastPrice        *float64  `json:"lastPrice"`
	UnrealizedPnL    *float64  `json:"unrealizedPnl"`
	UnrealizedPnLPct *float64  `json:"unrealizedPnlPct"`
	StopPrice        *float64  `json:"stopPrice"`
	StopDistancePct  *float64  `json:"stopDistancePct"`
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mode := queryOr(r, "mode", s.defaultMode())

	trades, err := s.st.ListTrades(ctx, mode, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pozisyonlar okunamadı: "+err.Error())
		return
	}

	var open []store.Trade
	symbols := make([]string, 0)
	seen := map[string]bool{}
	for _, t := range trades {
		if !t.ExitTime.IsZero() {
			continue
		}
		open = append(open, t)
		if !seen[t.Symbol] {
			seen[t.Symbol] = true
			symbols = append(symbols, t.Symbol)
		}
	}

	lastPrice := s.lastClosePriceBySymbol(ctx, symbols)
	stopPrice := s.latestFilledStopBySymbol(ctx, seen)

	now := time.Now().UTC()
	out := make([]positionView, 0, len(open))
	for _, t := range open {
		pv := positionView{
			Symbol:     t.Symbol,
			Strategy:   t.Strategy,
			Qty:        t.Qty,
			EntryPrice: t.EntryPrice,
			EntryTime:  t.EntryTime,
			HoldDays:   now.Sub(t.EntryTime).Hours() / 24,
		}
		qty, _ := decimalToFloat(t.Qty)
		if last, ok := lastPrice[t.Symbol]; ok {
			l := last
			pv.LastPrice = &l
			pnl := qty * (last - t.EntryPrice)
			pv.UnrealizedPnL = &pnl
			if t.EntryPrice != 0 {
				pct := (last/t.EntryPrice - 1) * 100
				pv.UnrealizedPnLPct = &pct
			}
			if sp, ok := stopPrice[t.Symbol]; ok {
				spv := sp
				pv.StopPrice = &spv
				if last != 0 {
					dist := (last - sp) / last * 100
					pv.StopDistancePct = &dist
				}
			}
		}
		out = append(out, pv)
	}

	writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "positions": out})
}

// lastClosePriceBySymbol fetches each symbol's most recent stored candle
// close in a single round trip (store.GetCandlesForSymbols — see its doc
// comment on why this package batches instead of querying per-symbol: the
// store enforces one SQLite connection, so N sequential calls would just
// queue behind each other for no parallelism gain).
func (s *Server) lastClosePriceBySymbol(ctx context.Context, symbols []string) map[string]float64 {
	out := map[string]float64{}
	if len(symbols) == 0 {
		return out
	}
	// A 14-day lookback comfortably covers the daily timeframe SPEC.md
	// Bölüm 8 defaults to even across a weekend/holiday data gap; GetCandles
	// for other timeframes just returns a few extra rows, which is
	// harmless since only the last one is used.
	from := time.Now().UTC().AddDate(0, 0, -14)
	candles, err := s.st.GetCandlesForSymbols(ctx, symbols, s.cfg.Data.Timeframe, from, time.Time{})
	if err != nil {
		return out
	}
	for sym, cs := range candles {
		if len(cs) == 0 {
			continue
		}
		out[sym] = cs[len(cs)-1].Close
	}
	return out
}

// latestFilledStopBySymbol returns, for each symbol in want, the stop_price
// of the most recently created FILLED proposal for that symbol — a
// best-effort stand-in for a live trailing stop (see positionView's doc
// comment). Proposals with a nil StopPrice are skipped.
func (s *Server) latestFilledStopBySymbol(ctx context.Context, want map[string]bool) map[string]float64 {
	out := map[string]float64{}
	if len(want) == 0 {
		return out
	}
	filled, err := s.st.ListProposalsByStatus(ctx, store.ProposalFilled)
	if err != nil {
		return out
	}
	best := map[string]store.Proposal{}
	for _, p := range filled {
		if !want[p.Symbol] {
			continue
		}
		cur, ok := best[p.Symbol]
		if !ok || p.CreatedAt.After(cur.CreatedAt) {
			best[p.Symbol] = p
		}
	}
	for sym, p := range best {
		if p.StopPrice != nil {
			out[sym] = *p.StopPrice
		}
	}
	return out
}

// --- /api/proposals ---------------------------------------------------

// allProposalStatuses lists every store.ProposalStatus value (see
// store/proposals.go's state machine comment) in the order ListProposals
// merges them when no ?status= filter is given.
var allProposalStatuses = []store.ProposalStatus{
	store.ProposalPending, store.ProposalApproved, store.ProposalRejected,
	store.ProposalExpired, store.ProposalSubmitted, store.ProposalFilled, store.ProposalFailed,
}

func isKnownProposalStatus(st store.ProposalStatus) bool {
	for _, s := range allProposalStatuses {
		if s == st {
			return true
		}
	}
	return false
}

type proposalView struct {
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"createdAt"`
	AsOf       time.Time       `json:"asOf"`
	Symbol     string          `json:"symbol"`
	Side       string          `json:"side"`
	Strategy   string          `json:"strategy"`
	Score      *float64        `json:"score"`
	RefPrice   float64         `json:"refPrice"`
	StopPrice  *float64        `json:"stopPrice"`
	Qty        string          `json:"qty"`
	RiskAmount float64         `json:"riskAmount"`
	Reason     string          `json:"reason"`
	Metrics    json.RawMessage `json:"metrics"`
	Status     string          `json:"status"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	DecidedAt  *time.Time      `json:"decidedAt"`
	OrderID    string          `json:"orderId,omitempty"`
}

func toProposalView(p store.Proposal) proposalView {
	pv := proposalView{
		ID: p.ID, CreatedAt: p.CreatedAt, AsOf: p.AsOf, Symbol: p.Symbol, Side: p.Side,
		Strategy: p.Strategy, Score: p.Score, RefPrice: p.RefPrice, StopPrice: p.StopPrice,
		Qty: p.Qty, RiskAmount: p.RiskAmount, Reason: p.Reason,
		Metrics: rawJSONOrNull(p.MetricsJSON), Status: string(p.Status), ExpiresAt: p.ExpiresAt,
		OrderID: p.OrderID,
	}
	if !p.DecidedAt.IsZero() {
		d := p.DecidedAt
		pv.DecidedAt = &d
	}
	return pv
}

func (s *Server) handleProposals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	statusParam := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))

	var all []store.Proposal
	if statusParam != "" {
		st := store.ProposalStatus(statusParam)
		if !isKnownProposalStatus(st) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("bilinmeyen status: %q", statusParam))
			return
		}
		list, err := s.st.ListProposalsByStatus(ctx, st)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "öneriler okunamadı: "+err.Error())
			return
		}
		all = list
	} else {
		for _, st := range allProposalStatuses {
			list, err := s.st.ListProposalsByStatus(ctx, st)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "öneriler okunamadı: "+err.Error())
				return
			}
			all = append(all, list...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	}

	out := make([]proposalView, 0, len(all))
	for _, p := range all {
		out = append(out, toProposalView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": out})
}

// --- /api/trades ------------------------------------------------------

type tradeView struct {
	ID         string     `json:"id"`
	Symbol     string     `json:"symbol"`
	Strategy   string     `json:"strategy"`
	EntryTime  time.Time  `json:"entryTime"`
	EntryPrice float64    `json:"entryPrice"`
	ExitTime   *time.Time `json:"exitTime"`
	ExitPrice  *float64   `json:"exitPrice"`
	Qty        string     `json:"qty"`
	Fees       float64    `json:"fees"`
	PnLQuote   *float64   `json:"pnlQuote"`
	PnLPct     *float64   `json:"pnlPct"`
	ExitReason string     `json:"exitReason,omitempty"`
	Mode       string     `json:"mode"`
}

func toTradeView(t store.Trade) tradeView {
	tv := tradeView{
		ID: t.ID, Symbol: t.Symbol, Strategy: t.Strategy, EntryTime: t.EntryTime,
		EntryPrice: t.EntryPrice, ExitPrice: t.ExitPrice, Qty: t.Qty, Fees: t.Fees,
		PnLQuote: t.PnLQuote, PnLPct: t.PnLPct, ExitReason: t.ExitReason, Mode: t.Mode,
	}
	if !t.ExitTime.IsZero() {
		et := t.ExitTime
		tv.ExitTime = &et
	}
	return tv
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mode := queryOr(r, "mode", s.defaultMode())
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))

	trades, err := s.st.ListTrades(ctx, mode, symbol)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "işlemler okunamadı: "+err.Error())
		return
	}

	out := make([]tradeView, 0, len(trades))
	for _, t := range trades {
		out = append(out, toTradeView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "symbol": symbol, "trades": out})
}

// --- /api/runs, /api/runs/{id} -----------------------------------------

type runView struct {
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"createdAt"`
	Strategy   string          `json:"strategy"`
	Params     json.RawMessage `json:"params"`
	StartTS    time.Time       `json:"startTs"`
	EndTS      time.Time       `json:"endTs"`
	Costs      json.RawMessage `json:"costs"`
	Metrics    json.RawMessage `json:"metrics"`
	ReportPath string          `json:"reportPath,omitempty"`
	GitSHA     string          `json:"gitSha,omitempty"`
}

func toRunView(run store.Run) runView {
	return runView{
		ID: run.ID, CreatedAt: run.CreatedAt, Strategy: run.Strategy,
		Params: rawJSONOrNull(run.ParamsJSON), StartTS: run.StartTS, EndTS: run.EndTS,
		Costs: rawJSONOrNull(run.CostsJSON), Metrics: rawJSONOrNull(run.MetricsJSON),
		ReportPath: run.ReportPath, GitSHA: run.GitSHA,
	}
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runs, err := s.st.ListRuns(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "koşular okunamadı: "+err.Error())
		return
	}
	out := make([]runView, 0, len(runs))
	for _, run := range runs {
		out = append(out, toRunView(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *Server) handleRunByID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	run, ok, err := s.st.GetRun(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "koşu okunamadı: "+err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("koşu bulunamadı: %q", id))
		return
	}
	writeJSON(w, http.StatusOK, toRunView(run))
}

// --- /api/universe ------------------------------------------------------

type universeSymbolView struct {
	Rank                int             `json:"rank"`
	Symbol              string          `json:"symbol"`
	Score               float64         `json:"score"`
	MedianQuoteVolume30 float64         `json:"medianQuoteVolume30"`
	Components          json.RawMessage `json:"components"`
}

type universeExcludedView struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// handleUniverse rebuilds the universe live for ?date= (default: today
// UTC) using the exact same universe.Build call `swingbot universe show`
// uses (see cmd/swingbot/universe.go) — SPEC.md Bölüm 4.1 has no
// `universe` table to read back, and Build is cheap enough (it is already
// the CLI's own implementation) that recomputing on request keeps this
// endpoint from ever disagreeing with the CLI about "today's universe".
func (s *Server) handleUniverse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dateStr := strings.TrimSpace(r.URL.Query().Get("date"))
	asOf := time.Now().UTC()
	asOf = time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	if dateStr != "" {
		parsed, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "date geçersiz (YYYY-MM-DD bekleniyor): "+err.Error())
			return
		}
		asOf = parsed.UTC()
	}

	tfDur, err := datafeed.ParseTimeframe(s.cfg.Data.Timeframe)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config: data.timeframe geçersiz: "+err.Error())
		return
	}

	params := universe.FilterParams{
		Quote:                s.cfg.Exchange.Quote,
		MinMedianQuoteVolume: s.cfg.Universe.MinMedianQuoteVolume,
		MinListingAgeDays:    s.cfg.Universe.MinListingAgeDays,
		ExcludePatterns:      s.cfg.Universe.ExcludePatterns,
		ExcludeStablecoins:   s.cfg.Universe.ExcludeStablecoins,
		TimeframeDur:         tfDur,
		MaxSymbols:           s.cfg.Universe.MaxSymbols,
	}
	weights := universe.WeightsFromMap(s.cfg.Strategy.Momentum.Weights)

	result, err := universe.Build(ctx, s.st, s.cfg.Data.Timeframe, asOf, params, weights)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "evren hesaplanamadı: "+err.Error())
		return
	}

	included := make([]universeSymbolView, 0, len(result.Universe))
	for _, sc := range result.Universe {
		comp, _ := json.Marshal(sc.Components.ToMetrics())
		included = append(included, universeSymbolView{
			Rank: sc.Rank, Symbol: sc.Symbol, Score: sc.Score,
			MedianQuoteVolume30: sc.Components.MedianQuoteVolume30,
			Components:          comp,
		})
	}
	excluded := make([]universeExcludedView, 0, len(result.Excluded))
	for _, e := range result.Excluded {
		excluded = append(excluded, universeExcludedView{Symbol: e.Symbol, Reason: e.Reason, Detail: e.Detail})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"asOf":     asOf.Format("2006-01-02"),
		"included": included,
		"excluded": excluded,
	})
}
