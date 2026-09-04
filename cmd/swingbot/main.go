// Command swingbot is the CLI entry point for the crypto swing trading bot
// described in SPEC.md. This file (owned by Ajan 14 — CLI ve Entegrasyon
// Sorumlusu, .claude/agents/cli-integration-lead.md) wires together the
// cobra command tree from SPEC.md Bölüm 9 and dispatches into the packages
// other agents own. It intentionally contains NO strategy/risk/broker
// business logic of its own — see the "Bağlayıcı kurallar" section of that
// agent's brief.
//
// As of 2026-09-02 the following packages are implemented and consumed
// here: internal/config (root command's config load), internal/datafeed
// (backs `data backfill|update|verify`), internal/universe (backs
// `universe show`, see universe.go in this package — Ajan 8),
// internal/strategy (momentum + trendfollow — Ajan 7), internal/risk
// (gate/sizer/breaker — Ajan 9), internal/broker + internal/backtest +
// internal/web (backs `backtest` — Ajan 6), internal/engine + internal/notify
// (backs `paper start`, see paper.go in this package — Ajan 11), and
// indirectly internal/domain via all of the above. internal/broker/live.go
// does not exist yet (backs `live start` — Ajan 13). Every subcommand whose
// backing package is missing is registered as a real cobra command (so
// `--help` reports it
// correctly and the command tree matches SPEC.md Bölüm 9 exactly) but its
// RunE just reports which package/agent it is waiting on and exits 1 — see
// notImplemented below. As each agent lands its package, swap the matching
// RunE for a real call into that package; do not add new logic here beyond
// the call itself. NOTE: keep this list current — a stale claim here
// ("X doesn't exist yet") is exactly what let backtest's resolveStrategy
// and its risk-gate wiring silently rot after Ajan 7/9 landed (2026-08-16
// review finding).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	"swingbot/internal/backtest"
	"swingbot/internal/broker"
	"swingbot/internal/config"
	"swingbot/internal/datafeed"
	"swingbot/internal/domain"
	"swingbot/internal/exchange"
	"swingbot/internal/risk"
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
// backtest is wired end to end (2026-08-16): candles come from
// internal/store, costs/risk from internal/config via internal/backtest
// and internal/risk (Gate/Sizer/Breaker), the strategy from
// resolveStrategy (internal/strategy's momentum/trendfollow), and the
// result goes to internal/web's report generator plus a runs row.

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

// resolveStrategy builds the strategy.Strategy named by name from cfg's
// strategy.trendfollow/strategy.momentum blocks (SPEC.md Bölüm 8),
// overlaying config.yaml's values on top of each strategy's package
// defaults so a config.yaml that omits (or zeroes) a field still gets a
// sane WarmupBars/Evaluate instead of silently breaking on a zero.
func resolveStrategy(name string, cfg *config.Config) (strategy.Strategy, error) {
	switch name {
	case "":
		return nil, fmt.Errorf("strateji belirtilmedi: --strategy bayrağını kullanın veya config.yaml'daki strategy.active'i doldurun")
	case "trendfollow":
		return strategy.NewTrendfollow(trendfollowParamsFromConfig(cfg.Strategy.Trendfollow)), nil
	case "momentum":
		return strategy.NewMomentum(momentumParamsFromConfig(cfg.Strategy.Momentum)), nil
	default:
		return nil, fmt.Errorf("bilinmeyen strateji: %q (bilinenler: momentum, trendfollow)", name)
	}
}

// trendfollowParamsFromConfig overlays config.yaml's strategy.trendfollow
// block onto DefaultTrendfollowParams(). Every TrendfollowParams field has
// a matching config.TrendfollowConfig field, so a config value is used
// whenever it is set (non-zero) and the package default fills any gap.
func trendfollowParamsFromConfig(c config.TrendfollowConfig) strategy.TrendfollowParams {
	p := strategy.DefaultTrendfollowParams()
	if c.SMALong > 0 {
		p.SMALong = c.SMALong
	}
	if c.SMAExit > 0 {
		p.SMAExit = c.SMAExit
	}
	if c.BreakoutLookback > 0 {
		p.BreakoutLookback = c.BreakoutLookback
	}
	if c.ATRPeriod > 0 {
		p.ATRPeriod = c.ATRPeriod
	}
	if c.ATRStopMult > 0 {
		p.ATRStopMult = c.ATRStopMult
	}
	if c.MaxATRPct > 0 {
		p.MaxATRPct = c.MaxATRPct
	}
	return p
}

