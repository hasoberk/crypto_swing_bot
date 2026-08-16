// Package web is the visual layer (SPEC.md Bölüm 7): the single-file
// backtest HTML report (this file) and, once panel-developer (Ajan 12)
// lands it, the local read-only panel server.
package web

import (
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"swingbot/internal/backtest"
	"swingbot/internal/broker"
)

// ReportData is everything GenerateReport needs to render a
// reports/backtest_<zaman>.html file (SPEC.md Bölüm 7.3).
type ReportData struct {
	Strategy    string
	Params      map[string]any
	Start, End  time.Time
	Costs       broker.Costs
	GitSHA      string
	GeneratedAt time.Time
	Warnings    []string // survivorship bias, data gaps, short sample, ...

	Metrics    backtest.Metrics
	Equity     []backtest.EquityPoint // strateji
	BenchBTC   []backtest.EquityPoint
	BenchTop10 []backtest.EquityPoint

	Trades     []broker.ClosedTrade
	Rejections []backtest.Rejection

	// ParamSensitivity is nil for a single run — section 9 renders "tek
	// koşum, duyarlılık taraması yok" instead of a table. Populated by a
	// caller that ran ParamSensitivitySweep (validation-analysis-engineer
	// territory, Faz 3) is out of scope here.
	ParamSensitivity []ParamSensitivityRow
}

// ParamSensitivityRow is one point in a parameter sweep: a parameter
// value and the metric it produced, for section 9's table.
type ParamSensitivityRow struct {
	Param                     string
	Value                     string
	Sharpe, CAGR, MaxDrawdown float64
}

// colors mirrors SPEC.md Bölüm 7.2's panel token palette so the backtest
// report and the eventual panel (Ajan 12) read as one visual system.
const (
	colGround   = "#E6E9E4"
	colInk      = "#16191C"
	colHairline = "#C2C7C1"
	colGhost    = "#98A199"
	colGain     = "#1F6F4A"
	colLoss     = "#A32E22"
	colPending  = "#C9782F"
)

// WriteReportFile renders data and writes it to
// <dir>/backtest_<YYYYMMDD_HHMMSS>.html, creating dir if needed. It
// returns the path written, for runs.report_path (SPEC.md Bölüm 4.1).
func WriteReportFile(dir string, data ReportData) (string, error) {
	if data.GeneratedAt.IsZero() {
		data.GeneratedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("web: report dir %s: %w", dir, err)
	}
	name := fmt.Sprintf("backtest_%s.html", data.GeneratedAt.Format("20060102_150405"))
	path := filepath.Join(dir, name)

	out, err := GenerateReport(data)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return "", fmt.Errorf("web: write %s: %w", path, err)
	}
	return path, nil
}

// GenerateReport renders data into a single self-contained HTML document
// (CSS and JS inline, no external dependency — SPEC.md Bölüm 7.3) covering
// all 9 required sections in order.
func GenerateReport(data ReportData) (string, error) {
	var b strings.Builder

	b.WriteString("<!doctype html>\n<html lang=\"tr\"><head><meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>swingbot backtest — %s</title>\n", html.EscapeString(data.Strategy))
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString(reportCSS())
	b.WriteString("</head><body>\n<main>\n")

	writeHeaderSection(&b, data)
	writeWarningsSection(&b, data)
	writeEquitySection(&b, data)
	writeDrawdownSection(&b, data)
	writeMetricsTableSection(&b, data)
	writeYearlyBreakdownSection(&b, data)
	writeTradeDistributionSection(&b, data)
	writeTradeListSection(&b, data)
	writeParamSensitivitySection(&b, data)

	b.WriteString("</main>\n")
	b.WriteString(reportJS())
	b.WriteString("</body></html>\n")
	return b.String(), nil
}

