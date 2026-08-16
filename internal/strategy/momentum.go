package strategy

import (
	"fmt"
	"math"
	"sort"
	"time"

	"swingbot/internal/domain"
	"swingbot/internal/indicator"
)

// MomentumParams are the tunables for Momentum (SPEC.md Bölüm 6.4.1 /
// config.yaml strategy.momentum). Values are copied out of config by the
// caller (cmd/engine assembly code) — this package never reads config.yaml
// itself, it only knows about MomentumParams.
type MomentumParams struct {
	// TopN is how many top-ranked symbols to hold after a rebalance.
	TopN int
	// ExitRank: a held position exits once its rank falls outside the top
	// ExitRank (SPEC.md default 2*TopN = 10).
	ExitRank int
	// RebalanceWeekday: Evaluate only rebalances (produces enter/exit
	// signals) when AsOf falls on this weekday. SPEC.md default: Monday.
	RebalanceWeekday time.Weekday
	// ATRPeriod / ATRStopMult size the initial stop: entry - ATRStopMult *
	// ATR(ATRPeriod).
	ATRPeriod   int
	ATRStopMult float64
	// MomLong / MomShort are the two ROC lookbacks (SPEC.md: mom_90,
	// mom_30).
	MomLong  int
	MomShort int
	// VolLookback is the window for the (negatively weighted) return
	// volatility component (SPEC.md: vol_30).
	VolLookback int
	// LiqLookback is the window (in bars) over which the median quote
	// volume is computed before taking its log (SPEC.md: liq).
	LiqLookback int
	// Weights holds the cross-sectional z-score weights, keyed exactly as
	// in config.yaml: "mom_90", "mom_30", "vol_30", "liq". vol_30 is
	// SUBTRACTED (volatility is penalized) — see score formula in
	// SPEC.md Bölüm 6.2.
	Weights map[string]float64
}

// DefaultMomentumParams returns the SPEC.md Bölüm 8 config.yaml defaults
// for strategy.momentum.
func DefaultMomentumParams() MomentumParams {
	return MomentumParams{
		TopN:             5,
		ExitRank:         10,
		RebalanceWeekday: time.Monday,
		ATRPeriod:        14,
		ATRStopMult:      2.5,
		MomLong:          90,
		MomShort:         30,
		VolLookback:      30,
		LiqLookback:      30,
		Weights: map[string]float64{
			"mom_90": 0.4,
			"mom_30": 0.3,
			"vol_30": 0.2,
			"liq":    0.1,
		},
	}
}

// Momentum implements the cross-sectional momentum strategy of SPEC.md
// Bölüm 6.4.1: weekly rebalance into the top-N ranked symbols of Universe,
// exit once a held symbol's rank falls outside the top 2N.
type Momentum struct {
	params MomentumParams
}

// NewMomentum constructs a Momentum strategy with the given params. Zero
// params are NOT filled in with defaults — callers that want SPEC.md
// defaults should start from DefaultMomentumParams() and override.
func NewMomentum(params MomentumParams) *Momentum {
	return &Momentum{params: params}
}

func (m *Momentum) Name() string { return "momentum" }

func (m *Momentum) WarmupBars() int {
	// ROC(v, n) only becomes non-NaN once len(v) >= n+1, so the longest
	// lookback (mom_long, default 90) drives warmup. VolLookback rides on
	// top of daily returns (ROC(v,1)), so it needs VolLookback+1 closes
	// too; LiqLookback and ATRPeriod are shorter in the default config but
	// are included so a caller who reconfigures them larger still gets a
	// correct warmup requirement.
	need := m.params.MomLong + 1
	if v := m.params.MomShort + 1; v > need {
		need = v
	}
	if v := m.params.VolLookback + 1; v > need {
		need = v
	}
	if v := m.params.LiqLookback; v > need {
		need = v
	}
	if v := m.params.ATRPeriod; v > need {
		need = v
	}
	return need
}

func (m *Momentum) Params() map[string]any {
	return map[string]any{
		"top_n":             m.params.TopN,
		"exit_rank":         m.params.ExitRank,
		"rebalance_weekday": int(m.params.RebalanceWeekday),
		"atr_period":        m.params.ATRPeriod,
		"atr_stop_mult":     m.params.ATRStopMult,
		"mom_long":          m.params.MomLong,
		"mom_short":         m.params.MomShort,
		"vol_lookback":      m.params.VolLookback,
		"liq_lookback":      m.params.LiqLookback,
		"weights":           m.params.Weights,
	}
}