// momentumParamsFromConfig overlays config.yaml's strategy.momentum block
// onto DefaultMomentumParams(). NOTE: config.MomentumConfig (SPEC.md Bölüm
// 8) only exposes top_n/exit_rank/rebalance_weekday/weights — it has no
// fields for the ATR stop or the mom_90/mom_30/vol_30/liq lookback
// windows, so those always come from the package default regardless of
// config.yaml (a config-schema gap, domain-config-architect/Ajan 1
// territory, not something to paper over here with invented config keys).
func momentumParamsFromConfig(c config.MomentumConfig) strategy.MomentumParams {
	p := strategy.DefaultMomentumParams()
	if c.TopN > 0 {
		p.TopN = c.TopN
	}
	if c.ExitRank > 0 {
		p.ExitRank = c.ExitRank
	}
	// RebalanceWeekday's zero value (Sunday) is indistinguishable from
	// "not set" on a plain int, unlike every other field here — a
	// config.yaml that omits the whole strategy.momentum block entirely
	// must still fall back to DefaultMomentumParams()'s Monday, not
	// silently become Sunday. Only honor c.RebalanceWeekday (including an
	// explicit Sunday) once some other momentum field shows the block was
	// actually provided.
	if c.TopN > 0 || c.ExitRank > 0 || len(c.Weights) > 0 || c.RebalanceWeekday != 0 {
		p.RebalanceWeekday = time.Weekday(c.RebalanceWeekday)
	}
	if len(c.Weights) > 0 {
		p.Weights = c.Weights
	}
	return p
}

// applyConfigOverrides applies each "--config-override path=value" flag
// in order, mutating cfg in place. Scope is deliberately narrow — only
// numeric fields under strategy.trendfollow.* / strategy.momentum.* (SPEC.md
// Bölüm 9's own example is exactly this: "strategy.trendfollow.atr_stop_mult=3.0"),
// which is what parameter-sensitivity testing (SPEC.md Bölüm 11.3) actually
// needs. It does not open arbitrary Config mutation (e.g. mode, secrets,
// exchange) to a CLI string flag.
func applyConfigOverrides(cfg *config.Config, overrides []string) error {
	for _, kv := range overrides {
		path, valStr, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--config-override: %q formatı geçersiz, beklenen path=value (örn. strategy.trendfollow.atr_stop_mult=3.0)", kv)
		}
		parts := strings.Split(path, ".")
		if len(parts) != 3 || parts[0] != "strategy" {
			return fmt.Errorf("--config-override: %q desteklenmiyor (şu an yalnızca strategy.trendfollow.<alan> veya strategy.momentum.<alan> destekleniyor)", path)
		}

		var target reflect.Value
		switch parts[1] {
		case "trendfollow":
			target = reflect.ValueOf(&cfg.Strategy.Trendfollow).Elem()
		case "momentum":
			target = reflect.ValueOf(&cfg.Strategy.Momentum).Elem()
		default:
			return fmt.Errorf("--config-override: %q desteklenmiyor (bilinen strateji: trendfollow, momentum)", path)
		}
		if err := setNumericFieldByYAMLTag(target, parts[2], valStr); err != nil {
			return fmt.Errorf("--config-override %q: %w", kv, err)
		}
	}
	return nil
}

