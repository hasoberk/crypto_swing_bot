// walkforward_report.go renders the SPEC.md Bölüm 11 validation report:
// walk-forward windows, the combined out-of-sample track record (vs BTC —
// İ3), the per-window regime breakdown, the parameter sensitivity heatmap
// with its plateau-vs-peak verdicts (Bölüm 11.3), and the Bölüm 11.4
// go/no-go stamp. It is deliberately a SEPARATE file from report.go's
// single-run backtest report (owned by backtest-engine-architect / Ajan 6)
// to avoid two agents editing the same file — the two reports share
// report.go's CSS tokens/colors and chart.go's SVG helpers (same package),
// so they stay visually one system (SPEC.md Bölüm 7.2) without touching
// each other's code.
package web

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"swingbot/internal/backtest"
	"swingbot/internal/broker"
)

// WalkForwardReportData is everything GenerateWalkForwardReport needs.
type WalkForwardReportData struct {
	Strategy      string
	Segment       string // "geliştirme" | "KİLİTLİ" (SPEC.md Bölüm 11.1)
	ParamGridDesc []string
	Costs         broker.Costs
	GitSHA        string
	GeneratedAt   time.Time
	Warnings      []string

	Windows          []backtest.WindowResult
	CombinedEquity   []backtest.EquityPoint
	CombinedBenchBTC []backtest.EquityPoint
	Metrics          backtest.Metrics

	Sensitivity []backtest.SensitivityPoint
	Plateaus    []backtest.PlateauVerdict

	Thresholds backtest.Thresholds
	Verdict    backtest.Verdict
}

// WriteWalkForwardReportFile renders data and writes it to
// <dir>/walkforward_<YYYYMMDD_HHMMSS>.html, creating dir if needed.
func WriteWalkForwardReportFile(dir string, data WalkForwardReportData) (string, error) {
	if data.GeneratedAt.IsZero() {
		data.GeneratedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("web: report dir %s: %w", dir, err)
	}
	name := fmt.Sprintf("walkforward_%s.html", data.GeneratedAt.Format("20060102_150405"))
	path := filepath.Join(dir, name)

	out, err := GenerateWalkForwardReport(data)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return "", fmt.Errorf("web: write %s: %w", path, err)
	}
	return path, nil
}

// GenerateWalkForwardReport renders data into a single self-contained HTML
// document, same no-external-dependency constraint as report.go (SPEC.md
// Bölüm 7.3).
func GenerateWalkForwardReport(data WalkForwardReportData) (string, error) {
	var b strings.Builder

	b.WriteString("<!doctype html>\n<html lang=\"tr\"><head><meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>swingbot walk-forward — %s</title>\n", html.EscapeString(data.Strategy))
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString(reportCSS())
	b.WriteString("</head><body>\n<main>\n")

	writeWFHeaderSection(&b, data)
	writeWFWarningsSection(&b, data)
	writeWFVerdictSection(&b, data)
	writeWFEquitySection(&b, data)
	writeWFDrawdownSection(&b, data)
	writeWFMetricsTableSection(&b, data)
	writeWFRegimeSection(&b, data)
	writeWFWindowsSection(&b, data)
	writeWFSensitivitySection(&b, data)

	b.WriteString("</main>\n")
	b.WriteString(reportJS())
	b.WriteString("</body></html>\n")
	return b.String(), nil
}

// --- 1. Başlık bloğu ---------------------------------------------------

