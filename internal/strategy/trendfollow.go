package strategy

import (
	"fmt"
	"math"
	"sort"

	"swingbot/internal/domain"
	"swingbot/internal/indicator"
)

// TrendfollowParams are the tunables for Trendfollow (SPEC.md Bölüm 6.4.2 /
// config.yaml strategy.trendfollow).
type TrendfollowParams struct {
	// SMALong: long-term trend filter, close > SMA(SMALong) required to enter.
	SMALong int
	// SMAExit: close < SMA(SMAExit) is the discretionary (non-stop) exit.
	SMAExit int
	// BreakoutLookback: entry requires close == max(close[-BreakoutLookback:]).
	BreakoutLookback int
	// ATRPeriod / ATRStopMult size both the entry stop and every trailing
	// stop update: HighWater - ATRStopMult*ATR(ATRPeriod).
	ATRPeriod   int
	ATRStopMult float64
	// MaxATRPct: entries are rejected when ATR(ATRPeriod)/close >= MaxATRPct
	// (too volatile).
	MaxATRPct float64
}

// DefaultTrendfollowParams returns the SPEC.md Bölüm 8 config.yaml
// defaults for strategy.trendfollow.
func DefaultTrendfollowParams() TrendfollowParams {
	return TrendfollowParams{
		SMALong:          200,
		SMAExit:          50,
		BreakoutLookback: 20,
		ATRPeriod:        14,
		ATRStopMult:      2.5,
		MaxATRPct:        0.08,
	}
}

// Trendfollow implements the single-symbol trend-following strategy of
// SPEC.md Bölüm 6.4.2: enter on a 20-day breakout above a rising SMA(200)
// filter, manage an ATR trailing stop that only ever ratchets up, and exit
// discretionarily when the close falls back below SMA(50).
//
// Trendfollow never decides that low <= StopPrice triggers an exit — it
// only ever reports the current StopPrice via domain.SignalStop. See the
// package doc comment and SPEC.md Bölüm 6.4.2's closing "Not".
type Trendfollow struct {
	params TrendfollowParams
}

// NewTrendfollow constructs a Trendfollow strategy with the given params.
// Zero params are NOT filled in with defaults — callers that want SPEC.md
// defaults should start from DefaultTrendfollowParams() and override.
func NewTrendfollow(params TrendfollowParams) *Trendfollow {
	return &Trendfollow{params: params}
}

func (t *Trendfollow) Name() string { return "trendfollow" }

func (t *Trendfollow) WarmupBars() int {
	need := t.params.SMALong
	if v := t.params.SMAExit; v > need {
		need = v
	}
	if v := t.params.BreakoutLookback; v > need {
		need = v
	}
	if v := t.params.ATRPeriod; v > need {
		need = v
	}
	return need
}

func (t *Trendfollow) Params() map[string]any {
	return map[string]any{
		"sma_long":          t.params.SMALong,
		"sma_exit":          t.params.SMAExit,
		"breakout_lookback": t.params.BreakoutLookback,
		"atr_period":        t.params.ATRPeriod,
		"atr_stop_mult":     t.params.ATRStopMult,
		"max_atr_pct":       t.params.MaxATRPct,
	}
}