// setNumericFieldByYAMLTag finds the int or float64 field of the struct v
// whose `yaml:"..."` tag equals yamlName and sets it from valStr. v must
// be an addressable struct value (reflect.ValueOf(&x).Elem()).
func setNumericFieldByYAMLTag(v reflect.Value, yamlName, valStr string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("yaml") != yamlName {
			continue
		}
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Int:
			n, err := strconv.Atoi(valStr)
			if err != nil {
				return fmt.Errorf("%q bir tam sayı değil: %w", valStr, err)
			}
			fv.SetInt(int64(n))
		case reflect.Float64:
			f, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return fmt.Errorf("%q bir ondalık sayı değil: %w", valStr, err)
			}
			fv.SetFloat(f)
		default:
			return fmt.Errorf("alan %q sayısal değil (tür: %s), --config-override ile değiştirilemez", yamlName, fv.Kind())
		}
		return nil
	}
	return fmt.Errorf("bilinmeyen alan %q", yamlName)
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
	overrides, err := cmd.Flags().GetStringArray("config-override")
	if err != nil {
		return err
	}
	if err := applyConfigOverrides(cfg, overrides); err != nil {
		return err
	}

	strategyName, err := cmd.Flags().GetString("strategy")
	if err != nil {
		return err
	}
	if strategyName == "" {
		strategyName = cfg.Strategy.Active
	}
	strat, err := resolveStrategy(strategyName, cfg)
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

	symbols := make([]string, len(rows))
	for i, r := range rows {
		symbols[i] = r.Symbol
	}
	candles, err := st.GetCandlesForSymbols(ctx, symbols, cfg.Data.Timeframe, from, to)
	if err != nil {
		return fmt.Errorf("mumlar okunamadı: %w", err)
	}
	if len(candles) == 0 {
		return fmt.Errorf("veritabanında mum verisi yok — önce `swingbot data backfill` çalıştırın (data.db_path=%s)", cfg.Data.DBPath)
	}

	costs := backtest.CostsFromConfig(cfg.Costs)
	result, err := backtest.Run(ctx, backtest.Config{
		Strategy:    strat,
		Candles:     candles,
		Markets:     markets,
		InitialCash: capital,
		Costs:       costs,
		RiskGate:    risk.NewGate(cfg.Risk, risk.NewSizer(cfg.Risk)),
		Breaker:     risk.NewBreaker(cfg.Breaker),
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

// newWalkforwardCmd backs `swingbot walkforward` (SPEC.md Bölüm 9/11):
// rolling train/test walk-forward validation, parameter-sensitivity
// plateau detection (Bölüm 11.3) and the Bölüm 11.4 go/no-go stamp, all
// wired into internal/backtest's walkforward.go/sensitivity.go/
// thresholds.go/locked.go (validation-analysis-engineer, Ajan 10).
//
// SPEC.md Bölüm 11.1's "yalnızca bir kez bakılır" rule for the locked
// segment is enforced here, not just documented: a plain (non---locked)
// run always operates on the DEVELOPMENT segment and, on its first
// invocation, permanently records the Bölüm 11.4 thresholds BEFORE any
// locked-segment data is ever touched; --locked refuses to run at all
// until that record exists, and refuses a SECOND look unless
// --force-reveal-again is passed (which then prints a loud, impossible-
// to-miss warning rather than silently complying).
func newWalkforwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "walkforward",
		Short: "Kayan eğitim/test pencereleriyle walk-forward doğrulama koşturur (SPEC.md Bölüm 11)",
		RunE:  runWalkforward,
	}
	cmd.Flags().String("strategy", "", "koşturulacak strateji (config.yaml'daki strategy.active'i geçersiz kılar)")
	cmd.Flags().Int("train", 365, "eğitim penceresi (gün, SPEC.md Bölüm 11.2 varsayılanı)")
	cmd.Flags().Int("test", 90, "test penceresi (gün)")
	cmd.Flags().Int("step", 90, "pencere kayma adımı (gün)")
	cmd.Flags().Float64("capital", 10000, "başlangıç sermayesi (quote para birimi)")
	cmd.Flags().StringArray("param", nil, "duyarlılık/arama parametresi: ad=v1,v2,v3 (tekrarlanabilir; SPEC.md Bölüm 11.3). Aynı değerler hem eğitim-penceresi arama ızgarasını hem de duyarlılık taramasını besler.")
	cmd.Flags().Int("min-trades", 50, "Bölüm 11.4 eşiği: minimum işlem sayısı (yalnızca eşikler İLK KEZ kaydedilirken kullanılır)")
	cmd.Flags().String("locked-start", "2025-07-01", "geliştirme/kilitli bölüm ayrım tarihi, YYYY-MM-DD (SPEC.md Bölüm 11.1)")
	cmd.Flags().Bool("locked", false, "KİLİTLİ bölümü görüntüle (SPEC.md Bölüm 11.1) — yalnızca BİR KEZ; eşikler önceden (--locked olmadan) kaydedilmiş olmalı")
	cmd.Flags().Bool("force-reveal-again", false, "kilitli bölüm zaten görüntülenmiş olsa bile yeniden görüntülemeyi KABUL EDER (bir disiplin ihlalidir, sessizce geçilmez)")
	return cmd
}

// walkforwardThresholdsStateKey persists the Bölüm 11.4 thresholds — the
// FIRST (non-locked) `swingbot walkforward` run on the development segment
// writes this once; every later run (dev or locked) reads it back rather
// than re-deriving it, so results can never retroactively change the bar
// they are graded against.
const walkforwardThresholdsStateKey = "walkforward_thresholds"