func writeWFHeaderSection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<h1>swingbot walk-forward doğrulama raporu</h1>\n<section class=\"meta\">\n")
	fmt.Fprintf(b, "<div>Strateji: <strong>%s</strong></div>\n", html.EscapeString(d.Strategy))
	fmt.Fprintf(b, "<div>Bölüm: <strong>%s</strong> (SPEC.md Bölüm 11.1)</div>\n", html.EscapeString(d.Segment))
	fmt.Fprintf(b, "<div>Pencere sayısı: %d</div>\n", len(d.Windows))
	fmt.Fprintf(b, "<div>Maliyetler: fee_rate=%.4f, slippage_bps=%.1f</div>\n", d.Costs.FeeRate, d.Costs.SlippageBps)
	if d.GitSHA != "" {
		fmt.Fprintf(b, "<div>Git SHA: %s</div>\n", html.EscapeString(d.GitSHA))
	}
	fmt.Fprintf(b, "<div>Üretim zamanı: %s UTC</div>\n", d.GeneratedAt.Format("2006-01-02 15:04:05"))
	if len(d.ParamGridDesc) > 0 {
		fmt.Fprintf(b, "<div>Aranan parametreler: %s</div>\n", html.EscapeString(strings.Join(d.ParamGridDesc, "; ")))
	}
	b.WriteString("</section>\n")
}

// --- 2. Uyarı bloğu -----------------------------------------------------

func writeWFWarningsSection(b *strings.Builder, d WalkForwardReportData) {
	if len(d.Warnings) == 0 {
		return
	}
	b.WriteString("<section><h2>Uyarılar</h2>\n")
	for _, w := range d.Warnings {
		fmt.Fprintf(b, "<div class=\"warn\">%s</div>\n", html.EscapeString(w))
	}
	b.WriteString("</section>\n")
}

// --- Bölüm 11.4 damgası: GEÇTİ / TERK EDİLDİ -------------------------------

func writeWFVerdictSection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Devam Etme Eşikleri (SPEC.md Bölüm 11.4)</h2>\n")
	if len(d.Verdict.Criteria) == 0 {
		b.WriteString("<div class=\"meta\">Eşikler henüz değerlendirilmedi.</div></section>\n")
		return
	}

	stampClass, stampText := "loss", "TERK EDİLDİ"
	if d.Verdict.Passed {
		stampClass, stampText = "gain", "GEÇTİ"
	}
	fmt.Fprintf(b, "<div class=\"%s\" style=\"font-size:22px;font-weight:700;letter-spacing:0.08em;text-transform:uppercase;margin-bottom:10px;\">%s</div>\n", stampClass, stampText)
	if !d.Verdict.Passed {
		b.WriteString("<div class=\"warn\">En az bir eşik karşılanmadı. SPEC.md Bölüm 11.4: strateji İYİLEŞTİRİLMEZ, TERK EDİLİR — Faz 2'ye dönüp farklı bir hipotezle baştan başlayın.</div>\n")
	}

	b.WriteString("<table><thead><tr><th>Kriter</th><th>Sonuç</th><th>Detay</th></tr></thead><tbody>\n")
	for _, c := range d.Verdict.Criteria {
		cls, mark := "gain", "GEÇTİ"
		if !c.Passed {
			cls, mark = "loss", "TERK EDİLDİ"
		}
		fmt.Fprintf(b, "<tr><td>%s</td><td class=\"%s\">%s</td><td>%s</td></tr>\n",
			html.EscapeString(c.Name), cls, mark, html.EscapeString(c.Detail))
	}
	b.WriteString("</tbody></table></section>\n")
}

// --- Birleşik equity eğrisi (İ3) -------------------------------------------

func writeWFEquitySection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Birleşik Walk-Forward Equity Eğrisi (logaritmik)</h2>\n")
	b.WriteString("<div class=\"meta\">Her pencerenin test dilimi bir öncekinin equity'sinden devam eder — bu tek bir sürekli out-of-sample kayıt gibi okunur (SPEC.md Bölüm 11.2).</div>\n")
	series := []namedSeries{{"strateji (birleşik test)", colInk, toXY(d.CombinedEquity)}}
	if len(d.CombinedBenchBTC) > 0 {
		series = append(series, namedSeries{"BTC al-tut", colGhost, toXY(d.CombinedBenchBTC)})
	}
	b.WriteString(lineChartSVG(1060, 320, series, true))
	writeLegend(b, series)
	b.WriteString("</section>\n")
}

