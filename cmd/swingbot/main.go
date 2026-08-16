// Command swingbot is the CLI entry point for the crypto swing trading bot
// described in SPEC.md. This file (owned by Ajan 14 — CLI ve Entegrasyon
// Sorumlusu, .claude/agents/cli-integration-lead.md) wires together the
// cobra command tree from SPEC.md Bölüm 9 and dispatches into the packages
// other agents own. It intentionally contains NO strategy/risk/broker
// business logic of its own — see the "Bağlayıcı kurallar" section of that
// agent's brief.
//
// As of 2026-08-16 the following packages are implemented and consumed
// here: internal/config (root command's config load), internal/datafeed
// (backs `data backfill|update|verify`, via internal/store and
// internal/exchange to construct its dependencies), internal/universe
// (backs `universe show`, see universe.go in this package — owned by
// universe-scoring-engineer/Ajan 8), and indirectly internal/domain via all
// of the above. The following packages do not exist yet: internal/strategy,
// internal/risk, internal/broker, internal/backtest, internal/engine,
// internal/notify, internal/web. Every subcommand whose backing package is
// missing is registered as a real cobra command (so `--help` reports it
// correctly and the command tree matches SPEC.md Bölüm 9 exactly) but its
// RunE just reports which package/agent it is waiting on and exits 1 — see
// notImplemented below. As each agent lands its package, swap the matching
// RunE for a real call into that package; do not add new logic here beyond
// the call itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	"swingbot/internal/backtest"
	"swingbot/internal/broker"
	"swingbot/internal/config"
	"swingbot/internal/datafeed"
	"swingbot/internal/domain"
	"swingbot/internal/exchange"
	"swingbot/internal/store"
	"swingbot/internal/strategy"
	"swingbot/internal/web"
)

// swingbotVersion is a plain constant for now (Faz 0). If/when this needs to
// track build info (git commit, build date via -ldflags), that is a small,
// self-contained change to this file alone — still no new package required.
const swingbotVersion = "swingbot v0.1.0-dev"

// configPath/envPath back the root command's persistent --config/--env
// flags. cfg is populated by the root PersistentPreRunE once config.Load
// succeeds, so subcommands that gain a real implementation later can read
// it directly instead of reloading it themselves.
var (
	configPath string
	envPath    string
	cfg        *config.Config
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// cobra has already printed the error (SilenceUsage suppresses the
		// noisy usage dump but not the error line itself); we only need to
		// turn it into a process exit code.
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "swingbot",
		Short: "Kripto swing trading botu için CLI (SPEC.md)",
		Long: `swingbot, SPEC.md'de tanımlanan kripto swing trading botunun komut satırı arayüzüdür.

Aynı strateji/risk/broker kodu backtest, paper ve live modlarda değişmeden
çalışır (SPEC.md İ1) — bu araç yalnızca komutları ilgili paketlere bağlar.
Henüz yazılmamış katmanlara bağlı komutlar, o katman tamamlanana kadar
"henüz implemente edilmedi" hatası ile netleştirilir; sonuç uydurulmaz.`,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// version amaçlı olarak config'e ihtiyaç duymaz: config.yaml
			// henüz kopyalanmamış bir ortamda bile "swingbot version"
			// çalışabilmeli.
			if cmd.Name() == "version" {
				return nil
			}

			loaded, warnings, err := config.Load(configPath, envPath)
			if err != nil {
				return fmt.Errorf(
					"yapılandırma yüklenemedi (--config=%s): %w\nİpucu: config.example.yaml dosyasını %s olarak kopyalayıp kendi değerlerinizle doldurun (SPEC.md Bölüm 8).",
					configPath, err, configPath,
				)
			}
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "uyarı: %s\n", w)
			}
			cfg = loaded
			return nil
		},
	}

	root.PersistentFlags().StringVar(&configPath, "config", "config.yaml", "yapılandırma dosyası yolu (SPEC.md Bölüm 8)")
	root.PersistentFlags().StringVar(&envPath, "env", ".env", "sırların (API anahtarları, telegram token) olduğu .env dosyası yolu")

	root.AddCommand(
		newDataCmd(),
		newUniverseCmd(),
		newBacktestCmd(),
		newWalkforwardCmd(),
		newPaperCmd(),
		newLiveCmd(),
		newServeCmd(),
		newBreakerCmd(),
		newReportCmd(),
		newPositionsCmd(),
		newVersionCmd(),
	)

	return root
}