// momentumMetrics holds the raw (pre-z-score) values behind a symbol's
// score, plus the ATR needed for the stop and the last close used as the
// entry-price approximation.
type momentumMetrics struct {
	symbol string
	mom90  float64
	mom30  float64
	vol30  float64
	liq    float64
	atr    float64
	close  float64
}

func (m *Momentum) Evaluate(in Input) ([]domain.Signal, error) {
	// Rebalancing only happens on the configured weekday — ranks are only
	// meaningful relative to a full weekly recompute (SPEC.md 6.4.1:
	// "Yeniden dengeleme: haftada bir"). On any other day there is nothing
	// to do: momentum carries no daily trailing-stop update (unlike
	// trendfollow) and the stop set at entry is reported once.
	if in.AsOf.Weekday() != m.params.RebalanceWeekday {
		return nil, nil
	}

	p := m.params

	raw := make([]momentumMetrics, 0, len(in.Universe))
	for _, sym := range in.Universe {
		series, ok := in.Series[sym]
		if !ok || len(series) == 0 {
			continue
		}
		mm, ok := m.rawMetrics(sym, series)
		if !ok {
			continue // insufficient/invalid data: excluded from ranking entirely
		}
		raw = append(raw, mm)
	}

	// Sort by symbol first so the cross-sectional slices below (and thus
	// ZScore's output) are built in a fixed, deterministic order
	// regardless of map/Universe iteration order.
	sort.Slice(raw, func(i, j int) bool { return raw[i].symbol < raw[j].symbol })

	mom90s := make([]float64, len(raw))
	mom30s := make([]float64, len(raw))
	vol30s := make([]float64, len(raw))
	liqs := make([]float64, len(raw))
	for i, mm := range raw {
		mom90s[i] = mm.mom90
		mom30s[i] = mm.mom30
		vol30s[i] = mm.vol30
		liqs[i] = mm.liq
	}

	zMom90 := zOrZero(indicator.ZScore(mom90s))
	zMom30 := zOrZero(indicator.ZScore(mom30s))
	zVol30 := zOrZero(indicator.ZScore(vol30s))
	zLiq := zOrZero(indicator.ZScore(liqs))

	w := p.Weights
	type scored struct {
		mm    momentumMetrics
		score float64
		zM90  float64
		zM30  float64
		zVol  float64
		zLiq  float64
	}
	scoredList := make([]scored, len(raw))
	for i, mm := range raw {
		sc := w["mom_90"]*zMom90[i] + w["mom_30"]*zMom30[i] - w["vol_30"]*zVol30[i] + w["liq"]*zLiq[i]
		scoredList[i] = scored{mm: mm, score: sc, zM90: zMom90[i], zM30: zMom30[i], zVol: zVol30[i], zLiq: zLiq[i]}
	}

	// Rank descending by score; ties broken by symbol ascending so the
	// ordering (and therefore every signal produced from it) is fully
	// deterministic.
	sort.Slice(scoredList, func(i, j int) bool {
		if scoredList[i].score != scoredList[j].score {
			return scoredList[i].score > scoredList[j].score
		}
		return scoredList[i].mm.symbol < scoredList[j].mm.symbol
	})

	rankOf := make(map[string]int, len(scoredList)) // 1-based
	for i, s := range scoredList {
		rankOf[s.mm.symbol] = i + 1
	}
	universeSize := float64(len(scoredList))

	var signals []domain.Signal

	// Entries: top N, skipping symbols already held by ANY strategy (risk
	// gate also enforces "already_open", but the strategy has no business
	// proposing a second entry into a position it already sees).
	for i, s := range scoredList {
		rank := i + 1
		if rank > p.TopN {
			break
		}
		if _, held := in.Portfolio.Positions[s.mm.symbol]; held {
			continue
		}
		stop := s.mm.close - p.ATRStopMult*s.mm.atr
		signals = append(signals, domain.Signal{
			AsOf:      in.AsOf,
			Symbol:    s.mm.symbol,
			Kind:      domain.SignalEnter,
			Score:     s.score,
			RefPrice:  s.mm.close,
			StopPrice: stop,
			Reason: fmt.Sprintf(
				"Momentum sıralaması %d/%d (skor %.3f), top %d içinde: giriş.",
				rank, int(universeSize), s.score, p.TopN,
			),
			Metrics: map[string]float64{
				"rank":          float64(rank),
				"universe":      universeSize,
				"score":         s.score,
				"mom_90":        s.mm.mom90,
				"mom_30":        s.mm.mom30,
				"vol_30":        s.mm.vol30,
				"liq":           s.mm.liq,
				"z_mom_90":      s.zM90,
				"z_mom_30":      s.zM30,
				"z_vol_30":      s.zVol,
				"z_liq":         s.zLiq,
				"atr_14":        s.mm.atr,
				"atr_stop_mult": p.ATRStopMult,
				"top_n":         float64(p.TopN),
			},
		})
	}

	// Exits: any position opened by this strategy whose rank has fallen
	// outside ExitRank (or that dropped out of the ranking entirely —
	// delisted, or data became invalid) is closed out.
	for sym, pos := range in.Portfolio.Positions {
		if pos.Strategy != m.Name() {
			continue
		}
		rank, ok := rankOf[sym]
		if !ok {
			signals = append(signals, domain.Signal{
				AsOf:   in.AsOf,
				Symbol: sym,
				Kind:   domain.SignalExit,
				Reason: "Sembol bu haftaki evrende/sıralamada yok (veri eksik veya evrenden çıktı): çıkış.",
				Metrics: map[string]float64{
					"exit_rank": float64(p.ExitRank),
				},
			})
			continue
		}
		if rank > p.ExitRank {
			signals = append(signals, domain.Signal{
				AsOf:   in.AsOf,
				Symbol: sym,
				Kind:   domain.SignalExit,
				Reason: fmt.Sprintf(
					"Momentum sıralaması %d/%d, ilk %d dışına düştü: çıkış.",
					rank, int(universeSize), p.ExitRank,
				),
				Metrics: map[string]float64{
					"rank":      float64(rank),
					"universe":  universeSize,
					"exit_rank": float64(p.ExitRank),
				},
			})
		}
	}

	// Deterministic output order: sort by symbol so callers/tests never
	// depend on Go's map iteration order.
	sort.Slice(signals, func(i, j int) bool { return signals[i].Symbol < signals[j].Symbol })

	return signals, nil
}