func writeWFDrawdownSection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Düşüş (Drawdown) — birleşik</h2>\n")
	dd := drawdownSeries(d.CombinedEquity)
	b.WriteString(lineChartSVG(1060, 200, []namedSeries{{"drawdown", colLoss, dd}}, false))
	b.WriteString("</section>\n")
}

func writeWFMetricsTableSection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Birleşik Metrikler</h2>\n<table><thead><tr><th>Metrik</th><th>Strateji</th><th>BTC al-tut</th></tr></thead><tbody>\n")
	btc := backtest.Compute(d.CombinedBenchBTC, nil)
	rows := []struct {
		label string
		get   func(m backtest.Metrics) string
	}{
		{"Toplam Getiri", func(m backtest.Metrics) string { return pct(m.TotalReturn) }},
		{"CAGR", func(m backtest.Metrics) string { return pct(m.CAGR) }},
		{"Maks. Düşüş", func(m backtest.Metrics) string { return pct(m.MaxDrawdown) }},
		{"Sharpe", func(m backtest.Metrics) string { return num(m.Sharpe) }},
		{"Calmar", func(m backtest.Metrics) string { return num(m.Calmar) }},
		{"Kazanma Oranı", func(m backtest.Metrics) string { return pct(m.WinRate) }},
		{"İşlem Sayısı", func(m backtest.Metrics) string { return fmt.Sprintf("%d", m.TradeCount) }},
	}
	for _, r := range rows {
		fmt.Fprintf(b, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>\n", html.EscapeString(r.label), r.get(d.Metrics), r.get(btc))
	}
	b.WriteString("</tbody></table></section>\n")
}

// --- Rejim bazlı kırılım (SPEC.md Bölüm 14, Faz 3 kabul kriteri) ----------

func writeWFRegimeSection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Rejim Bazlı Kırılım</h2>\n")
	b.WriteString("<table><thead><tr><th>Rejim</th><th>Pencere</th><th>Ort. Strateji Getirisi</th><th>Ort. BTC Getirisi</th><th>Fark</th></tr></thead><tbody>\n")
	type bucket struct {
		windows          int
		stratSum, btcSum float64
	}
	buckets := map[backtest.Regime]*bucket{}
	for _, w := range d.Windows {
		if len(w.TestEquity) == 0 || len(w.TestBenchBTC) == 0 {
			continue
		}
		stratRet := w.TestEquity[len(w.TestEquity)-1].Equity/w.TestEquity[0].Equity - 1
		btcRet := w.TestBenchBTC[len(w.TestBenchBTC)-1].Equity/w.TestBenchBTC[0].Equity - 1
		bk, ok := buckets[w.Regime]
		if !ok {
			bk = &bucket{}
			buckets[w.Regime] = bk
		}
		bk.windows++
		bk.stratSum += stratRet
		bk.btcSum += btcRet
	}
	for _, regime := range []backtest.Regime{backtest.RegimeBull, backtest.RegimeBear, backtest.RegimeSideways} {
		bk, ok := buckets[regime]
		if !ok || bk.windows == 0 {
			continue
		}
		stratAvg := bk.stratSum / float64(bk.windows)
		btcAvg := bk.btcSum / float64(bk.windows)
		cls := "loss"
		if stratAvg > btcAvg {
			cls = "gain"
		}
		fmt.Fprintf(b, "<tr><td>%s</td><td>%d</td><td>%s</td><td>%s</td><td class=\"%s\">%s</td></tr>\n",
			html.EscapeString(string(regime)), bk.windows, pct(stratAvg), pct(btcAvg), cls, pct(stratAvg-btcAvg))
	}
	b.WriteString("</tbody></table></section>\n")
}

// --- Pencere bazlı detay ----------------------------------------------------

