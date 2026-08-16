// Package config loads and validates swingbot's configuration.
//
// Settings live in config.yaml (checked into git as config.example.yaml,
// SPEC.md Bölüm 8); secrets live in .env (never checked into git). This
// package is the only place that knows how to combine the two into a
// validated Config.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode is the operating mode swingbot runs in. The same strategy and risk
// code runs unchanged in all three (İ1) — only Clock/DataSource/Broker
// implementations differ.
const (
	ModeBacktest = "backtest"
	ModePaper    = "paper"
	ModeLive     = "live"
)

// Sentinel errors. Callers (cmd/swingbot, tests) should use errors.Is
// against these rather than comparing strings.
var (
	// ErrCostsNotConfigured is İ4: a simulation/live run with zero fees or
	// zero slippage is refused. Defaulting costs to zero silently produces
	// backtests that look far better than reality.
	ErrCostsNotConfigured = errors.New("config: costs.fee_rate and costs.slippage_bps must both be greater than zero (İ4 — maliyetler varsayılan olarak kötümser olmalı)")

	// ErrInvalidMode is returned when mode is not one of
	// backtest|paper|live.
	ErrInvalidMode = errors.New("config: mode must be one of \"backtest\", \"paper\" or \"live\"")

	// ErrMissingSecrets is returned when mode=live but the .env file (or
	// process environment) does not supply the credentials live trading
	// needs.
	ErrMissingSecrets = errors.New("config: mode=live requires EXCHANGE_API_KEY and EXCHANGE_API_SECRET (and, if telegram is enabled, TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID) to be set in .env or the environment")
)

// WarnWebAddrExposed is the (non-fatal) warning text returned by Validate
// when web.addr is bound to 0.0.0.0. Callers should surface this to the
// user and require explicit confirmation before proceeding — it is not an
// error on its own because there may be a deliberate reason (e.g. running
// inside an already-isolated container/VPN).
const WarnWebAddrExposed = "web.addr is bound to 0.0.0.0 (all interfaces) — the panel would be reachable from outside this machine. SPEC.md Bölüm 13 requires 127.0.0.1. Confirm this is intentional."