// walkforwardLockedViewStateKey persists backtest.LockedSegmentRecord —
// separate from walkforwardThresholdsStateKey because it records a
// DIFFERENT fact ("has the locked segment itself ever been looked at"),
// not "have thresholds been decided".
const walkforwardLockedViewStateKey = "walkforward_locked_view"

// storeLockedSegmentAdapter adapts *store.Store's generic GetState/SetState
// (SPEC.md Bölüm 4.1 system_state table) into backtest.LockedSegmentStore,
// the same pattern newBreakerCmd's RunE functions already use for
// risk.State via breakerStateKey.
type storeLockedSegmentAdapter struct{ st *store.Store }

func (a storeLockedSegmentAdapter) Load(ctx context.Context) (backtest.LockedSegmentRecord, bool, error) {
	raw, ok, err := a.st.GetState(ctx, walkforwardLockedViewStateKey)
	if err != nil || !ok {
		return backtest.LockedSegmentRecord{}, false, err
	}
	var rec backtest.LockedSegmentRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return backtest.LockedSegmentRecord{}, false, fmt.Errorf("locked segment kaydı ayrıştırılamadı: %w", err)
	}
	return rec, true, nil
}

func (a storeLockedSegmentAdapter) Save(ctx context.Context, rec backtest.LockedSegmentRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return a.st.SetState(ctx, walkforwardLockedViewStateKey, string(raw))
}

func loadWalkforwardThresholds(ctx context.Context, st *store.Store) (backtest.Thresholds, bool, error) {
	raw, ok, err := st.GetState(ctx, walkforwardThresholdsStateKey)
	if err != nil || !ok {
		return backtest.Thresholds{}, false, err
	}
	var t backtest.Thresholds
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return backtest.Thresholds{}, false, fmt.Errorf("kaydedilmiş eşikler ayrıştırılamadı: %w", err)
	}
	return t, true, nil
}

func saveWalkforwardThresholds(ctx context.Context, st *store.Store, t backtest.Thresholds) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return st.SetState(ctx, walkforwardThresholdsStateKey, string(raw))
}

// parseWalkforwardParamFlags turns repeated "--param ad=v1,v2,v3" flags
// into sensitivity/search axes plus a center ParamSet (the median value of
// each axis, used as ParamSensitivitySweep's BaseParams — SPEC.md Bölüm
// 11.3's one-parameter-at-a-time sweep needs every OTHER parameter pinned
// somewhere sensible while one moves).
func parseWalkforwardParamFlags(flags []string) ([]backtest.ParamAxis, backtest.ParamSet, error) {
	axes := make([]backtest.ParamAxis, 0, len(flags))
	base := backtest.ParamSet{}
	for _, f := range flags {
		name, valsStr, ok := strings.Cut(f, "=")
		if !ok {
			return nil, nil, fmt.Errorf("--param: %q formatı geçersiz, beklenen ad=v1,v2,v3 (örn. atr_stop_mult=2.0,2.5,3.0)", f)
		}
		name = strings.TrimSpace(name)
		var values []float64
		for _, vs := range strings.Split(valsStr, ",") {
			vs = strings.TrimSpace(vs)
			if vs == "" {
				continue
			}
			v, err := strconv.ParseFloat(vs, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("--param %q: %q sayısal değil: %w", name, vs, err)
			}
			values = append(values, v)
		}
		if len(values) == 0 {
			return nil, nil, fmt.Errorf("--param %q: en az bir değer gerekli", name)
		}
		sort.Float64s(values)
		axes = append(axes, backtest.ParamAxis{Name: name, Values: values})
		base[name] = values[len(values)/2] // medyan (tek sayıda değilse alt-orta)
	}
	return axes, base, nil
}

// paramSetStrategyFactory bridges backtest.ParamSet (name -> float64) onto
// config.TrendfollowConfig/MomentumConfig's numeric fields, reusing
// setNumericFieldByYAMLTag/applyConfigOverrides' exact same field-name
// convention ("atr_stop_mult", "sma_long", ...) so --param and
// --config-override speak the same vocabulary. baseCfg is never mutated —
// every call clones it first.
func paramSetStrategyFactory(name string, baseCfg *config.Config) backtest.StrategyFactory {
	return func(ps backtest.ParamSet) (strategy.Strategy, error) {
		c := *baseCfg
		var target reflect.Value
		switch name {
		case "trendfollow":
			target = reflect.ValueOf(&c.Strategy.Trendfollow).Elem()
		case "momentum":
			target = reflect.ValueOf(&c.Strategy.Momentum).Elem()
		default:
			return nil, fmt.Errorf("bilinmeyen strateji: %q (bilinenler: momentum, trendfollow)", name)
		}
		for k, v := range ps {
			valStr := strconv.FormatFloat(v, 'g', -1, 64)
			if err := setNumericFieldByYAMLTag(target, k, valStr); err != nil {
				return nil, fmt.Errorf("--param %s uygulanamadı: %w", k, err)
			}
		}
		return resolveStrategy(name, &c)
	}
}