func writeWFWindowsSection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Pencereler</h2>\n<table data-sortable><thead><tr>")
	for _, h := range []string{"Eğitim", "Test", "Rejim", "Seçilen Parametreler", "Eğitim CAGR", "Test Getirisi", "Test BTC", "İşlem"} {
		fmt.Fprintf(b, "<th>%s</th>", h)
	}
	b.WriteString("</tr></thead><tbody>\n")
	for _, w := range d.Windows {
		stratRet, btcRet := 0.0, 0.0
		if len(w.TestEquity) > 0 {
			stratRet = w.TestEquity[len(w.TestEquity)-1].Equity/w.TestEquity[0].Equity - 1
		}
		if len(w.TestBenchBTC) > 0 {
			btcRet = w.TestBenchBTC[len(w.TestBenchBTC)-1].Equity/w.TestBenchBTC[0].Equity - 1
		}
		fmt.Fprintf(b, "<tr><td>%s → %s</td><td>%s → %s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td></tr>\n",
			w.Window.TrainStart.Format("2006-01-02"), w.Window.TrainEnd.Format("2006-01-02"),
			w.Window.TestStart.Format("2006-01-02"), w.Window.TestEnd.Format("2006-01-02"),
			html.EscapeString(string(w.Regime)),
			html.EscapeString(formatParamSet(w.ChosenParams)),
			pct(w.TrainMetrics.CAGR), pct(stratRet), pct(btcRet), len(w.TestTrades),
		)
	}
	b.WriteString("</tbody></table></section>\n")
}

func formatParamSet(ps map[string]float64) string {
	if len(ps) == 0 {
		return "(varsayılan)"
	}
	keys := make([]string, 0, len(ps))
	for k := range ps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%.4g", k, ps[k])
	}
	return strings.Join(parts, ", ")
}

// --- Parametre duyarlılığı ısı haritası (SPEC.md Bölüm 11.3) --------------

func writeWFSensitivitySection(b *strings.Builder, d WalkForwardReportData) {
	b.WriteString("<section><h2>Parametre Duyarlılığı (Bölüm 11.3)</h2>\n")
	b.WriteString("<div class=\"meta\">Aradığın şey bir tepe değil, bir plato — tek bir keskin en iyi değer tesadüf olabilir.</div>\n")
	if len(d.Sensitivity) == 0 {
		b.WriteString("<div class=\"meta\">Duyarlılık taraması koşulmadı.</div></section>\n")
		return
	}

	plateauByParam := make(map[string]backtest.PlateauVerdict, len(d.Plateaus))
	for _, p := range d.Plateaus {
		plateauByParam[p.Param] = p
	}

	order, byParam := backtest.GroupByParam(d.Sensitivity)
	for _, param := range order {
		pts := byParam[param]
		sort.Slice(pts, func(i, j int) bool { return pts[i].Value < pts[j].Value })

		verdict, hasVerdict := plateauByParam[param]
		fmt.Fprintf(b, "<h3 style=\"font-size:13px;letter-spacing:0.04em;margin-top:20px;\">%s</h3>\n", html.EscapeString(param))
		if hasVerdict {
			cls, label := "gain", "PLATO"
			if !verdict.IsPlateau {
				cls, label = "pending", "KESKİN TEPE — dikkat"
			}
			fmt.Fprintf(b, "<div class=\"%s\">%s: %s</div>\n", cls, label, html.EscapeString(verdict.Detail))
		}

		b.WriteString("<table><thead><tr><th>Değer</th><th>Sharpe</th><th>Calmar</th><th>CAGR</th><th>Maks. Düşüş</th><th>İşlem</th></tr></thead><tbody>\n")
		for _, p := range pts {
			cls := ""
			if hasVerdict && p.Value == verdict.BestValue {
				cls = " style=\"font-weight:700\""
			}
			fmt.Fprintf(b, "<tr%s><td>%.4g</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td></tr>\n",
				cls, p.Value, num(p.Metrics.Sharpe), num(p.Metrics.Calmar), pct(p.Metrics.CAGR), pct(p.Metrics.MaxDrawdown), p.Metrics.TradeCount)
		}
		b.WriteString("</tbody></table>\n")
	}
	b.WriteString("</section>\n")
}