// notImplemented builds a RunE for a subcommand whose backing package does
// not exist yet in this repo. It reports exactly what is missing (which
// package, which agent per .claude/agents/) and exits 1 — it never
// fabricates a result. what is the human-readable command name (e.g. "data
// backfill"); pkg is the internal/... package(s) the command needs; owner
// names the responsible sub-agent so a reader knows who to wait on.
func notImplemented(what, pkg, owner string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf(
			"henüz implemente edilmedi: %q komutu %s paketini gerektiriyor (sorumlu: %s, bkz. SPEC.md Bölüm 3/9). O paket tamamlanana kadar bu komut sadece bu mesajı basar.",
			what, pkg, owner,
		)
	}
}

// --- data backfill|update|verify ---------------------------------------
//
// These three RunE functions are the first commands wired to a real
// backing package (internal/datafeed, landed 2026-08-15). Per this agent's
// "Bağlayıcı kurallar" (.claude/agents/cli-integration-lead.md) they
// contain no business logic of their own: newExchangeFromConfig/openFeed
// below only translate already-validated *config.Config fields into the
// constructor calls internal/store and internal/exchange expect, and each
// RunE is just flag-parsing + a call into datafeed.Feed + printing the
// report that call returns.

// newExchangeFromConfig builds the exchange.Exchange implementation named
// by cfg.Exchange.Name (SPEC.md Bölüm 8). internal/exchange currently only
// implements Binance (see internal/exchange/ccxt.go's file-level doc
// comment for why it talks to Binance directly instead of importing a CCXT
// client) — any other configured name is a config error, not a silent
// fallback to Binance.
func newExchangeFromConfig(c *config.Config) (exchange.Exchange, error) {
	switch c.Exchange.Name {
	case "", "binance":
		return exchange.NewBinanceExchange(exchange.BinanceConfig{
			APIKey:          c.Secrets.ExchangeAPIKey,
			APISecret:       c.Secrets.ExchangeAPISecret,
			RateLimitPerMin: c.Exchange.RateLimitPerMin,
		}), nil
	default:
		return nil, fmt.Errorf(
			"desteklenmeyen exchange.name: %q (şu anda yalnızca \"binance\" destekleniyor, bkz. SPEC.md Bölüm 5.2/8 ve internal/exchange/ccxt.go)",
			c.Exchange.Name,
		)
	}
}

// openFeed opens the store at cfg.Data.DBPath and wires it together with
// the configured exchange into a *datafeed.Feed, ready for
// Backfill/Update/Verify. Callers must Close() the returned *store.Store
// once done (typically via defer).
func openFeed(ctx context.Context) (*datafeed.Feed, *store.Store, error) {
	ex, err := newExchangeFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}

	// SPEC.md Bölüm 8's data.db_path (e.g. "./data/swingbot.db") assumes
	// its parent directory exists; a fresh checkout ships one (data/ with
	// a .gitkeep), but a custom --config pointing elsewhere should not
	// fail with an opaque sqlite "unable to open database file" error.
	if dir := filepath.Dir(cfg.Data.DBPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("veritabanı dizini oluşturulamadı (%s): %w", dir, err)
		}
	}

	st, err := store.Open(ctx, cfg.Data.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("veritabanı açılamadı (data.db_path=%s): %w", cfg.Data.DBPath, err)
	}

	feed := datafeed.NewFeed(ex, st, cfg.Data.Timeframe,
		datafeed.WithQuoteFilter(cfg.Exchange.Quote),
		datafeed.WithBackfillYears(cfg.Data.BackfillYears),
	)
	return feed, st, nil
}

