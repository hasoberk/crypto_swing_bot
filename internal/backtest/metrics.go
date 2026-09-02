package backtest

import (
	"math"
	"sort"
	"time"

	"swingbot/internal/broker"
	"swingbot/internal/domain"
)

// Metrics is SPEC.md Bölüm 7.4's metric set. İ3 requires every performance
// output to travel WITH a benchmark: BenchBTC and BenchTop10 are filled in
// by Run (via BuyAndHoldCurve/Top10EqualWeightCurve + a second Compute
// call each), never left nil for a result a report or panel might show a
// user.
type Metrics struct {
	TotalReturn   float64
	CAGR          float64
	MaxDrawdown   float64 // negative fraction, e.g. -0.15 for a 15% drawdown
	MaxDDDuration int     // gün: en uzun "son zirveden bu yana geçen süre"
	Sharpe        float64 // yıllıklandırılmış, rf=0
	Sortino       float64
	Calmar        float64 // CAGR / |MaxDrawdown|
	WinRate       float64
	// ProfitFactor is grossProfit/grossLoss. 0 when there are no closed
	// trades OR no losing trades yet (grossLoss==0) — a bare 0 would be
	// indistinguishable from "never profitable" here, so always read it
	// alongside TradeCount/WinRate rather than in isolation. (A literal
	// +Inf is deliberately avoided: it cannot round-trip through
	// runs.metrics_json's JSON encoding.)
	ProfitFactor    float64
	AvgWin, AvgLoss float64
	Expectancy      float64 // mean PnLQuote per closed trade
	TradeCount      int
	AvgHoldDays     float64
	Exposure        float64 // piyasada geçirilen zamanın ortalama oranı, 0..1
	Turnover        float64 // yıllıklandırılmış devir (işlem hacmi / ortalama equity)
	TotalFees       float64

	BenchBTC   *Metrics // never itself carries nested benchmarks
	BenchTop10 *Metrics
}

// RunningPeakDrawdown is the single shared implementation behind
// Metrics.MaxDrawdown/MaxDDDuration (below), internal/web's drawdown
// chart, and its yearly breakdown table — all three used to independently
// re-derive "walk equity, track a running peak, measure how far below it
// we are" and could silently disagree with each other on the same report
// page. seedPeak/seedPeakDate are the peak in effect BEFORE equity[0] (for
// a whole-run computation, pass equity[0].Equity/.Date; for a per-
// calendar-year slice, pass the prior year's closing equity/date so the
// year's drawdown is measured against where the curve actually stood
// entering that year, not reset to zero at the year boundary).
//
// fracBelowPeak[i] is (equity[i]-peak)/peak, always <= 0. daysSincePeak[i]
// is how many days have elapsed since the running peak was last set at or
// before i — the standard equivalent way to derive "how long has the
// current drawdown lasted" without a second pass.
func RunningPeakDrawdown(equity []EquityPoint, seedPeak float64, seedPeakDate time.Time) (fracBelowPeak []float64, daysSincePeak []int) {
	if len(equity) == 0 {
		return nil, nil
	}
	fracBelowPeak = make([]float64, len(equity))
	daysSincePeak = make([]int, len(equity))
	peak, peakDate := seedPeak, seedPeakDate
	for i, p := range equity {
		if p.Equity >= peak {
			peak = p.Equity
			peakDate = p.Date
		}
		if peak > 0 {
			fracBelowPeak[i] = (p.Equity - peak) / peak
		}
		daysSincePeak[i] = int(p.Date.Sub(peakDate).Hours() / 24)
	}
	return fracBelowPeak, daysSincePeak
}