// Config mirrors the config.yaml schema described in SPEC.md Bölüm 8.
type Config struct {
	Mode string `yaml:"mode"`

	Exchange  ExchangeConfig  `yaml:"exchange"`
	Data      DataConfig      `yaml:"data"`
	Universe  UniverseConfig  `yaml:"universe"`
	Strategy  StrategyConfig  `yaml:"strategy"`
	Risk      RiskConfig      `yaml:"risk"`
	Breaker   BreakerConfig   `yaml:"breaker"`
	Costs     CostsConfig     `yaml:"costs"`
	Execution ExecutionConfig `yaml:"execution"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	Web       WebConfig       `yaml:"web"`
	Log       LogConfig       `yaml:"log"`

	// Secrets is never populated from YAML (see the yaml:"-" tag); it is
	// filled in by LoadEnv/Load from .env and the process environment.
	// Keeping it out of the yaml-decoded struct means a stray secret can
	// never accidentally end up committed in config.yaml.
	Secrets Secrets `yaml:"-"`
}

type ExchangeConfig struct {
	Name            string `yaml:"name"`
	Quote           string `yaml:"quote"`
	RateLimitPerMin int    `yaml:"rate_limit_per_min"`
}

type DataConfig struct {
	DBPath        string `yaml:"db_path"`
	Timeframe     string `yaml:"timeframe"`
	BackfillYears int    `yaml:"backfill_years"`
}

type UniverseConfig struct {
	MinMedianQuoteVolume float64  `yaml:"min_median_quote_volume"`
	MinListingAgeDays    int      `yaml:"min_listing_age_days"`
	MaxSymbols           int      `yaml:"max_symbols"`
	ExcludePatterns      []string `yaml:"exclude_patterns"`
	ExcludeStablecoins   bool     `yaml:"exclude_stablecoins"`
}

type StrategyConfig struct {
	Active      string            `yaml:"active"`
	Trendfollow TrendfollowConfig `yaml:"trendfollow"`
	Momentum    MomentumConfig    `yaml:"momentum"`
}

type TrendfollowConfig struct {
	SMALong          int     `yaml:"sma_long"`
	SMAExit          int     `yaml:"sma_exit"`
	BreakoutLookback int     `yaml:"breakout_lookback"`
	ATRPeriod        int     `yaml:"atr_period"`
	ATRStopMult      float64 `yaml:"atr_stop_mult"`
	MaxATRPct        float64 `yaml:"max_atr_pct"`
}

type MomentumConfig struct {
	TopN             int                `yaml:"top_n"`
	ExitRank         int                `yaml:"exit_rank"`
	RebalanceWeekday int                `yaml:"rebalance_weekday"`
	Weights          map[string]float64 `yaml:"weights"`
}

type RiskConfig struct {
	RiskPerTrade   float64 `yaml:"risk_per_trade"`
	MaxPositions   int     `yaml:"max_positions"`
	MaxExposure    float64 `yaml:"max_exposure"`
	MaxPositionPct float64 `yaml:"max_position_pct"`
	CooldownHours  int     `yaml:"cooldown_hours"`
}

type BreakerConfig struct {
	MaxDrawdown          float64 `yaml:"max_drawdown"`
	MaxDailyLoss         float64 `yaml:"max_daily_loss"`
	MaxConsecutiveLosses int     `yaml:"max_consecutive_losses"`
	MaxOrderErrors24h    int     `yaml:"max_order_errors_24h"`
}

// CostsConfig is İ4: fee_rate and slippage_bps are mandatory and must be
// non-zero. See Validate / ErrCostsNotConfigured.
type CostsConfig struct {
	FeeRate     float64 `yaml:"fee_rate"`
	SlippageBps float64 `yaml:"slippage_bps"`
}

type ExecutionConfig struct {
	OrderType        string `yaml:"order_type"`
	ApprovalTTLHours int    `yaml:"approval_ttl_hours"`
	RunAtUTC         string `yaml:"run_at_utc"`
}

type TelegramConfig struct {
	Enabled       bool  `yaml:"enabled"`
	AllowedChatID int64 `yaml:"allowed_chat_id"` // .env'den override edilir
}

type WebConfig struct {
	Addr string `yaml:"addr"` // DIŞARI AÇMA
}

type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

// Secrets holds values that must never appear in config.yaml. They are
// read from .env (and/or the process environment, which takes
// precedence).
type Secrets struct {
	ExchangeAPIKey    string
	ExchangeAPISecret string
	TelegramBotToken  string
	TelegramChatID    string
}

// Load reads configPath (YAML) and envPath (.env-style KEY=VALUE), merges
// secrets into the resulting Config, validates it, and returns it.
//
// Load itself never treats a non-fatal warning (e.g. web.addr = 0.0.0.0)
// as a reason to fail: those are returned alongside the config so the
// caller (e.g. cmd/swingbot) can prompt the user for confirmation. Fatal
// validation problems are returned as a non-nil error and cfg should be
// discarded — SPEC.md Bölüm 8 requires the application to refuse to start.
func Load(configPath, envPath string) (cfg *Config, warnings []string, err error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: read %s: %w", configPath, err)
	}

	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, nil, fmt.Errorf("config: parse %s: %w", configPath, err)
	}

	secrets, err := LoadEnv(envPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	c.Secrets = secrets

	// .env'den override: telegram.allowed_chat_id sıfırsa ve env'de bir
	// chat id varsa onu kullan (SPEC.md Bölüm 8: "TELEGRAM_CHAT_ID ...
	// .env'den override edilir").
	if c.Telegram.AllowedChatID == 0 && secrets.TelegramChatID != "" {
		if id, convErr := parseInt64(secrets.TelegramChatID); convErr == nil {
			c.Telegram.AllowedChatID = id
		}
	}

	warnings, err = c.Validate()
	if err != nil {
		return nil, warnings, err
	}
	return c, warnings, nil
}

// LoadEnv reads envPath (a .env-style file: KEY=VALUE lines, '#' comments,
// blank lines ignored) and returns the four secrets swingbot needs.
//
// Precedence: an already-set process environment variable always wins
// over the value found in the file — this makes it possible to override
// .env from a real deployment environment (systemd, docker, CI) without
// editing the file. A missing .env file is not an error by itself: on
// backtest/paper runs with telegram disabled, no secrets may be needed at
// all. Validate is what decides whether the mode in use actually requires
// them.
func LoadEnv(envPath string) (Secrets, error) {
	fileVals := map[string]string{}

	if envPath != "" {
		f, err := os.Open(envPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return Secrets{}, fmt.Errorf("read %s: %w", envPath, err)
			}
			// dosya yok: sorun değil, süreç ortam değişkenlerine güveniriz.
		} else {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				key, val, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				key = strings.TrimSpace(key)
				val = strings.TrimSpace(val)
				val = strings.Trim(val, `"'`)
				fileVals[key] = val
			}
			if err := scanner.Err(); err != nil {
				return Secrets{}, fmt.Errorf("scan %s: %w", envPath, err)
			}
		}
	}

	get := func(key string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return fileVals[key]
	}

	return Secrets{
		ExchangeAPIKey:    get("EXCHANGE_API_KEY"),
		ExchangeAPISecret: get("EXCHANGE_API_SECRET"),
		TelegramBotToken:  get("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:    get("TELEGRAM_CHAT_ID"),
	}, nil
}

