// universe.go backs `swingbot universe show` (SPEC.md Bölüm 9). It is owned
// by universe-scoring-engineer (Ajan 8): internal/universe's Filter/Score/
// Build do all the actual work (SPEC.md Bölüm 6.2); this file only parses
// the --date flag, opens the store read-only, translates cfg into
// universe.FilterParams/Weights, and prints the result — no filtering or
// scoring logic of its own, per this repo's "cmd/swingbot contains no
// business logic" convention (see main.go's file-level doc comment).
//
// newUniverseCmd itself still lives in main.go (cli-integration-lead/Ajan
// 14 owns the command tree's registration); this file only supplies the
// RunE that command's "show" subcommand now points at, per main.go's own
// instruction ("As each agent lands its package, swap the matching RunE for
// a real call into that package").
package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"swingbot/internal/datafeed"
	"swingbot/internal/store"
	"swingbot/internal/universe"
)

// openUniverseStore opens the store at cfg.Data.DBPath for `universe show`.
// Unlike openFeed (used by the `data` commands), this does not construct an
// exchange.Exchange — `universe show` only ever reads candles/markets
// already backfilled by `data backfill|update`.
func openUniverseStore(ctx context.Context) (*store.Store, error) {
	st, err := store.Open(ctx, cfg.Data.DBPath)
	if err != nil {
		return nil, fmt.Errorf("veritabanı açılamadı (data.db_path=%s): %w. İpucu: önce `swingbot data backfill` çalıştırın.", cfg.Data.DBPath, err)
	}
	return st, nil
}

// parseAsOfFlag parses --date (YYYY-MM-DD) into a UTC midnight time.Time
// matching candles.open_time's granularity for the configured (daily, by
// SPEC.md Bölüm 8 default) timeframe. An empty value defaults to today UTC.
func parseAsOfFlag(dateStr string) (time.Time, error) {
	if dateStr == "" {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("--date geçersiz (YYYY-MM-DD bekleniyor, örn. 2026-08-14): %w", err)
	}
	return t.UTC(), nil
}

func runUniverseShow(cmd *cobra.Command, args []string) error {
	dateStr, err := cmd.Flags().GetString("date")
	if err != nil {
		return err
	}
	asOf, err := parseAsOfFlag(dateStr)
	if err != nil {
		return err
	}

	st, err := openUniverseStore(cmd.Context())
	if err != nil {
		return err
	}
	defer st.Close()

	tfDur, err := datafeed.ParseTimeframe(cfg.Data.Timeframe)
	if err != nil {
		return fmt.Errorf("universe show: config.yaml data.timeframe geçersiz: %w", err)
	}

	params := universe.FilterParams{
		Quote:                cfg.Exchange.Quote,
		MinMedianQuoteVolume: cfg.Universe.MinMedianQuoteVolume,
		MinListingAgeDays:    cfg.Universe.MinListingAgeDays,
		ExcludePatterns:      cfg.Universe.ExcludePatterns,
		ExcludeStablecoins:   cfg.Universe.ExcludeStablecoins,
		TimeframeDur:         tfDur,
		MaxSymbols:           cfg.Universe.MaxSymbols,
	}
	weights := universe.WeightsFromMap(cfg.Strategy.Momentum.Weights)

	result, err := universe.Build(cmd.Context(), st, cfg.Data.Timeframe, asOf, params, weights)
	if err != nil {
		return fmt.Errorf("universe show: %w", err)
	}

	printUniverseResult(result, asOf)
	return nil
}

// printUniverseResult renders a BuildResult as two plain tables (included,
// ranked; excluded, with reasons) — SPEC.md Bölüm 9 / this agent's kabul
// kriteri: "belirli bir tarihteki evreni, skorları ve elenen sembollerin
// eleme gerekçesini gösteriyor."
func printUniverseResult(result *universe.BuildResult, asOf time.Time) {
	fmt.Printf("Evren — %s (%d sembol)\n\n", asOf.Format("2006-01-02"), len(result.Universe))

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "RANK\tSEMBOL\tSKOR\tMOM_90\tMOM_30\tVOL_30\tLIQ(ln)\t30G MEDYAN HACİM")
	for _, s := range result.Universe {
		c := s.Components
		fmt.Fprintf(w, "%d\t%s\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.0f\n",
			s.Rank, s.Symbol, s.Score, c.Mom90, c.Mom30, c.Vol30, c.Liq, c.MedianQuoteVolume30)
	}
	w.Flush()

	if len(result.Excluded) == 0 {
		return
	}
	fmt.Printf("\nElenen semboller (%d)\n\n", len(result.Excluded))
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SEMBOL\tGEREKÇE\tDETAY")
	for _, e := range result.Excluded {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Symbol, e.Reason, e.Detail)
	}
	w.Flush()
}
