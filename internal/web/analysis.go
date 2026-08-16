package web

import (
	"swingbot/internal/backtest"
)

// drawdownSeries turns an equity curve into an underwater curve: at each
// point, the percentage below the running peak so far (SPEC.md Bölüm
// 7.3 madde 4).
func drawdownSeries(equity []backtest.EquityPoint) []point {
	if len(equity) == 0 {
		return nil
	}
	out := make([]point, len(equity))
	peak := equity[0].Equity
	for i, p := range equity {
		if p.Equity > peak {
			peak = p.Equity
		}
		dd := 0.0
		if peak > 0 {
			dd = (p.Equity - peak) / peak * 100
		}
		out[i] = point{X: p.Date, Y: dd}
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
// MaxDrawdown resets to that same starting point at the top of the year.
func yearlyBreakdown(equity []backtest.EquityPoint) []yearRow {
	if len(equity) == 0 {
		return nil
	}

	var rows []yearRow
	curYear := equity[0].Date.Year()
	yearStart := equity[0].Equity
	peak := yearStart
	var maxDD float64

	for i, p := range equity {
		if p.Date.Year() != curYear {
			prevEquity := equity[i-1].Equity
			ret := 0.0
			if yearStart != 0 {
				ret = prevEquity/yearStart - 1
			}
			rows = append(rows, yearRow{Year: curYear, Return: ret, MaxDrawdown: maxDD})

			curYear = p.Date.Year()
			yearStart = prevEquity
			peak = yearStart
			maxDD = 0
		}
		if p.Equity > peak {
			peak = p.Equity
		}
		if peak > 0 {
			if dd := (p.Equity - peak) / peak; dd < maxDD {
				maxDD = dd
			}
		}
	}
	last := equity[len(equity)-1].Equity
	ret := 0.0
	if yearStart != 0 {
		ret = last/yearStart - 1
	}
	rows = append(rows, yearRow{Year: curYear, Return: ret, MaxDrawdown: maxDD})

	return rows
}