// Compute derives Metrics from a daily equity curve and the trades closed
// during it. It never sets BenchBTC/BenchTop10 — callers (Run) compute
// those independently via BuyAndHoldCurve/Top10EqualWeightCurve and a
// second Compute call, then attach them, which is what keeps this
// function from having to guard against infinite recursion.
func Compute(equity []EquityPoint, trades []broker.ClosedTrade) Metrics {
	var m Metrics
	if len(equity) == 0 {
		return m
	}

	first, last := equity[0], equity[len(equity)-1]
	if first.Equity != 0 {
		m.TotalReturn = last.Equity/first.Equity - 1
	}

	days := last.Date.Sub(first.Date).Hours() / 24
	if days > 0 && first.Equity > 0 && last.Equity >= 0 {
		years := days / 365
		m.CAGR = math.Pow(last.Equity/first.Equity, 1/years) - 1
	}

	rets := make([]float64, 0, len(equity)-1)
	for i := 1; i < len(equity); i++ {
		if equity[i-1].Equity == 0 {
			continue
		}
		rets = append(rets, equity[i].Equity/equity[i-1].Equity-1)
	}
	mean, std := meanStdDev(rets)
	if std > 0 {
		m.Sharpe = mean / std * math.Sqrt(365)
	}
	downside := downsideDeviation(rets)
	if downside > 0 {
		m.Sortino = mean / downside * math.Sqrt(365)
	}

	fracBelowPeak, daysSincePeak := RunningPeakDrawdown(equity, first.Equity, first.Date)
	for i := range fracBelowPeak {
		if fracBelowPeak[i] < m.MaxDrawdown {
			m.MaxDrawdown = fracBelowPeak[i]
		}
		if daysSincePeak[i] > m.MaxDDDuration {
			m.MaxDDDuration = daysSincePeak[i]
		}
	}
	if m.MaxDrawdown < 0 {
		m.Calmar = m.CAGR / -m.MaxDrawdown
	}

	var expSum float64
	for _, p := range equity {
		expSum += p.Exposure
	}
	m.Exposure = expSum / float64(len(equity))

	m.TradeCount = len(trades)
	var grossProfit, grossLoss, wins, losses, holdDays, totalNotional, totalFees, pnlSum float64
	for _, tr := range trades {
		pnlSum += tr.PnLQuote
		totalFees += tr.Fees
		qty, _ := tr.Qty.Float64()
		totalNotional += tr.EntryPrice*qty + tr.ExitPrice*qty
		holdDays += tr.ExitTime.Sub(tr.EntryTime).Hours() / 24
		switch {
		case tr.PnLQuote > 0:
			grossProfit += tr.PnLQuote
			wins++
		case tr.PnLQuote < 0:
			grossLoss += -tr.PnLQuote
			losses++
		}
	}
	m.TotalFees = totalFees
	if m.TradeCount > 0 {
		m.WinRate = wins / float64(m.TradeCount)
		m.Expectancy = pnlSum / float64(m.TradeCount)
		m.AvgHoldDays = holdDays / float64(m.TradeCount)
	}
	if wins > 0 {
		m.AvgWin = grossProfit / wins
	}
	if losses > 0 {
		m.AvgLoss = -grossLoss / losses
	}
	if grossLoss > 0 {
		m.ProfitFactor = grossProfit / grossLoss
	}

	meanEquity := meanEquityOf(equity)
	if days > 0 && meanEquity > 0 {
		m.Turnover = (totalNotional / meanEquity) * (365 / days)
	}

	return m
}

func meanEquityOf(equity []EquityPoint) float64 {
	var sum float64
	for _, p := range equity {
		sum += p.Equity
	}
	if len(equity) == 0 {
		return 0
	}
	return sum / float64(len(equity))
}

func meanStdDev(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var sq float64
	for _, x := range xs {
		d := x - mean
		sq += d * d
	}
	return mean, math.Sqrt(sq / float64(len(xs)-1))
}

// downsideDeviation is the semi-deviation Sortino needs: the RMS of only
// the negative returns, computed over ALL returns (not just the negative
// count) so a mostly-positive series correctly shows low downside risk.
func downsideDeviation(rets []float64) float64 {
	if len(rets) == 0 {
		return 0
	}
	var sq float64
	for _, r := range rets {
		if r < 0 {
			sq += r * r
		}
	}
	return math.Sqrt(sq / float64(len(rets)))
}

