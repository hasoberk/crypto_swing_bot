// paper.go backs `swingbot paper start` (SPEC.md Bölüm 9/12 Faz 4). It is
// owned by live-engine-notify-engineer (Ajan 11): internal/engine and
// internal/notify do all the actual work (SPEC.md Bölüm 6.7/6.8); this file
// only parses flags, wires config.Config into internal/engine.Config and
// internal/notify.TelegramNotifier, and runs the resulting Engine until
// interrupted — no daily-loop or Telegram business logic of its own, per
// this repo's "cmd/swingbot contains no business logic" convention (see
// main.go's file-level doc comment).
//
// newPaperCmd itself still lives in main.go (cli-integration-lead/Ajan 14
// owns the command tree's registration); this file only supplies the RunE
// that command's "start" subcommand now points at.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"swingbot/internal/backtest"
	"swingbot/internal/engine"
	"swingbot/internal/notify"
	"swingbot/internal/risk"
	"swingbot/internal/universe"
)

func runPaperStart(cmd *cobra.Command, args []string) error {
	overrides, err := cmd.Flags().GetStringArray("config-override")
	if err != nil {
		return err
	}

	once, err := cmd.Flags().GetBool("once")
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

	capital, err := cmd.Flags().GetFloat64("capital")
	if err != nil {
		return err
	}
	if capital <= 0 {
		return fmt.Errorf("paper start: --capital pozitif olmalı, aldı: %v", capital)
	}

	if !cfg.Telegram.Enabled {
		return errors.New(
			"paper start: config.yaml telegram.enabled=false — onay akışı olmadan paper trading çalışamaz (SPEC.md Bölüm 6.7 adım 11/12)",
		)
	}
	if cfg.Secrets.TelegramBotToken == "" {
		return errors.New("paper start: .env TELEGRAM_BOT_TOKEN boş (SPEC.md Bölüm 8)")
	}

	ctx := cmd.Context()
	feed, st, err := openFeed(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	tg, err := notify.NewTelegramNotifier(cfg.Secrets.TelegramBotToken, cfg.Telegram.AllowedChatID)
	if err != nil {
		return fmt.Errorf("paper start: telegram: %w", err)
	}

	riskGate := risk.NewGate(cfg.Risk, risk.NewSizer(cfg.Risk))
	costs := backtest.CostsFromConfig(cfg.Costs)

	uParams := universe.FilterParams{
		Quote:                cfg.Exchange.Quote,
		MinMedianQuoteVolume: cfg.Universe.MinMedianQuoteVolume,
		MinListingAgeDays:    cfg.Universe.MinListingAgeDays,
		ExcludePatterns:      cfg.Universe.ExcludePatterns,
		ExcludeStablecoins:   cfg.Universe.ExcludeStablecoins,
		MaxSymbols:           cfg.Universe.MaxSymbols,
	}
	uWeights := universe.WeightsFromMap(cfg.Strategy.Momentum.Weights)

	eng, err := engine.New(engine.Config{
		Store: st, Feed: feed, Strategy: strat, Notifier: tg,
		RiskGate: riskGate, BreakerCfg: cfg.Breaker,
		Costs: costs, InitialCash: capital,
		UniverseParams: uParams, UniverseWeights: uWeights,
		Timeframe: cfg.Data.Timeframe, Quote: cfg.Exchange.Quote,
		ApprovalTTL: time.Duration(cfg.Execution.ApprovalTTLHours) * time.Hour,
		RunAtUTC:    cfg.Execution.RunAtUTC,
	})
	if err != nil {
		return fmt.Errorf("paper start: %w", err)
	}

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go tg.Start(sigCtx)

	if once {
		fmt.Printf("paper trading (--once): strateji=%s sermaye=%.2f %s bugünün döngüsü hemen çalıştırılıyor\n",
			strat.Name(), capital, cfg.Exchange.Quote)
		if err := eng.ResumePending(sigCtx); err != nil {
			return fmt.Errorf("paper start --once: resume pending: %w", err)
		}
		if err := eng.RunOnce(sigCtx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("paper start --once: %w", err)
		}
		fmt.Println("paper trading (--once) tamamlandı")
		return nil
	}

	fmt.Printf("paper trading başladı: strateji=%s sermaye=%.2f %s günlük çalışma saati (UTC)=%s (durdurmak için Ctrl+C)\n",
		strat.Name(), capital, cfg.Exchange.Quote, cfg.Execution.RunAtUTC)

	if err := eng.Run(sigCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("paper start: %w", err)
	}
	fmt.Println("paper trading durduruldu")
	return nil
}