func runWalkforward(cmd *cobra.Command, args []string) error {
	strategyName, err := cmd.Flags().GetString("strategy")
	if err != nil {
		return err
	}
	if strategyName == "" {
		strategyName = cfg.Strategy.Active
	}
	train, _ := cmd.Flags().GetInt("train")
	test, _ := cmd.Flags().GetInt("test")
	step, _ := cmd.Flags().GetInt("step")
	capital, err := cmd.Flags().GetFloat64("capital")
	if err != nil {
		return err
	}
	paramFlags, err := cmd.Flags().GetStringArray("param")
	if err != nil {
		return err
	}
	minTrades, _ := cmd.Flags().GetInt("min-trades")
	viewLocked, _ := cmd.Flags().GetBool("locked")
	force, _ := cmd.Flags().GetBool("force-reveal-again")
	lockedStartStr, _ := cmd.Flags().GetString("locked-start")
	lockedStart, err := time.Parse("2006-01-02", lockedStartStr)
	if err != nil {
		return fmt.Errorf("--locked-start ayrıştırılamadı: %w", err)
	}

	axes, baseParams, err := parseWalkforwardParamFlags(paramFlags)
	if err != nil {
		return err
	}
	grid, err := backtest.CartesianParamGrid(axes, 500)
	if err != nil {
		return err
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
	symbols := make([]string, len(rows))
	for i, r := range rows {
		symbols[i] = r.Symbol
	}
	allCandles, err := st.GetCandlesForSymbols(ctx, symbols, cfg.Data.Timeframe, time.Time{}, time.Time{})
	if err != nil {
		return fmt.Errorf("mumlar okunamadı: %w", err)
	}
	if len(allCandles) == 0 {
		return fmt.Errorf("veritabanında mum verisi yok — önce `swingbot data backfill` çalıştırın (data.db_path=%s)", cfg.Data.DBPath)
	}

	devCandles, lockedCandles := backtest.SplitDevLocked(allCandles, lockedStart)

	thresholds := backtest.DefaultThresholds()
	if minTrades > 0 {
		thresholds.MinTradeCount = minTrades
	}

	lsAdapter := storeLockedSegmentAdapter{st: st}
	segment := devCandles
	segmentLabel := "geliştirme"

	if viewLocked {
		segmentLabel = "KİLİTLİ"
		recorded, ok, err := loadWalkforwardThresholds(ctx, st)
		if err != nil {
			return fmt.Errorf("kaydedilmiş eşikler okunamadı: %w", err)
		}
		if !ok {
			return fmt.Errorf(
				"kilitli bölüm görüntülenemez: Bölüm 11.4 eşikleri henüz kaydedilmedi (SPEC.md Bölüm 11.1/11.4). " +
					"Önce `swingbot walkforward` komutunu (--locked OLMADAN) geliştirme bölümünde çalıştırıp eşikleri kaydedin.",
			)
		}
		thresholds = recorded

		rec, alreadyViewed, err := backtest.ViewLockedSegment(ctx, lsAdapter, backtest.LockedSegmentRecord{Thresholds: thresholds, GitSHA: gitSHA()}, force)
		if err != nil {
			return err
		}
		if alreadyViewed {
			fmt.Fprintf(os.Stderr,
				"*** UYARI: kilitli bölüm DAHA ÖNCE %s UTC tarihinde görüntülenmişti. SPEC.md Bölüm 11.1: bundan sonra strateji üzerinde DEĞİŞİKLİK YAPILMAMALI. "+
					"--force-reveal-again ile yeniden görüntüleniyorsunuz; bu bir disiplin ihlalidir ve olduğu gibi kaydedilir. ***\n",
				rec.ViewedAt.Format("2006-01-02 15:04:05"))
		} else {
			fmt.Printf("kilitli bölüm İLK KEZ görüntülendi (%s UTC). Bundan sonra strateji parametreleri DEĞİŞTİRİLEMEZ (SPEC.md Bölüm 11.1).\n",
				rec.ViewedAt.Format("2006-01-02 15:04:05"))
		}
		segment = lockedCandles
	} else {
		if _, ok, err := loadWalkforwardThresholds(ctx, st); err != nil {
			return fmt.Errorf("kaydedilmiş eşikler okunamadı: %w", err)
		} else if !ok {
			if err := saveWalkforwardThresholds(ctx, st, thresholds); err != nil {
				return fmt.Errorf("eşikler kaydedilemedi: %w", err)
			}
			fmt.Printf("Bölüm 11.4 eşikleri kaydedildi (bundan sonraki TÜM sonuçlar — kilitli bölüm dahil — bu eşiklere göre değerlendirilecek): %+v\n", thresholds)
		} else {
			thresholds, _, _ = loadWalkforwardThresholds(ctx, st)
		}
	}
	if len(segment) == 0 {
		return fmt.Errorf("%s bölümünde mum verisi yok (locked-start=%s)", segmentLabel, lockedStart.Format("2006-01-02"))
	}

	factory := paramSetStrategyFactory(strategyName, cfg)
	costs := backtest.CostsFromConfig(cfg.Costs)

	wfCfg := backtest.WalkForwardConfig{
		NewStrategy: factory,
		ParamGrid:   grid,
		Candles:     segment,
		Markets:     markets,
		InitialCash: capital,
		Costs:       costs,
		NewRiskGate: func() backtest.RiskGate { return risk.NewGate(cfg.Risk, risk.NewSizer(cfg.Risk)) },
		NewBreaker:  func() *risk.Breaker { return risk.NewBreaker(cfg.Breaker) },
		TrainDays:   train, TestDays: test, StepDays: step,
		BenchmarkSymbol: "BTC/" + cfg.Exchange.Quote,
	}
	result, err := backtest.RunWalkForward(ctx, wfCfg)
	if err != nil {
		return fmt.Errorf("walk-forward: %w", err)
	}

	// Bölüm 11.4 kriteri #4: komisyon 2x'te hâlâ pozitif mi?
	costs2x := costs
	costs2x.FeeRate *= 2
	costs2x.SlippageBps *= 2
	wfCfg2x := wfCfg
	wfCfg2x.Costs = costs2x
	result2x, err := backtest.RunWalkForward(ctx, wfCfg2x)
	if err != nil {
		return fmt.Errorf("walk-forward (2x maliyet): %w", err)
	}

	// Bölüm 11.3: parametre duyarlılığı + plato tespiti, TÜM segment
	// üzerinde (pencere başına değil) — SPEC.md'nin kendi örneği tek bir
	// tam-dönem taramasıdır.
	var sensPoints []backtest.SensitivityPoint
	var plateaus []backtest.PlateauVerdict
	var paramGridDesc []string
	if len(axes) > 0 {
		sensPoints, err = backtest.ParamSensitivitySweep(ctx, backtest.SensitivityConfig{
			NewStrategy: factory, BaseParams: baseParams, Axes: axes,
			Candles: segment, Markets: markets, InitialCash: capital, Costs: costs,
			RiskGate: risk.NewGate(cfg.Risk, risk.NewSizer(cfg.Risk)), Breaker: risk.NewBreaker(cfg.Breaker),
		})
		if err != nil {
			return fmt.Errorf("parametre duyarlılığı: %w", err)
		}
		_, byParam := backtest.GroupByParam(sensPoints)
		for _, axis := range axes {
			plateaus = append(plateaus, backtest.DetectPlateau(axis.Name, byParam[axis.Name], func(m backtest.Metrics) float64 { return m.Sharpe }, 0))
			vals := make([]string, len(axis.Values))
			for i, v := range axis.Values {
				vals[i] = strconv.FormatFloat(v, 'g', -1, 64)
			}
			paramGridDesc = append(paramGridDesc, fmt.Sprintf("%s: %s", axis.Name, strings.Join(vals, ", ")))
		}
	}

	verdict := backtest.EvaluateThresholds(thresholds, result, plateaus, result2x.Metrics)

	warnings := append([]string{}, marketWarnings...)
	if segmentLabel == "geliştirme" {
		warnings = append(warnings, "Bu koşum GELİŞTİRME bölümü üzerinde çalıştı (SPEC.md Bölüm 11.1) — kilitli bölüm henüz görüntülenmedi; bu sonuçlar nihai doğrulama değildir.")
	}

	reportPath, err := web.WriteWalkForwardReportFile("reports", web.WalkForwardReportData{
		Strategy: strategyName, Segment: segmentLabel, ParamGridDesc: paramGridDesc,
		Costs: costs, GitSHA: gitSHA(), GeneratedAt: time.Now().UTC(), Warnings: warnings,
		Windows: result.Windows, CombinedEquity: result.CombinedEquity, CombinedBenchBTC: result.CombinedBenchBTC,
		Metrics: result.Metrics, Sensitivity: sensPoints, Plateaus: plateaus,
		Thresholds: thresholds, Verdict: verdict,
	})
	if err != nil {
		return fmt.Errorf("rapor üretilemedi: %w", err)
	}

	btcMetrics := backtest.Compute(result.CombinedBenchBTC, nil)
	fmt.Printf("walk-forward tamamlandı: %s (%s bölümü, %d pencere)\n", strategyName, segmentLabel, len(result.Windows))
	fmt.Printf("birleşik getiri: %.2f%%  |  BTC al-tut: %.2f%%  |  maks. düşüş: %.2f%%  |  işlem sayısı: %d\n",
		result.Metrics.TotalReturn*100, btcMetrics.TotalReturn*100, result.Metrics.MaxDrawdown*100, result.Metrics.TradeCount)
	fmt.Printf("2x maliyetli toplam getiri: %.2f%%\n", result2x.Metrics.TotalReturn*100)
	for _, c := range verdict.Criteria {
		mark := "GEÇTİ"
		if !c.Passed {
			mark = "TERK EDİLDİ"
		}
		fmt.Printf("  [%s] %s — %s\n", mark, c.Name, c.Detail)
	}
	stamp := "TERK EDİLDİ"
	if verdict.Passed {
		stamp = "GEÇTİ"
	}
	fmt.Printf("\nSONUÇ: %s (SPEC.md Bölüm 11.4)\n", stamp)
	if !verdict.Passed {
		fmt.Println("Eşikler karşılanmadı: strateji İYİLEŞTİRİLMEZ. Faz 2'ye dönün ya da farklı bir hipotezle baştan başlayın (SPEC.md Bölüm 11.4/14).")
	}
	fmt.Printf("rapor: %s\n", reportPath)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "uyarı: %s\n", w)
	}
	return nil
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
		RunE:  runPaperStart,
	}
	start.Flags().String("strategy", "", "koşturulacak strateji (config.yaml'daki strategy.active'i geçersiz kılar)")
	start.Flags().Float64("capital", 10000, "başlangıç sermayesi (quote para birimi, örn. USDT) — SPEC.md config şemasında henüz bir alanı yok")
	start.Flags().StringArray("config-override", nil, "config.yaml alanını geçersiz kılar, örn. strategy.trendfollow.atr_stop_mult=3.0 (tekrarlanabilir)")
	start.Flags().Bool("once", false, "execution.run_at_utc'yi beklemeden bugünün döngüsünü hemen bir kez çalıştırır ve çıkar (smoke test için)")
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