// --- benchmarks (İ3) -------------------------------------------------------
//
// Both benchmarks apply the SAME cost model as the strategy (one round of
// fee+slippage on entry) so the comparison in the report is apples-to-
// apples, not a strategy paying realistic costs against a frictionless
// reference line.

// BuyAndHoldCurve simulates spending all of initialCash on symbol at
// calendar's first date and holding through calendar's last date — the
// BTC al-tut benchmark (SPEC.md İ3). candles must be symbol's own
// chronological history; calendar is normally the same calendar
// Run used, so the benchmark and the strategy curve line up point for
// point in the report.
func BuyAndHoldCurve(candles []domain.Candle, calendar []time.Time, initialCash float64, costs broker.Costs) []EquityPoint {
	if len(candles) == 0 || len(calendar) == 0 || initialCash <= 0 {
		return nil
	}
	byTime := make(map[int64]domain.Candle, len(candles))
	for _, c := range candles {
		byTime[c.OpenTime.UnixMilli()] = c
	}

	entryCandle, ok := byTime[calendar[0].UnixMilli()]
	if !ok {
		return nil
	}
	entryFill := entryCandle.Open * (1 + costs.SlippageBps/10000)
	qty := initialCash / (entryFill * (1 + costs.FeeRate))
	cost := qty * entryFill
	fee := cost * costs.FeeRate
	cashRemaining := initialCash - cost - fee

	out := make([]EquityPoint, 0, len(calendar))
	last := entryCandle
	for _, t := range calendar {
		if c, ok := byTime[t.UnixMilli()]; ok {
			last = c
		}
		equity := cashRemaining + qty*last.Close
		exposure := 0.0
		if equity > 0 {
			exposure = (equity - cashRemaining) / equity
		}
		out = append(out, EquityPoint{Date: t, Equity: equity, Cash: cashRemaining, Exposure: exposure})
	}
	return out
}

// Top10EqualWeightCurve buys an equal-cash slice of the top (up to) 10
// symbols by mean quote volume and holds through calendar — the
// "eşit ağırlıklı top-10" benchmark (SPEC.md İ3). Real universe/liquidity
// ranking is universe-scoring-engineer's job (Ajan 8); this is a
// best-effort stand-in using whatever candles the caller has on hand, so
// backtest.Run never has to show a performance result with no second
// benchmark at all.
func Top10EqualWeightCurve(candles map[string][]domain.Candle, calendar []time.Time, initialCash float64, costs broker.Costs) []EquityPoint {
	if len(candles) == 0 || len(calendar) == 0 || initialCash <= 0 {
		return nil
	}

	type ranked struct {
		symbol string
		volume float64
	}
	var candidates []ranked
	entryKey := calendar[0].UnixMilli()
	for sym, series := range candles {
		hasEntry := false
		var volSum float64
		for _, c := range series {
			volSum += c.QuoteVolume
			if c.OpenTime.UnixMilli() == entryKey {
				hasEntry = true
			}
		}
		if !hasEntry || len(series) == 0 {
			continue
		}
		candidates = append(candidates, ranked{sym, volSum / float64(len(series))})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].volume > candidates[j].volume })
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}
	if len(candidates) == 0 {
		return nil
	}

	share := initialCash / float64(len(candidates))
	out := make([]EquityPoint, len(calendar))
	for i, t := range calendar {
		out[i] = EquityPoint{Date: t}
	}

	for _, cand := range candidates {
		leg := BuyAndHoldCurve(candles[cand.symbol], calendar, share, costs)
		for i, p := range leg {
			out[i].Equity += p.Equity
			out[i].Cash += p.Cash
		}
	}
	for i := range out {
		if out[i].Equity > 0 {
			out[i].Exposure = (out[i].Equity - out[i].Cash) / out[i].Equity
		}
	}
	return out
}