func reportCSS() string {
	return fmt.Sprintf(`<style>
:root {
  --ground: %s; --ink: %s; --hairline: %s; --ghost: %s;
  --gain: %s; --loss: %s; --pending: %s;
}
* { box-sizing: border-box; }
body {
  background: var(--ground); color: var(--ink); margin: 0;
  font-family: "Inter Tight", "IBM Plex Sans Condensed", system-ui, sans-serif;
}
main { max-width: 1100px; margin: 0 auto; padding: 24px 20px 80px; }
section { margin-top: 40px; }
h1 { font-size: 20px; letter-spacing: 0.02em; text-transform: uppercase; }
h2 {
  font-size: 13px; letter-spacing: 0.08em; text-transform: uppercase;
  color: var(--ghost); border-bottom: 1px solid var(--hairline); padding-bottom: 6px;
}
table { border-collapse: collapse; width: 100%%; font-variant-numeric: tabular-nums; }
th, td {
  text-align: right; padding: 4px 10px; border-bottom: 1px solid var(--hairline);
  font-family: ui-monospace, "SF Mono", "JetBrains Mono", monospace; font-size: 13px;
}
th:first-child, td:first-child { text-align: left; }
th { color: var(--ghost); font-weight: 500; cursor: pointer; user-select: none; }
.mono { font-family: ui-monospace, "SF Mono", "JetBrains Mono", monospace; font-variant-numeric: tabular-nums; }
.gain { color: var(--gain); }
.loss { color: var(--loss); }
.pending { color: var(--pending); }
.warn {
  border: 1px solid var(--pending); color: var(--pending); padding: 10px 14px;
  margin-bottom: 6px; font-size: 13px;
}
.meta { color: var(--ghost); font-size: 13px; }
.legend { display: flex; gap: 16px; font-size: 12px; color: var(--ghost); margin-top: 4px; }
.legend span::before { content: "—"; margin-right: 4px; }
svg { display: block; width: 100%%; height: auto; }
@media (prefers-reduced-motion: reduce) { * { animation: none !important; transition: none !important; } }
</style>
`, colGround, colInk, colHairline, colGhost, colGain, colLoss, colPending)
}

func reportJS() string {
	// Minimal click-to-sort for the trade list table (SPEC.md Bölüm 7.3
	// madde 8: "tam tablo, sıralanabilir"). No external library.
	return `<script>
document.querySelectorAll("table[data-sortable] th").forEach(function (th, idx) {
  th.addEventListener("click", function () {
    var table = th.closest("table");
    var tbody = table.tBodies[0];
    var rows = Array.prototype.slice.call(tbody.rows);
    var asc = th.dataset.asc !== "1";
    rows.sort(function (a, b) {
      var av = a.cells[idx].dataset.sort || a.cells[idx].textContent;
      var bv = b.cells[idx].dataset.sort || b.cells[idx].textContent;
      var an = parseFloat(av), bn = parseFloat(bv);
      var cmp = (!isNaN(an) && !isNaN(bn)) ? an - bn : av.localeCompare(bv);
      return asc ? cmp : -cmp;
    });
    rows.forEach(function (r) { tbody.appendChild(r); });
    table.querySelectorAll("th").forEach(function (h) { h.dataset.asc = ""; });
    th.dataset.asc = asc ? "1" : "0";
  });
});
</script>
`
}

// --- 1. Başlık bloğu ---------------------------------------------------

func writeHeaderSection(b *strings.Builder, d ReportData) {
	b.WriteString("<h1>swingbot backtest raporu</h1>\n<section class=\"meta\">\n")
	fmt.Fprintf(b, "<div>Strateji: <strong>%s</strong></div>\n", html.EscapeString(d.Strategy))
	fmt.Fprintf(b, "<div>Dönem: %s → %s</div>\n", d.Start.Format("2006-01-02"), d.End.Format("2006-01-02"))
	fmt.Fprintf(b, "<div>Maliyetler: fee_rate=%.4f, slippage_bps=%.1f</div>\n", d.Costs.FeeRate, d.Costs.SlippageBps)
	if d.GitSHA != "" {
		fmt.Fprintf(b, "<div>Git SHA: %s</div>\n", html.EscapeString(d.GitSHA))
	}
	fmt.Fprintf(b, "<div>Üretim zamanı: %s UTC</div>\n", d.GeneratedAt.Format("2006-01-02 15:04:05"))
	if len(d.Params) > 0 {
		keys := make([]string, 0, len(d.Params))
		for k := range d.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, d.Params[k]))
		}
		fmt.Fprintf(b, "<div>Parametreler: %s</div>\n", html.EscapeString(strings.Join(parts, ", ")))
	}
	b.WriteString("</section>\n")
}

// --- 2. Uyarı bloğu -----------------------------------------------------

func writeWarningsSection(b *strings.Builder, d ReportData) {
	if len(d.Warnings) == 0 {
		return
	}
	b.WriteString("<section><h2>Uyarılar</h2>\n")
	for _, w := range d.Warnings {
		fmt.Fprintf(b, "<div class=\"warn\">%s</div>\n", html.EscapeString(w))
	}
	b.WriteString("</section>\n")
}

