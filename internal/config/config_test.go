package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// baseYAML is a minimal but complete config.yaml. Tests override just the
// fields they care about via simple string substitution so each test stays
// focused on the one thing it's checking.
const baseYAML = `
mode: %s
exchange:
  name: binance
  quote: USDT
  rate_limit_per_min: 1000
data:
  db_path: ./data/swingbot.db
  timeframe: 1d
  backfill_years: 3
universe:
  min_median_quote_volume: 5000000
  min_listing_age_days: 180
  max_symbols: 100
  exclude_patterns: ["UP/", "DOWN/"]
  exclude_stablecoins: true
strategy:
  active: trendfollow
  trendfollow:
    sma_long: 200
    sma_exit: 50
    breakout_lookback: 20
    atr_period: 14
    atr_stop_mult: 2.5
    max_atr_pct: 0.08
  momentum:
    top_n: 5
    exit_rank: 10
    rebalance_weekday: 1
    weights: { mom_90: 0.4, mom_30: 0.3, vol_30: 0.2, liq: 0.1 }
risk:
  risk_per_trade: 0.01
  max_positions: 5
  max_exposure: 0.80
  max_position_pct: 0.25
  cooldown_hours: 24
breaker:
  max_drawdown: 0.15
  max_daily_loss: 0.05
  max_consecutive_losses: 6
  max_order_errors_24h: 3
costs:
  fee_rate: %s
  slippage_bps: %s
execution:
  order_type: market
  approval_ttl_hours: 4
  run_at_utc: "00:05"
telegram:
  enabled: %s
  allowed_chat_id: 0
web:
  addr: "%s"
log:
  level: info
  file: ./data/swingbot.log
`

// writeYAML renders baseYAML with the given knobs and writes it to a temp
// file, returning its path.
func writeYAML(t *testing.T, dir, mode, feeRate, slippageBps, telegramEnabled, webAddr string) string {
	t.Helper()
	content := fmt.Sprintf(baseYAML, mode, feeRate, slippageBps, telegramEnabled, webAddr)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return path
}

func writeEnv(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, ".env")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return path
}

func TestLoad_ValidPaperConfig(t *testing.T) {
	dir := t.TempDir()
	yamlPath := writeYAML(t, dir, "paper", "0.001", "15", "false", "127.0.0.1:8080")

	cfg, warnings, err := Load(yamlPath, filepath.Join(dir, "does-not-exist.env"))
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if cfg.Mode != ModePaper {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, ModePaper)
	}
	if cfg.Costs.FeeRate != 0.001 || cfg.Costs.SlippageBps != 15 {
		t.Fatalf("costs not parsed correctly: %+v", cfg.Costs)
	}
}

// İ4: a backtest/paper/live run configured with zero costs must be
// rejected, not silently defaulted.
func TestValidate_ZeroCostsRejected(t *testing.T) {
	cases := []struct {
		name        string
		feeRate     string
		slippageBps string
	}{
		{"zero fee_rate", "0", "15"},
		{"zero slippage_bps", "0.001", "0"},
		{"both zero", "0", "0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			yamlPath := writeYAML(t, dir, "backtest", tc.feeRate, tc.slippageBps, "false", "127.0.0.1:8080")

			_, _, err := Load(yamlPath, filepath.Join(dir, "missing.env"))
			if err == nil {
				t.Fatal("expected error for zero-cost config, got nil")
			}
			if !errors.Is(err, ErrCostsNotConfigured) {
				t.Fatalf("expected ErrCostsNotConfigured, got: %v", err)
			}
		})
	}
}

// mode: live without exchange credentials in .env must be rejected.
func TestValidate_LiveModeRequiresSecrets(t *testing.T) {
	dir := t.TempDir()
	yamlPath := writeYAML(t, dir, "live", "0.001", "15", "false", "127.0.0.1:8080")

	_, _, err := Load(yamlPath, filepath.Join(dir, "missing.env"))
	if err == nil {
		t.Fatal("expected error for live mode with missing secrets, got nil")
	}
	if !errors.Is(err, ErrMissingSecrets) {
		t.Fatalf("expected ErrMissingSecrets, got: %v", err)
	}
}

// mode: live with telegram enabled additionally requires telegram
// credentials.
func TestValidate_LiveModeRequiresTelegramSecretsWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	yamlPath := writeYAML(t, dir, "live", "0.001", "15", "true", "127.0.0.1:8080")
	envPath := writeEnv(t, dir,
		"EXCHANGE_API_KEY=key123",
		"EXCHANGE_API_SECRET=secret123",
		// TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID deliberately omitted.
	)

	_, _, err := Load(yamlPath, envPath)
	if !errors.Is(err, ErrMissingSecrets) {
		t.Fatalf("expected ErrMissingSecrets, got: %v", err)
	}
}

// mode: live with all required secrets present should succeed.
func TestValidate_LiveModeWithSecretsSucceeds(t *testing.T) {
	dir := t.TempDir()
	yamlPath := writeYAML(t, dir, "live", "0.001", "15", "true", "127.0.0.1:8080")
	envPath := writeEnv(t, dir,
		"EXCHANGE_API_KEY=key123",
		"EXCHANGE_API_SECRET=secret123",
		"TELEGRAM_BOT_TOKEN=bot123",
		"TELEGRAM_CHAT_ID=42",
	)

	cfg, warnings, err := Load(yamlPath, envPath)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if cfg.Secrets.ExchangeAPIKey != "key123" {
		t.Fatalf("ExchangeAPIKey = %q, want key123", cfg.Secrets.ExchangeAPIKey)
	}
	if cfg.Telegram.AllowedChatID != 42 {
		t.Fatalf("AllowedChatID = %d, want 42 (should be overridden from .env)", cfg.Telegram.AllowedChatID)
	}
}

// A process environment variable must take precedence over the same key
// in the .env file.
func TestLoadEnv_ProcessEnvTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	envPath := writeEnv(t, dir, "EXCHANGE_API_KEY=from-file")

	t.Setenv("EXCHANGE_API_KEY", "from-process-env")

	secrets, err := LoadEnv(envPath)
	if err != nil {
		t.Fatalf("LoadEnv error: %v", err)
	}
	if secrets.ExchangeAPIKey != "from-process-env" {
		t.Fatalf("ExchangeAPIKey = %q, want %q", secrets.ExchangeAPIKey, "from-process-env")
	}
}

// web.addr = 0.0.0.0 is a warning, not a fatal error.
func TestValidate_WebAddrExposedWarnsNotFatal(t *testing.T) {
	dir := t.TempDir()
	yamlPath := writeYAML(t, dir, "paper", "0.001", "15", "false", "0.0.0.0:8080")

	cfg, warnings, err := Load(yamlPath, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("expected non-fatal warning, got error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	if len(warnings) != 1 || warnings[0] != WarnWebAddrExposed {
		t.Fatalf("warnings = %v, want [%q]", warnings, WarnWebAddrExposed)
	}
}

func TestValidate_InvalidModeRejected(t *testing.T) {
	dir := t.TempDir()
	yamlPath := writeYAML(t, dir, "yolo", "0.001", "15", "false", "127.0.0.1:8080")

	_, _, err := Load(yamlPath, filepath.Join(dir, "missing.env"))
	if !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("expected ErrInvalidMode, got: %v", err)
	}
}