// rawMetrics computes the pre-z-score momentum inputs for one symbol's
// series, using only series[:len(series)] (never past the AS-OF candle).
// ok is false if any component is NaN (insufficient history or a
// degenerate liquidity window) — such symbols are excluded from the
// ranking entirely rather than silently scored as 0.
func (m *Momentum) rawMetrics(sym string, series []domain.Candle) (momentumMetrics, bool) {
	p := m.params
	n := len(series)
	closes := make([]float64, n)
	quoteVols := make([]float64, n)
	for i, c := range series {
		closes[i] = c.Close
		quoteVols[i] = c.QuoteVolume
	}

	mom90 := indicator.ROC(closes, p.MomLong)[n-1]
	mom30 := indicator.ROC(closes, p.MomShort)[n-1]
	returns := indicator.ROC(closes, 1)
	vol30 := indicator.StdDev(returns, p.VolLookback)[n-1]
	atr := indicator.ATR(series, p.ATRPeriod)[n-1]
	liq := medianLog(quoteVols, p.LiqLookback)

	if anyNaN(mom90, mom30, vol30, atr, liq) {
		return momentumMetrics{}, false
	}
	return momentumMetrics{
		symbol: sym,
		mom90:  mom90,
		mom30:  mom30,
		vol30:  vol30,
		liq:    liq,
		atr:    atr,
		close:  closes[n-1],
	}, true
}

// medianLog returns log(median(v[len(v)-lookback:])). NaN if v is shorter
// than lookback or the median is not strictly positive (log undefined).
func medianLog(v []float64, lookback int) float64 {
	if lookback <= 0 || len(v) < lookback {
		return math.NaN()
	}
	window := append([]float64(nil), v[len(v)-lookback:]...)
	sort.Float64s(window)
	var med float64
	mid := len(window) / 2
	if len(window)%2 == 0 {
		med = (window[mid-1] + window[mid]) / 2
	} else {
		med = window[mid]
	}
	if med <= 0 {
		return math.NaN()
	}
	return math.Log(med)
}

func anyNaN(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) {
			return true
		}
	}
	return false
}

// zOrZero replaces indicator.ZScore's NaN output (population stddev == 0:
// a single symbol, or every symbol tied on this metric) with 0 — a neutral
// score contribution — so a small or degenerate universe can still be
// ranked instead of producing an all-NaN score for everyone.
func zOrZero(z []float64) []float64 {
	out := make([]float64, len(z))
	for i, v := range z {
		if math.IsNaN(v) {
			out[i] = 0
			continue
		}
		out[i] = v
	}
	return out
}