// printSymbolReports prints one line per symbol from a Backfill/Update run
// plus a summary line, so a human running the command interactively can see
// progress and quality issues without needing `data verify` afterwards.
func printSymbolReports(action string, symbols []datafeed.SymbolReport, delisted []string) {
	var totalCandles, totalIssues, totalCritical int
	for _, sr := range symbols {
		critical := 0
		for _, iss := range sr.Issues {
			if iss.Severity == datafeed.SeverityCritical {
				critical++
			}
		}
		fmt.Printf("%-14s %-6d sayfa  %-8d mum yazıldı  %-4d kalite bulgusu (%d kritik)\n",
			sr.Symbol, sr.Pages, sr.CandlesWritten, len(sr.Issues), critical)
		totalCandles += sr.CandlesWritten
		totalIssues += len(sr.Issues)
		totalCritical += critical
	}
	if len(delisted) > 0 {
		fmt.Printf("delisted olarak işaretlendi: %v\n", delisted)
	}
	fmt.Printf(
		"\n%s tamamlandı: %d sembol, %d mum yazıldı, %d kalite bulgusu (%d kritik).\n",
		action, len(symbols), totalCandles, totalIssues, totalCritical,
	)
}

func runDataBackfill(cmd *cobra.Command, args []string) error {
	symbols, err := cmd.Flags().GetStringSlice("symbols")
	if err != nil {
		return err
	}
	years, err := cmd.Flags().GetInt("years")
	if err != nil {
		return err
	}

	feed, st, err := openFeed(cmd.Context())
	if err != nil {
		return err
	}
	defer st.Close()

	report, err := feed.Backfill(cmd.Context(), datafeed.BackfillOptions{Symbols: symbols, Years: years})
	if report != nil {
		printSymbolReports("backfill", report.Symbols, report.Delisted)
	}
	if err != nil {
		return fmt.Errorf("data backfill: %w", err)
	}
	return nil
}

func runDataUpdate(cmd *cobra.Command, args []string) error {
	feed, st, err := openFeed(cmd.Context())
	if err != nil {
		return err
	}
	defer st.Close()

	report, err := feed.Update(cmd.Context())
	if report != nil {
		printSymbolReports("update", report.Symbols, report.Delisted)
	}
	if err != nil {
		return fmt.Errorf("data update: %w", err)
	}
	return nil
}

func runDataVerify(cmd *cobra.Command, args []string) error {
	feed, st, err := openFeed(cmd.Context())
	if err != nil {
		return err
	}
	defer st.Close()

	report, err := feed.Verify(cmd.Context(), nil)
	if err != nil {
		return fmt.Errorf("data verify: %w", err)
	}

	for _, sr := range report.Symbols {
		critical := 0
		for _, iss := range sr.Issues {
			if iss.Severity == datafeed.SeverityCritical {
				critical++
			}
		}
		fmt.Printf("%-14s %-8d mum  %-4d bulgu (%d kritik)\n", sr.Symbol, sr.CandleCount, len(sr.Issues), critical)
	}
	for _, w := range report.Warnings {
		fmt.Printf("uyarı: %s\n", w)
	}
	fmt.Printf(
		"\nverify tamamlandı (%s): %d sembol, %d bulgu, %d kritik.\n",
		report.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"), len(report.Symbols), report.TotalIssues, report.CriticalIssues,
	)

	if !report.OK {
		return fmt.Errorf("data verify: %d kritik hata bulundu (SPEC.md Bölüm 6.1 kabul kriteri: sıfır kritik hata)", report.CriticalIssues)
	}
	fmt.Println("OK: kritik hata yok.")
	return nil
}