// --- serve ------------------------------------------------------------

// newServeCmd backs `swingbot serve` (SPEC.md Bölüm 9): the local,
// read-only web panel (internal/web, Ajan 12). Per this file's own
// "cmd/swingbot contains no business logic" convention, everything below
// is flag parsing + wiring into web.Server; the panel itself (routes,
// JSON shapes, embedded assets) lives entirely in internal/web.
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Yerel salt-okunur web panelini başlatır (yalnızca 127.0.0.1)",
		RunE:  runServe,
	}
	cmd.Flags().String("addr", "", "panelin dinleyeceği host:port (varsayılan: config.yaml web.addr — SPEC.md Bölüm 7.1/13: 127.0.0.1 dışına açmayın)")
	return cmd
}

func runServe(cmd *cobra.Command, args []string) error {
	addrFlag, err := cmd.Flags().GetString("addr")
	if err != nil {
		return err
	}
	addr := cfg.Web.Addr
	if addrFlag != "" {
		addr = addrFlag
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	// config.Validate already warns (does not fail) on 0.0.0.0/::/wildcard
	// hosts when config.yaml is loaded; this additionally catches a LAN IP
	// such as 192.168.1.5:8080, which is neither a wildcard host nor
	// loopback but is just as reachable from outside this machine — the
	// panel has zero authentication (SPEC.md Bölüm 7.1: onay yalnızca
	// Telegram üzerinden), so anything non-loopback deserves a loud warning
	// even though this command does not refuse to start over it (an
	// operator may have a deliberate reason, e.g. an already-isolated VPN).
	if !web.IsLoopbackAddr(addr) {
		fmt.Fprintf(os.Stderr, "uyarı: panel %s adresinde dinleyecek — bu loopback (127.0.0.1) değil, panel dışarıdan erişilebilir olabilir (SPEC.md Bölüm 7.1/13).\n", addr)
	}

	ctx := cmd.Context()
	st, err := store.Open(ctx, cfg.Data.DBPath)
	if err != nil {
		return fmt.Errorf("veritabanı açılamadı (data.db_path=%s): %w", cfg.Data.DBPath, err)
	}
	defer st.Close()

	srv := web.NewServer(st, cfg, swingbotVersion)

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("panel dinliyor: http://%s (durdurmak için Ctrl+C)\n", addr)
	if err := srv.ListenAndServe(sigCtx, addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("panel sunucusu hata verdi: %w", err)
	}
	fmt.Println("panel durduruldu")
	return nil
}

// --- breaker status|reset -------------------------------------------------

// breakerStateKey is the system_state key the (not-yet-built)
// internal/engine daily loop will write to when risk.Breaker trips
// (SPEC.md Bölüm 6.5.3: "system_state['breaker'] = 'open' + gerekçe +
// zaman damgası"). This file establishes the wire format — a JSON-encoded
// risk.State — since it is the first code to read/write it; internal/engine
// should reuse this same encoding rather than inventing another one.
const breakerStateKey = "breaker"

func newBreakerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "breaker",
		Short: "Devre kesici durumu ve sıfırlama (internal/risk)",
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Devre kesicinin güncel durumunu ve tetikleyen koşulu gösterir",
		RunE:  runBreakerStatus,
	}

	reset := &cobra.Command{
		Use:   "reset",
		Short: "Devre kesiciyi sıfırlar (yalnızca --confirm ile, dikkatli kullanın)",
		RunE:  runBreakerReset,
	}
	reset.Flags().Bool("confirm", false, "sıfırlamayı açıkça onaylar")

	cmd.AddCommand(status, reset)
	return cmd
}

