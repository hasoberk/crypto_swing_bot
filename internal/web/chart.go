package web

import (
	"fmt"
	"math"
	"strings"
	"time"

	"swingbot/internal/backtest"
)

// point is a single (time, value) sample. Charts in this file are
// rendered server-side as plain SVG — no client-side charting library, so
// the report stays a genuinely dependency-free single file (SPEC.md
// Bölüm 7.3).
type point struct {
	X time.Time
	Y float64
}

type namedSeries struct {
	Label  string
	Color  string
	Points []point
}

func toXY(eq []backtest.EquityPoint) []point {
	out := make([]point, len(eq))
	for i, p := range eq {
		out[i] = point{X: p.Date, Y: p.Equity}
	}
	return out
}

const (
	chartPadLeft   = 56
	chartPadRight  = 12
	chartPadTop    = 12
	chartPadBottom = 24
)

// lineChartSVG renders series as an SVG line chart. logScale applies
// math.Log10 to Y values (skipping non-positive ones, which cannot be
// placed on a log axis) — used for the equity curve per SPEC.md Bölüm
// 7.3's "logaritmik ölçek" requirement.
func lineChartSVG(width, height int, series []namedSeries, logScale bool) string {
	minY, maxY := math.Inf(1), math.Inf(-1)
	var minX, maxX time.Time
	first := true
	for _, s := range series {
		for _, p := range s.Points {
			y := p.Y
			if logScale {
				if y <= 0 {
					continue
				}
				y = math.Log10(y)
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
			if first || p.X.Before(minX) {
				minX = p.X
			}
			if first || p.X.After(maxX) {
				maxX = p.X
			}
			first = false
		}
	}
	if first || minY == maxY {
		return fmt.Sprintf("<svg viewBox=\"0 0 %d %d\"><text x=\"12\" y=\"20\" fill=\"%s\" font-size=\"12\">veri yok</text></svg>\n", width, height, colGhost)
	}

	plotW := float64(width - chartPadLeft - chartPadRight)
	plotH := float64(height - chartPadTop - chartPadBottom)
	xSpan := maxX.Sub(minX).Seconds()
	if xSpan == 0 {
		xSpan = 1
	}

	sx := func(t time.Time) float64 {
		return float64(chartPadLeft) + t.Sub(minX).Seconds()/xSpan*plotW
	}
	sy := func(y float64) float64 {
		v := y
		if logScale {
			if v <= 0 {
				v = minY
			} else {
				v = math.Log10(v)
			}
		}
		return float64(chartPadTop) + (1-(v-minY)/(maxY-minY))*plotH
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<svg viewBox=\"0 0 %d %d\" role=\"img\">\n", width, height)

	// Gridlines + Y labels at min/mid/max.
	for i, frac := range []float64{0, 0.5, 1} {
		yv := minY + frac*(maxY-minY)
		ypix := float64(chartPadTop) + (1-frac)*plotH
		fmt.Fprintf(&b, "<line x1=\"%d\" y1=\"%.1f\" x2=\"%d\" y2=\"%.1f\" stroke=\"%s\" stroke-width=\"1\"/>\n",
			chartPadLeft, ypix, width-chartPadRight, ypix, colHairline)
		label := yv
		if logScale {
			label = math.Pow(10, yv)
		}
		fmt.Fprintf(&b, "<text x=\"2\" y=\"%.1f\" font-size=\"10\" fill=\"%s\">%s</text>\n", ypix+3, colGhost, formatAxisNumber(label))
		_ = i
	}
	fmt.Fprintf(&b, "<text x=\"%d\" y=\"%d\" font-size=\"10\" fill=\"%s\">%s</text>\n", chartPadLeft, height-6, colGhost, minX.Format("2006-01-02"))
	fmt.Fprintf(&b, "<text x=\"%d\" y=\"%d\" font-size=\"10\" fill=\"%s\" text-anchor=\"end\">%s</text>\n", width-chartPadRight, height-6, colGhost, maxX.Format("2006-01-02"))

	for _, s := range series {
		var pts strings.Builder
		for _, p := range s.Points {
			if logScale && p.Y <= 0 {
				continue
			}
			fmt.Fprintf(&pts, "%.2f,%.2f ", sx(p.X), sy(p.Y))
		}
		fmt.Fprintf(&b, "<polyline fill=\"none\" stroke=\"%s\" stroke-width=\"1.5\" points=\"%s\"/>\n", s.Color, strings.TrimSpace(pts.String()))
	}

	b.WriteString("</svg>\n")
	return b.String()
}

func writeLegend(b *strings.Builder, series []namedSeries) {
	b.WriteString("<div class=\"legend\">\n")
	for _, s := range series {
		fmt.Fprintf(b, "<span style=\"color:%s\">%s</span>\n", s.Color, s.Label)
	}
	b.WriteString("</div>\n")
}

// histogramSVG bins values into n buckets and renders them as bars,
// coloring by sign (used for the PnL histogram; harmless for the
// hold-duration histogram, which is never negative).
func histogramSVG(width, height int, values []float64, n int) string {
	if len(values) == 0 {
		return fmt.Sprintf("<svg viewBox=\"0 0 %d %d\"><text x=\"12\" y=\"20\" fill=\"%s\" font-size=\"12\">işlem yok</text></svg>\n", width, height, colGhost)
	}
	minV, maxV := values[0], values[0]
	for _, v := range values {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	if minV == maxV {
		maxV = minV + 1
	}
	counts := make([]int, n)
	for _, v := range values {
		idx := int((v - minV) / (maxV - minV) * float64(n))
		if idx >= n {
			idx = n - 1
		}
		if idx < 0 {
			idx = 0
		}
		counts[idx]++
	}
	maxCount := 1
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}

	plotW := float64(width - chartPadLeft - chartPadRight)
	plotH := float64(height - chartPadTop - chartPadBottom)
	barW := plotW / float64(n)

	var b strings.Builder
	fmt.Fprintf(&b, "<svg viewBox=\"0 0 %d %d\" role=\"img\">\n", width, height)
	fmt.Fprintf(&b, "<line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"%s\"/>\n",
		chartPadLeft, height-chartPadBottom, width-chartPadRight, height-chartPadBottom, colHairline)
	for i, c := range counts {
		barH := float64(c) / float64(maxCount) * plotH
		x := float64(chartPadLeft) + float64(i)*barW
		y := float64(chartPadTop) + (plotH - barH)
		binMid := minV + (float64(i)+0.5)*(maxV-minV)/float64(n)
		color := colGain
		if binMid < 0 {
			color = colLoss
		}
		fmt.Fprintf(&b, "<rect x=\"%.2f\" y=\"%.2f\" width=\"%.2f\" height=\"%.2f\" fill=\"%s\"/>\n", x+1, y, barW-2, barH, color)
	}
	fmt.Fprintf(&b, "<text x=\"%d\" y=\"%d\" font-size=\"10\" fill=\"%s\">%s</text>\n", chartPadLeft, height-6, colGhost, formatAxisNumber(minV))
	fmt.Fprintf(&b, "<text x=\"%d\" y=\"%d\" font-size=\"10\" fill=\"%s\" text-anchor=\"end\">%s</text>\n", width-chartPadRight, height-6, colGhost, formatAxisNumber(maxV))
	b.WriteString("</svg>\n")
	return b.String()
}

func formatAxisNumber(v float64) string {
	switch {
	case math.Abs(v) >= 1000:
		return fmt.Sprintf("%.0f", v)
	case math.Abs(v) >= 1:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.3f", v)
	}
}