func (t *Trendfollow) Evaluate(in Input) ([]domain.Signal, error) {
	p := t.params
	var signals []domain.Signal

	// Position management: every symbol currently held BY THIS STRATEGY
	// gets its trailing stop re-evaluated and its discretionary exit
	// condition checked, every day — regardless of whether the symbol is
	// still part of today's Universe (a delisted/filtered-out symbol that
	// we are still holding must keep being managed until it is closed).
	for sym, pos := range in.Portfolio.Positions {
		if pos.Strategy != t.Name() {
			continue
		}
		series, ok := in.Series[sym]
		if !ok || len(series) == 0 {
			continue // no data to manage this symbol today; leave it untouched
		}
		n := len(series)
		last := series[n-1]

		atrs := indicator.ATR(series, p.ATRPeriod)
		atr := atrs[n-1]
		if !isFinite(atr) {
			continue // not enough data to size a stop; skip until it is
		}

		highWater := pos.HighWater
		if last.High > highWater {
			highWater = last.High
		}
		newStop := highWater - p.ATRStopMult*atr
		stop := pos.StopPrice
		if newStop > stop {
			stop = newStop
		}

		signals = append(signals, domain.Signal{
			AsOf:      in.AsOf,
			Symbol:    sym,
			Kind:      domain.SignalStop,
			RefPrice:  last.Close,
			StopPrice: stop,
			Reason: fmt.Sprintf(
				"Trailing stop güncellendi: highwater=%.8g, atr14=%.8g, yeni_stop=%.8g, eski_stop=%.8g, uygulanan=%.8g (stop yalnızca yukarı gider).",
				highWater, atr, newStop, pos.StopPrice, stop,
			),
			Metrics: map[string]float64{
				"high_water":    highWater,
				"atr_14":        atr,
				"atr_stop_mult": p.ATRStopMult,
				"new_stop":      newStop,
				"prev_stop":     pos.StopPrice,
				"applied_stop":  stop,
			},
		})

		sma50s := indicator.SMA(closesOf(series), p.SMAExit)
		sma50 := sma50s[n-1]
		if isFinite(sma50) && last.Close < sma50 {
			signals = append(signals, domain.Signal{
				AsOf:     in.AsOf,
				Symbol:   sym,
				Kind:     domain.SignalExit,
				RefPrice: last.Close,
				Reason: fmt.Sprintf(
					"Kapanış (%.8g) SMA(%d) (%.8g) altına düştü: çıkış.",
					last.Close, p.SMAExit, sma50,
				),
				Metrics: map[string]float64{
					"close":    last.Close,
					"sma_exit": sma50,
				},
			})
		}
	}

	// Entries: scan Universe, skip anything already held by ANY strategy.
	for _, sym := range in.Universe {
		if _, held := in.Portfolio.Positions[sym]; held {
			continue
		}
		series, ok := in.Series[sym]
		if !ok || len(series) == 0 {
			continue
		}
		n := len(series)
		if n < t.WarmupBars() {
			continue
		}
		last := series[n-1]
		closes := closesOf(series)

		sma200s := indicator.SMA(closes, p.SMALong)
		sma200 := sma200s[n-1]
		atrs := indicator.ATR(series, p.ATRPeriod)
		atr := atrs[n-1]

		if !isFinite(sma200) || !isFinite(atr) {
			continue
		}

		breakoutHigh := rollingMaxClose(closes, p.BreakoutLookback)
		if !isFinite(breakoutHigh) {
			continue
		}

		atrPct := atr / last.Close

		trendUp := last.Close > sma200
		breakout := last.Close == breakoutHigh
		volOK := atrPct < p.MaxATRPct

		if !(trendUp && breakout && volOK) {
			continue
		}

		stop := last.Close - p.ATRStopMult*atr
		signals = append(signals, domain.Signal{
			AsOf:      in.AsOf,
			Symbol:    sym,
			Kind:      domain.SignalEnter,
			RefPrice:  last.Close,
			StopPrice: stop,
			Reason: fmt.Sprintf(
				"%d günlük kırılım (%.8g). Kapanış SMA(%d)'ün üzerinde (%.8g). ATR(%d)/kapanış = %.4f%% (limit %.4f%%).",
				p.BreakoutLookback, breakoutHigh, p.SMALong, sma200, p.ATRPeriod, atrPct*100, p.MaxATRPct*100,
			),
			Metrics: map[string]float64{
				"close":             last.Close,
				"sma_long":          sma200,
				"breakout_high":     breakoutHigh,
				"atr_14":            atr,
				"atr_pct":           atrPct,
				"max_atr_pct":       p.MaxATRPct,
				"atr_stop_mult":     p.ATRStopMult,
				"breakout_lookback": float64(p.BreakoutLookback),
			},
		})
	}

	sort.SliceStable(signals, func(i, j int) bool {
		if signals[i].Symbol != signals[j].Symbol {
			return signals[i].Symbol < signals[j].Symbol
		}
		return signalKindOrder(signals[i].Kind) < signalKindOrder(signals[j].Kind)
	})

	return signals, nil
}

// signalKindOrder gives a deterministic, sensible ordering when the same
// symbol produces more than one signal kind on the same day (trendfollow
// can emit both a SignalStop and a SignalExit for a held position): stop
// updates first (informational), exits last (SPEC.md 6.7 step 8 processes
// exits before anything else regardless of the slice order the strategy
// returns, but a stable order still makes tests and logs predictable).
func signalKindOrder(k domain.SignalKind) int {
	switch k {
	case domain.SignalStop:
		return 0
	case domain.SignalEnter:
		return 1
	case domain.SignalExit:
		return 2
	default:
		return 3
	}
}

// rollingMaxClose returns max(closes[len(closes)-n:]), or NaN if there are
// fewer than n closes.
func rollingMaxClose(closes []float64, n int) float64 {
	if n <= 0 || len(closes) < n {
		return math.NaN()
	}
	window := closes[len(closes)-n:]
	max := window[0]
	for _, c := range window[1:] {
		if c > max {
			max = c
		}
	}
	return max
}

func closesOf(series []domain.Candle) []float64 {
	out := make([]float64, len(series))
	for i, c := range series {
		out[i] = c.Close
	}
	return out
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