func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Piyasa verisi toplama ve kalite kontrolü (internal/datafeed)",
	}

	backfill := &cobra.Command{
		Use:   "backfill",
		Short: "Geçmiş OHLCV verisini borsadan çekip DB'ye yazar",
		RunE:  runDataBackfill,
	}
	backfill.Flags().StringSlice("symbols", nil, "geri doldurulacak semboller (boşsa: evrendeki tüm semboller)")
	backfill.Flags().Int("years", 3, "kaç yıl geriye gidileceği (<=0: config.yaml'daki data.backfill_years)")

	update := &cobra.Command{
		Use:   "update",
		Short: "En güncel kapanmış mumlarla artımlı, idempotent güncelleme yapar",
		RunE:  runDataUpdate,
	}

	verify := &cobra.Command{
		Use:   "verify",
		Short: "Veri kalite kontrollerini çalıştırır (eksik mum, aykırı değer, süreklilik, vb.)",
		RunE:  runDataVerify,
	}

	cmd.AddCommand(backfill, update, verify)
	return cmd
}

// --- universe show -------------------------------------------------------

func newUniverseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "universe",
		Short: "İşlem yapılabilir evren filtresi ve kesitsel skorlama (internal/universe)",
	}

	show := &cobra.Command{
		Use:   "show",
		Short: "Belirli bir tarihteki evreni ve skor bileşenlerini gösterir",
		RunE:  runUniverseShow,
	}
	show.Flags().String("date", "", "evrenin hesaplanacağı tarih, YYYY-MM-DD (varsayılan: bugün)")

	cmd.AddCommand(show)
	return cmd
}

// --- backtest / walkforward ----------------------------------------------
//
// backtest is the first command wired to internal/backtest (landed
// 2026-08-16 by backtest-engine-architect, Ajan 6) and internal/web's
// report generator. Everything below it — loading candles from
// internal/store, building broker.Costs from config, running the engine,
// computing benchmarks, writing the HTML report and the runs row — is
// real. The one piece still missing is resolveStrategy: internal/strategy
// only has the Strategy interface so far (also Ajan 6, so the engine had
// something to compile against) — momentum.go/trendfollow.go are
// strategy-developer's (Ajan 7). Once those land, resolveStrategy is the
// only function in this file that needs to change.

func newBacktestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backtest",
		Short: "Geçmiş veri üzerinde bir stratejiyi koşturup HTML rapor üretir",
		RunE:  runBacktest,
	}
	cmd.Flags().String("strategy", "", "koşturulacak strateji (config.yaml'daki strategy.active'i geçersiz kılar)")
	cmd.Flags().String("from", "", "başlangıç tarihi, YYYY-MM-DD")
	cmd.Flags().String("to", "", "bitiş tarihi, YYYY-MM-DD")
	cmd.Flags().Float64("capital", 10000, "başlangıç sermayesi (quote para birimi, örn. USDT) — SPEC.md config şemasında henüz bir alanı yok")
	cmd.Flags().StringArray("config-override", nil, "config.yaml alanını geçersiz kılar, örn. strategy.trendfollow.atr_stop_mult=3.0 (tekrarlanabilir)")
	return cmd
}

// resolveStrategy is the one seam in this command that has nothing real
// to call yet. It fails loudly and specifically rather than silently
// falling back to anything, per this file's "sonuç uydurulmaz" rule.
func resolveStrategy(name string) (strategy.Strategy, error) {
	switch name {
	case "":
		return nil, fmt.Errorf("strateji belirtilmedi: --strategy bayrağını kullanın veya config.yaml'daki strategy.active'i doldurun")
	case "momentum", "trendfollow":
		return nil, fmt.Errorf(
			"henüz implemente edilmedi: %q stratejisi internal/strategy paketini gerektiriyor (sorumlu: strategy-developer (Ajan 7), bkz. SPEC.md Bölüm 6.4/9). "+
				"internal/strategy şu an yalnızca Strategy arayüzünü içeriyor (backtest-engine-architect, Ajan 6).", name)
	default:
		return nil, fmt.Errorf("bilinmeyen strateji: %q (bilinenler: momentum, trendfollow)", name)
	}
}