// Validate checks c for the fatal and non-fatal conditions SPEC.md Bölüm 8
// requires:
//
//   - mode must be one of backtest|paper|live                     (fatal)
//   - costs.fee_rate / costs.slippage_bps must be non-zero (İ4)    (fatal)
//   - mode=live requires exchange (and, if telegram is enabled,
//     telegram) secrets to be present in .env/environment          (fatal)
//   - web.addr = 0.0.0.0 is a warning, not fatal: caller should ask
//     for confirmation before proceeding.
//
// On a fatal problem, Validate returns a non-nil error wrapping one of the
// sentinel errors in this package (use errors.Is to check). Warnings are
// always returned regardless of whether err is nil, so callers can log
// them even when validation otherwise succeeds.
func (c *Config) Validate() (warnings []string, err error) {
	switch c.Mode {
	case ModeBacktest, ModePaper, ModeLive:
	default:
		return warnings, fmt.Errorf("%w: got %q", ErrInvalidMode, c.Mode)
	}

	// İ4: sıfır maliyetli simülasyon/canlı çalışma reddedilir.
	if c.Costs.FeeRate <= 0 || c.Costs.SlippageBps <= 0 {
		return warnings, fmt.Errorf("%w (fee_rate=%v, slippage_bps=%v)", ErrCostsNotConfigured, c.Costs.FeeRate, c.Costs.SlippageBps)
	}

	if c.Mode == ModeLive {
		missing := c.Secrets.ExchangeAPIKey == "" || c.Secrets.ExchangeAPISecret == ""
		if c.Telegram.Enabled {
			missing = missing || c.Secrets.TelegramBotToken == "" || c.Secrets.TelegramChatID == ""
		}
		if missing {
			return warnings, ErrMissingSecrets
		}
	}

	if isAllInterfaces(c.Web.Addr) {
		warnings = append(warnings, WarnWebAddrExposed)
	}

	return warnings, nil
}

// isAllInterfaces reports whether addr (a host:port string) binds to all
// network interfaces (0.0.0.0, ::, or an empty host such as ":8080").
func isAllInterfaces(addr string) bool {
	host := addr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		host = addr[:idx]
	}
	switch host {
	case "0.0.0.0", "::", "", "*":
		return true
	default:
		return false
	}
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