func runBreakerStatus(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	st, err := store.Open(ctx, cfg.Data.DBPath)
	if err != nil {
		return fmt.Errorf("veritabanı açılamadı (data.db_path=%s): %w", cfg.Data.DBPath, err)
	}
	defer st.Close()

	raw, ok, err := st.GetState(ctx, breakerStateKey)
	if err != nil {
		return fmt.Errorf("devre kesici durumu okunamadı: %w", err)
	}
	if !ok {
		fmt.Println("devre kesici: KAPALI (hiç tetiklenmedi)")
		return nil
	}
	var state risk.State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return fmt.Errorf("devre kesici durumu ayrıştırılamadı (system_state[%q]=%q): %w", breakerStateKey, raw, err)
	}
	if !state.Open {
		fmt.Println("devre kesici: KAPALI")
		return nil
	}
	fmt.Printf("devre kesici: AÇIK\ngerekçe: %s\ndetay: %s\ntetiklenme zamanı: %s UTC\n\nkapatmak için: swingbot breaker reset --confirm\n",
		state.Reason, state.Detail, state.At.UTC().Format("2006-01-02 15:04:05"))
	return nil
}

func runBreakerReset(cmd *cobra.Command, args []string) error {
	confirm, err := cmd.Flags().GetBool("confirm")
	if err != nil {
		return err
	}
	if !confirm {
		return errors.New("breaker reset --confirm bayrağı olmadan çalışmaz (SPEC.md Bölüm 6.5.3: devre kesici yalnızca manuel olarak kapatılır)")
	}

	ctx := cmd.Context()
	st, err := store.Open(ctx, cfg.Data.DBPath)
	if err != nil {
		return fmt.Errorf("veritabanı açılamadı (data.db_path=%s): %w", cfg.Data.DBPath, err)
	}
	defer st.Close()

	raw, err := json.Marshal(risk.State{}) // zero value: Open=false, no reason/detail/timestamp
	if err != nil {
		return err
	}
	if err := st.SetState(ctx, breakerStateKey, string(raw)); err != nil {
		return fmt.Errorf("devre kesici sıfırlanamadı: %w", err)
	}
	fmt.Println("devre kesici sıfırlandı: KAPALI")
	return nil
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
