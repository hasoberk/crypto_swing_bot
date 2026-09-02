package web

import (
	"swingbot/internal/backtest"
)

// drawdownSeries turns an equity curve into an underwater curve: at each
// point, the percentage below the running peak so far (SPEC.md Bölüm
// 7.3 madde 4). Shares backtest.RunningPeakDrawdown with Metrics'
// MaxDrawdown/MaxDDDuration and yearlyBreakdown below, so this chart can
// never numerically disagree with the metrics table on the same page.
func drawdownSeries(equity []backtest.EquityPoint) []point {
	if len(equity) == 0 {
		return nil
	}
	fracBelowPeak, _ := backtest.RunningPeakDrawdown(equity, equity[0].Equity, equity[0].Date)
	out := make([]point, len(equity))
	for i, p := range equity {
		out[i] = point{X: p.Date, Y: fracBelowPeak[i] * 100}
	}
	return out
}

// yearRow is one calendar year's return and max drawdown, for section 6
// (SPEC.md Bölüm 7.3 madde 6: "her takvim yılı için getiri ve maksimum düşüş").
type yearRow struct {
	Year        int
	Return      float64
	MaxDrawdown float64
}

// yearlyBreakdown groups equity into calendar years. A year's Return is
// measured from the last equity point before that year began (or the
// series' first point, for the first year) to the year's last point;
// MaxDrawdown resets to that same starting point at the top of the year
// (via backtest.RunningPeakDrawdown's seed parameters — see drawdownSeries).
func yearlyBreakdown(equity []backtest.EquityPoint) []yearRow {
	if len(equity) == 0 {
		return nil
	}

	var rows []yearRow
	yearStart := equity[0].Equity
	seedPeak, seedDate := yearStart, equity[0].Date
	start := 0
	curYear := equity[0].Date.Year()

	flush := func(end int) {
		fracBelowPeak, _ := backtest.RunningPeakDrawdown(equity[start:end], seedPeak, seedDate)
		maxDD := 0.0
		for _, dd := range fracBelowPeak {
			if dd < maxDD {
				maxDD = dd
			}
		}
		ret := 0.0
		if yearStart != 0 {
			ret = equity[end-1].Equity/yearStart - 1
		}
		rows = append(rows, yearRow{Year: curYear, Return: ret, MaxDrawdown: maxDD})
	}

	for i, p := range equity {
		if p.Date.Year() != curYear {
			flush(i)
			yearStart = equity[i-1].Equity
			seedPeak, seedDate = yearStart, equity[i-1].Date
			start = i
			curYear = p.Date.Year()
		}
	}
	flush(len(equity))

	return rows
}