// marketsBySymbol converts store rows to domain.Market, for the risk
// sizer's step/min-notional rounding (SPEC.md Bölüm 6.5.1). A market with
// an unparsable decimal field is skipped with a warning rather than
// aborting the whole backtest.
func marketsBySymbol(rows []store.Market) (map[string]domain.Market, []string) {
	out := make(map[string]domain.Market, len(rows))
	var warnings []string
	for _, r := range rows {
		tick, err1 := decimal.NewFromString(r.TickSize)
		step, err2 := decimal.NewFromString(r.StepSize)
		minNotional, err3 := decimal.NewFromString(r.MinNotional)
		if err1 != nil || err2 != nil || err3 != nil {
			warnings = append(warnings, fmt.Sprintf("piyasa filtresi ayrıştırılamadı, atlanıyor: %s", r.Symbol))
			continue
		}
		out[r.Symbol] = domain.Market{
			Symbol: r.Symbol, Base: r.Base, Quote: r.Quote, Active: r.Active,
			TickSize: tick, StepSize: step, MinNotional: minNotional,
			ListedAt: r.ListedAt, DelistedAt: r.DelistedAt,
		}
	}
	return out, warnings
}

func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runBacktest(cmd *cobra.Command, args []string) error {
	strategyName, err := cmd.Flags().GetString("strategy")
	if err != nil {
		return err
	}
	if strategyName == "" {
		strategyName = cfg.Strategy.Active
	}
	strat, err := resolveStrategy(strategyName)
	if err != nil {
		return err
	}

	fromStr, _ := cmd.Flags().GetString("from")
	toStr, _ := cmd.Flags().GetString("to")
	capital, err := cmd.Flags().GetFloat64("capital")
	if err != nil {
		return err
	}
	var from, to time.Time
	if fromStr != "" {
		if from, err = time.Parse("2006-01-02", fromStr); err != nil {
			return fmt.Errorf("--from ayrıştırılamadı: %w", err)
		}
	}
	if toStr != "" {
		if to, err = time.Parse("2006-01-02", toStr); err != nil {
			return fmt.Errorf("--to ayrıştırılamadı: %w", err)
		}
	}

	ctx := cmd.Context()
	st, err := store.Open(ctx, cfg.Data.DBPath)
	if err != nil {
		return fmt.Errorf("veritabanı açılamadı (data.db_path=%s): %w", cfg.Data.DBPath, err)
	}
	defer st.Close()

	rows, err := st.ListMarkets(ctx, false)
	if err != nil {
		return fmt.Errorf("piyasa listesi okunamadı: %w", err)
	}
	markets, marketWarnings := marketsBySymbol(rows)

	candles := make(map[string][]domain.Candle, len(rows))
	for _, r := range rows {
		c, err := st.GetCandles(ctx, r.Symbol, cfg.Data.Timeframe, from, to)
		if err != nil {
			return fmt.Errorf("mumlar okunamadı (%s): %w", r.Symbol, err)
		}
		if len(c) > 0 {
			candles[r.Symbol] = c
		}
	}
	if len(candles) == 0 {
		return fmt.Errorf("veritabanında mum verisi yok — önce `swingbot data backfill` çalıştırın (data.db_path=%s)", cfg.Data.DBPath)
	}

	costs := backtest.CostsFromConfig(cfg.Costs)
	result, err := backtest.Run(ctx, backtest.Config{
		Strategy:    strat,
		Candles:     candles,
		InitialCash: capital,
		Costs:       costs,
		RiskGate:    backtest.NewSimpleRiskGate(cfg.Risk, markets),
		Mode:        "backtest",
	})
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	result.Warnings = append(result.Warnings, marketWarnings...)

	calendar := make([]time.Time, len(result.Equity))
	for i, p := range result.Equity {
		calendar[i] = p.Date
	}
	btcSymbol := "BTC/" + cfg.Exchange.Quote
	btcCurve := backtest.BuyAndHoldCurve(candles[btcSymbol], calendar, capital, costs)
	if btcCurve == nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("İ3: %s için veri yok, BTC al-tut benchmark'ı hesaplanamadı", btcSymbol))
	}
	top10Curve := backtest.Top10EqualWeightCurve(candles, calendar, capital, costs)
	btcMetrics := backtest.Compute(btcCurve, nil)
	top10Metrics := backtest.Compute(top10Curve, nil)
	result.Metrics.BenchBTC = &btcMetrics
	result.Metrics.BenchTop10 = &top10Metrics

	reportPath, err := web.WriteReportFile("reports", web.ReportData{
		Strategy: result.Strategy, Params: result.Params,
		Start: result.Start, End: result.End, Costs: costs,
		GitSHA: gitSHA(), GeneratedAt: time.Now().UTC(),
		Warnings: result.Warnings,
		Metrics:  result.Metrics, Equity: result.Equity,
		BenchBTC: btcCurve, BenchTop10: top10Curve,
		Trades: result.Trades, Rejections: result.Rejections,
	})
	if err != nil {
		return fmt.Errorf("rapor üretilemedi: %w", err)
	}

	paramsJSON, _ := json.Marshal(result.Params)
	metricsJSON, _ := json.Marshal(result.Metrics)
	runID := fmt.Sprintf("bt-%s", time.Now().UTC().Format("20060102-150405.000"))
	if err := st.InsertRun(ctx, store.Run{
		ID: runID, CreatedAt: time.Now().UTC(), Strategy: result.Strategy,
		ParamsJSON: string(paramsJSON), StartTS: result.Start, EndTS: result.End,
		CostsJSON: mustCostsJSON(costs), MetricsJSON: string(metricsJSON),
		ReportPath: reportPath, GitSHA: gitSHA(),
	}); err != nil {
		return fmt.Errorf("koşum kaydedilemedi: %w", err)
	}

	fmt.Printf("backtest tamamlandı: %s (%s → %s)\n", result.Strategy, result.Start.Format("2006-01-02"), result.End.Format("2006-01-02"))
	fmt.Printf("toplam getiri: %.2f%%  |  BTC al-tut: %.2f%%  |  maks. düşüş: %.2f%%  |  işlem sayısı: %d\n",
		result.Metrics.TotalReturn*100, btcMetrics.TotalReturn*100, result.Metrics.MaxDrawdown*100, result.Metrics.TradeCount)
	fmt.Printf("rapor: %s\nrun id: %s\n", reportPath, runID)
	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "uyarı: %s\n", w)
	}
	return nil
}