// --- 3. Equity eğrisi -----------------------------------------------------

func writeEquitySection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>Equity Eğrisi (logaritmik)</h2>\n")
	series := []namedSeries{{"strateji", colInk, toXY(d.Equity)}}
	if len(d.BenchBTC) > 0 {
		series = append(series, namedSeries{"BTC al-tut", colGhost, toXY(d.BenchBTC)})
	}
	if len(d.BenchTop10) > 0 {
		series = append(series, namedSeries{"eşit ağırlıklı top-10", colPending, toXY(d.BenchTop10)})
	}
	b.WriteString(lineChartSVG(1060, 320, series, true))
	writeLegend(b, series)
	b.WriteString("</section>\n")
}

// --- 4. Düşüş (drawdown) grafiği -------------------------------------------

func writeDrawdownSection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>Düşüş (Drawdown)</h2>\n")
	dd := drawdownSeries(d.Equity)
	b.WriteString(lineChartSVG(1060, 200, []namedSeries{{"drawdown", colLoss, dd}}, false))
	b.WriteString("</section>\n")
}

// --- 5. Metrik tablosu ------------------------------------------------------

func writeMetricsTableSection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>Metrikler</h2>\n<table><thead><tr><th>Metrik</th><th>Strateji</th><th>BTC al-tut</th><th>Top-10</th></tr></thead><tbody>\n")
	rows := []struct {
		label string
		get   func(m backtest.Metrics) string
	}{
		{"Toplam Getiri", func(m backtest.Metrics) string { return pct(m.TotalReturn) }},
		{"CAGR", func(m backtest.Metrics) string { return pct(m.CAGR) }},
		{"Maks. Düşüş", func(m backtest.Metrics) string { return pct(m.MaxDrawdown) }},
		{"Maks. Düşüş Süresi (gün)", func(m backtest.Metrics) string { return fmt.Sprintf("%d", m.MaxDDDuration) }},
		{"Sharpe", func(m backtest.Metrics) string { return num(m.Sharpe) }},
		{"Sortino", func(m backtest.Metrics) string { return num(m.Sortino) }},
		{"Calmar", func(m backtest.Metrics) string { return num(m.Calmar) }},
		{"Kazanma Oranı", func(m backtest.Metrics) string { return pct(m.WinRate) }},
		{"Kâr Faktörü", func(m backtest.Metrics) string { return num(m.ProfitFactor) }},
		{"Ort. Kazanç", func(m backtest.Metrics) string { return num(m.AvgWin) }},
		{"Ort. Kayıp", func(m backtest.Metrics) string { return num(m.AvgLoss) }},
		{"Beklenti (Expectancy)", func(m backtest.Metrics) string { return num(m.Expectancy) }},
		{"İşlem Sayısı", func(m backtest.Metrics) string { return fmt.Sprintf("%d", m.TradeCount) }},
		{"Ort. Tutma Süresi (gün)", func(m backtest.Metrics) string { return num(m.AvgHoldDays) }},
		{"Maruziyet", func(m backtest.Metrics) string { return pct(m.Exposure) }},
		{"Devir (yıllık)", func(m backtest.Metrics) string { return num(m.Turnover) }},
		{"Toplam Komisyon", func(m backtest.Metrics) string { return num(m.TotalFees) }},
	}
	empty := backtest.Metrics{}
	for _, r := range rows {
		btc, top10 := empty, empty
		if d.Metrics.BenchBTC != nil {
			btc = *d.Metrics.BenchBTC
		}
		if d.Metrics.BenchTop10 != nil {
			top10 = *d.Metrics.BenchTop10
		}
		fmt.Fprintf(b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(r.label), r.get(d.Metrics), r.get(btc), r.get(top10))
	}
	b.WriteString("</tbody></table></section>\n")
}

// --- 6. Yıllık kırılım -------------------------------------------------------

func writeYearlyBreakdownSection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>Yıllık Kırılım</h2>\n<table><thead><tr><th>Yıl</th><th>Getiri</th><th>Maks. Düşüş</th></tr></thead><tbody>\n")
	for _, yr := range yearlyBreakdown(d.Equity) {
		cls := "gain"
		if yr.Return < 0 {
			cls = "loss"
		}
		fmt.Fprintf(b, "<tr><td>%d</td><td class=\"%s\">%s</td><td>%s</td></tr>\n", yr.Year, cls, pct(yr.Return), pct(yr.MaxDrawdown))
	}
	b.WriteString("</tbody></table></section>\n")
}

