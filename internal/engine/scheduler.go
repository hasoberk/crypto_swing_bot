package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"swingbot/internal/notify"
)

// NextRunAt returns the next instant at/after now whose WALL-CLOCK time in
// loc matches runAt ("HH:MM", config.yaml execution.run_at_utc — SPEC.md
// Bölüm 6.7 adım 1: "borsa verisinin oturması için mum kapanışından sonra").
// loc is config.yaml execution.timezone (config.Config's
// withDefaults/paper.go resolve an empty setting to time.UTC, matching the
// SPEC default and every deployment before this field existed) — using
// time.Date with an IANA *time.Location rather than a fixed UTC offset is
// what makes this automatically track that zone's DST transitions (e.g.
// Europe/Rome shifting between CET and CEST) instead of drifting an hour
// twice a year the way a hardcoded UTC offset would. now need not already
// be in loc; it is converted first. The returned time is always strictly
// in the future relative to now (today's run_at if it has not happened
// yet, otherwise tomorrow's).
func NextRunAt(now time.Time, runAt string, loc *time.Location) (time.Time, error) {
	hh, mm, err := parseHHMM(runAt)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, loc)
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
		next, err := NextRunAt(e.cfg.Clock.Now(), e.cfg.RunAt, e.cfg.Location)
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