func mustCostsJSON(c broker.Costs) string {
	s, err := backtest.CostsJSON(c)
	if err != nil {
		return "{}"
	}
	return s
}

func newWalkforwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "walkforward",
		Short: "Kayan eğitim/test pencereleriyle walk-forward doğrulama koşturur",
		RunE:  notImplemented("walkforward", "internal/backtest (walkforward.go)", "validation-analysis-engineer (Ajan 10)"),
	}
	cmd.Flags().String("strategy", "", "koşturulacak strateji")
	cmd.Flags().Int("train", 365, "eğitim penceresi (gün)")
	cmd.Flags().Int("test", 90, "test penceresi (gün)")
	cmd.Flags().Int("step", 90, "pencere kayma adımı (gün)")
	return cmd
}

// --- paper start -----------------------------------------------------------

func newPaperCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "paper",
		Short: "Simüle sermaye ile günlük döngüyü çalıştırma (kağıt üzeri)",
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Günlük paper trading döngüsünü başlatır",
		RunE: notImplemented(
			"paper start",
			"internal/engine + internal/broker (paper.go)",
			"live-engine-notify-engineer (Ajan 11) / backtest-engine-architect (Ajan 6)",
		),
	}
	cmd.AddCommand(start)
	return cmd
}

// --- live start --confirm ---------------------------------------------

func newLiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "live",
		Short: "Gerçek sermaye ile günlük döngü — DİKKAT: gerçek emirler gönderir",
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Günlük live trading döngüsünü başlatır (yalnızca --confirm ile)",
		RunE:  runLiveStart,
	}
	// SPEC.md Bölüm 9: "`live start` komutu `--confirm` bayrağı olmadan
	// çalışmaz." Bu bayrağın zorunluluğu CLI sözleşmesinin bir parçası,
	// bu yüzden burada uygulanıyor. Onaydan önce basılması gereken tam
	// özet ekranı (mod/borsa/sermaye/risk/devre kesici/strateji) gerçek
	// sermaye/pozisyon verisine ihtiyaç duyar ve internal/broker/live.go
	// ile birlikte live-trading-security-engineer (Ajan 13) tarafından
	// kurulacaktır — burada uydurulmaz.
	start.Flags().Bool("confirm", false, "gerçek sermaye ile çalıştırmayı açıkça onaylar")
	cmd.AddCommand(start)
	return cmd
}

func runLiveStart(cmd *cobra.Command, args []string) error {
	confirm, err := cmd.Flags().GetBool("confirm")
	if err != nil {
		return err
	}
	if !confirm {
		return errors.New(
			"live start --confirm bayrağı olmadan çalışmaz (SPEC.md Bölüm 9). " +
				"Önce mod/borsa/sermaye/risk/devre kesici/strateji ayarlarınızı config.yaml üzerinden gözden geçirin, ardından `swingbot live start --confirm` ile tekrar çalıştırın.",
		)
	}
	return fmt.Errorf(
		"henüz implemente edilmedi: %q komutu %s paketini gerektiriyor (sorumlu: %s, bkz. SPEC.md Bölüm 3/9). O paket tamamlanana kadar bu komut sadece bu mesajı basar.",
		"live start", "internal/broker/live.go + internal/engine", "live-trading-security-engineer (Ajan 13) / live-engine-notify-engineer (Ajan 11)",
	)
}

// --- serve ---------------------------------------------------------------

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Yerel salt-okunur web panelini başlatır (yalnızca 127.0.0.1)",
		RunE:  notImplemented("serve", "internal/web", "panel-developer (Ajan 12)"),
	}
}

// --- breaker status|reset -------------------------------------------------

func newBreakerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "breaker",
		Short: "Devre kesici durumu ve sıfırlama (internal/risk)",
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Devre kesicinin güncel durumunu ve tetikleyen koşulu gösterir",
		RunE:  notImplemented("breaker status", "internal/risk (breaker.go)", "risk-management-engineer (Ajan 9)"),
	}

	reset := &cobra.Command{
		Use:   "reset",
		Short: "Devre kesiciyi sıfırlar (yalnızca --confirm ile, dikkatli kullanın)",
		RunE:  notImplemented("breaker reset", "internal/risk (breaker.go)", "risk-management-engineer (Ajan 9)"),
	}
	reset.Flags().Bool("confirm", false, "sıfırlamayı açıkça onaylar")

	cmd.AddCommand(status, reset)
	return cmd
}

// --- report / positions ---------------------------------------------------

func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Belirli bir koşum (run) için tek dosyalık HTML raporu üretir",
		RunE: notImplemented(
			"report",
			"internal/web (report.go) + internal/store",
			"panel-developer (Ajan 12) / backtest-engine-architect (Ajan 6)",
		),
	}
	cmd.Flags().String("run", "", "rapor üretilecek koşum kimliği")
	return cmd
}

func newPositionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "positions",
		Short: "Açık pozisyonları listeler",
		RunE: notImplemented(
			"positions",
			"internal/store + internal/engine",
			"storage-engineer (Ajan 2) / live-engine-notify-engineer (Ajan 11)",
		),
	}
}

// --- version ---------------------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "swingbot sürümünü yazdırır",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(swingbotVersion)
			return nil
		},
	}
}