// --- 7. İşlem dağılımı --------------------------------------------------------

func writeTradeDistributionSection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>İşlem Dağılımı</h2>\n")
	pnls := make([]float64, len(d.Trades))
	holds := make([]float64, len(d.Trades))
	for i, tr := range d.Trades {
		pnls[i] = tr.PnLQuote
		holds[i] = tr.ExitTime.Sub(tr.EntryTime).Hours() / 24
	}
	b.WriteString("<div class=\"meta\">K/Z dağılımı (quote)</div>\n")
	b.WriteString(histogramSVG(1060, 140, pnls, 20))
	b.WriteString("<div class=\"meta\" style=\"margin-top:16px\">Tutma süresi dağılımı (gün)</div>\n")
	b.WriteString(histogramSVG(1060, 140, holds, 20))
	b.WriteString("</section>\n")
}

// --- 8. İşlem listesi --------------------------------------------------------

func writeTradeListSection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>İşlem Listesi</h2>\n<table data-sortable><thead><tr>")
	for _, h := range []string{"Sembol", "Giriş", "Giriş Fiyatı", "Çıkış", "Çıkış Fiyatı", "Miktar", "K/Z", "K/Z %", "Komisyon", "Gerekçe"} {
		fmt.Fprintf(b, "<th>%s</th>", h)
	}
	b.WriteString("</tr></thead><tbody>\n")
	for _, tr := range d.Trades {
		cls := ""
		switch {
		case tr.PnLQuote > 0:
			cls = "gain"
		case tr.PnLQuote < 0:
			cls = "loss"
		}
		qty, _ := tr.Qty.Float64()
		fmt.Fprintf(b, "<tr><td>%s</td><td data-sort=\"%d\">%s</td><td>%s</td><td data-sort=\"%d\">%s</td><td>%s</td><td>%.8f</td>"+
			"<td class=\"%s\" data-sort=\"%f\">%s</td><td class=\"%s\">%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(tr.Symbol),
			tr.EntryTime.Unix(), tr.EntryTime.Format("2006-01-02"), num(tr.EntryPrice),
			tr.ExitTime.Unix(), tr.ExitTime.Format("2006-01-02"), num(tr.ExitPrice),
			qty,
			cls, tr.PnLQuote, num(tr.PnLQuote),
			cls, pct(tr.PnLPct),
			num(tr.Fees), html.EscapeString(tr.ExitReason))
	}
	b.WriteString("</tbody></table>\n")
	if len(d.Rejections) > 0 {
		b.WriteString("<div class=\"meta\" style=\"margin-top:12px\">Reddedilen sinyaller (SPEC.md 6.5.2: neden işlem yapılmadı)</div>\n")
		b.WriteString("<table><thead><tr><th>Tarih</th><th>Sembol</th><th>Gerekçe</th></tr></thead><tbody>\n")
		for _, r := range d.Rejections {
			fmt.Fprintf(b, "<tr><td>%s</td><td>%s</td><td class=\"pending\">%s</td></tr>\n",
				r.AsOf.Format("2006-01-02"), html.EscapeString(r.Symbol), html.EscapeString(r.Reason))
		}
		b.WriteString("</tbody></table>\n")
	}
	b.WriteString("</section>\n")
}

// --- 9. Parametre duyarlılığı -------------------------------------------------

func writeParamSensitivitySection(b *strings.Builder, d ReportData) {
	b.WriteString("<section><h2>Parametre Duyarlılığı</h2>\n")
	if len(d.ParamSensitivity) == 0 {
		b.WriteString("<div class=\"meta\">Tek koşum — duyarlılık taraması yok (bkz. SPEC.md Bölüm 11.3, Faz 3 / validation-analysis-engineer).</div>\n")
		b.WriteString("</section>\n")
		return
	}
	b.WriteString("<table><thead><tr><th>Parametre</th><th>Değer</th><th>Sharpe</th><th>CAGR</th><th>Maks. Düşüş</th></tr></thead><tbody>\n")
	for _, r := range d.ParamSensitivity {
		fmt.Fprintf(b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(r.Param), html.EscapeString(r.Value), num(r.Sharpe), pct(r.CAGR), pct(r.MaxDrawdown))
	}
	b.WriteString("</tbody></table></section>\n")
}

// --- formatting helpers -----------------------------------------------------

func pct(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", f*100)
}

func num(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "—"
	}
	return fmt.Sprintf("%.4f", f)
}
