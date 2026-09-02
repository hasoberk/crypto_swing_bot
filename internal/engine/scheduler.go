package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"swingbot/internal/notify"
)

// NextRunAt returns the next UTC instant at/after now that matches
// runAtUTC ("HH:MM", config.yaml execution.run_at_utc — SPEC.md Bölüm 6.7
// adım 1: "UTC 00:05, mum kapanışından 5 dk sonra"). now need not be UTC;
// it is converted first. The returned time is always strictly in the
// future relative to now (today's run_at_utc if it has not happened yet,
// otherwise tomorrow's).
func NextRunAt(now time.Time, runAtUTC string) (time.Time, error) {
	now = now.UTC()
	hh, mm, err := parseHHMM(runAtUTC)
	if err != nil {
		return time.Time{}, err
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

func parseHHMM(s string) (hh, mm int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("engine: run_at_utc %q geçersiz (HH:MM bekleniyor)", s)
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("engine: run_at_utc %q geçersiz (HH:MM bekleniyor)", s)
	}
	return hh, mm, nil
}

// sleepPollCap bounds how long a single Clock.Sleep call in sleepUntil
// blocks, so Run stays responsive to ctx cancellation instead of sleeping
// through a multi-hour wait in one uninterruptible call.
const sleepPollCap = time.Minute

// Run drives SPEC.md Bölüm 6.7's scheduling loop: resume anything left
// over from a previous run (restart resilience), then repeatedly sleep
// until the next run_at_utc and execute one daily cycle, until ctx is
// canceled.
//
// A single day's RunOnce returning an error does not stop the loop — it is
// reported (RunOnce itself already notified the specific failure) and Run
// simply waits for tomorrow's scheduled run rather than leaving the daemon
// dead after one bad day, which SPEC.md Bölüm 12's Faz 4 kabul kriteri
// ("8 hafta kesintisiz çalışma") depends on.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.ResumePending(ctx); err != nil {
		e.notify(ctx, notify.LevelCritical, "Devam ettirme hatası", err.Error())
		return fmt.Errorf("engine: resume pending: %w", err)
	}

	for {
		next, err := NextRunAt(e.cfg.Clock.Now(), e.cfg.RunAtUTC)
		if err != nil {
			return err
		}
		if err := e.sleepUntil(ctx, next); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := e.RunOnce(ctx); err != nil {
			e.notify(ctx, notify.LevelCritical, "Günlük döngü hata verdi", err.Error())
		}
	}
}

// sleepUntil blocks (via e.cfg.Clock.Sleep, in sleepPollCap-sized steps so
// ctx cancellation is honored promptly even mid-wait) until Clock.Now() is
// at/after t.
func (e *Engine) sleepUntil(ctx context.Context, t time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := e.cfg.Clock.Now()
		if !now.Before(t) {
			return nil
		}
		step := t.Sub(now)
		if step > sleepPollCap {
			step = sleepPollCap
		}
		e.cfg.Clock.Sleep(step)
	}
}
