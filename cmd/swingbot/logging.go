// This file wires config.yaml's `log:` block into log/slog's global default
// logger (setupLogging) and adds a last-resort secret-masking slog.Handler
// wrapper on top of it (maskingHandler). Both are pure plumbing owned by
// Ajan 14 (cli-integration-lead): no new business logic, just making sure
// (a) packages that already call slog.Default()/slog.Info honor the
// configured level/file, and (b) whatever text ends up in those log lines
// never contains a live secret value — SPEC.md Bölüm 13 security checklist:
// "Sırlar loglara yazılmıyor (log yazıcıda maskeleme var)".
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"swingbot/internal/config"
)

// maskedPlaceholder replaces any occurrence of a known secret value inside a
// rendered log line.
const maskedPlaceholder = "***MASKED***"

// maskingHandler wraps another slog.Handler and scrubs known secret values
// out of the final rendered text of every record before it reaches the real
// writer(s).
//
// Why render-then-scrub instead of inspecting each slog.Attr: the actual
// risk this defends against is not "someone wrote slog.Info(\"msg\", \"key\",
// secretValue)" (that call site simply shouldn't exist, and none does today)
// — it's a third-party library or an fmt.Errorf("%w", err) chain embedding a
// secret (e.g. a signed request URL, an Authorization header, a bot token)
// somewhere inside an *error string* that then gets logged as the record's
// Message or as an "error" attribute's value. A per-attribute check can miss
// that because the secret is buried inside a larger string, not living in
// its own attribute. Scrubbing the fully-rendered line with a plain
// substring search catches it regardless of which attribute (or the message
// itself) it ended up in.
//
// maskingHandler renders each record into a private buffer via an inner
// slog.Handler (built by the same NewTextHandler/opts the caller would have
// used directly), so it looks/behaves exactly like the unwrapped handler
// except for the masking pass. The buffer and its mutex are shared across
// every handler derived from the same root via WithAttrs/WithGroup, which is
// safe because they're always used to serialize access to that one buffer
// (slog requires handlers to be safe for concurrent use, and by protecting
// with a mutex here that requirement is met even though the buffer itself
// is single-writer-at-a-time).
type maskingHandler struct {
	mu      *sync.Mutex
	buf     *bytes.Buffer
	inner   slog.Handler
	out     io.Writer
	secrets []string // known non-empty secret values to scrub; never contains ""
}

// newMaskingHandler builds a maskingHandler that renders records with
// slog.NewTextHandler(..., opts) before scrubbing secrets and writing the
// result to out. secrets may contain empty strings; they are dropped here
// so an unset secret (e.g. no Telegram configured) can never turn into a
// rule that matches (and destroys) every log line.
func newMaskingHandler(out io.Writer, opts *slog.HandlerOptions, secrets []string) *maskingHandler {
	nonEmpty := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}

	buf := &bytes.Buffer{}
	return &maskingHandler{
		mu:      &sync.Mutex{},
		buf:     buf,
		inner:   slog.NewTextHandler(buf, opts),
		out:     out,
		secrets: nonEmpty,
	}
}

func (h *maskingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *maskingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}

	text := h.buf.String()
	for _, secret := range h.secrets {
		if strings.Contains(text, secret) {
			text = strings.ReplaceAll(text, secret, maskedPlaceholder)
		}
	}

	_, err := io.WriteString(h.out, text)
	return err
}

func (h *maskingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &maskingHandler{
		mu:      h.mu,
		buf:     h.buf,
		inner:   h.inner.WithAttrs(attrs),
		out:     h.out,
		secrets: h.secrets,
	}
}

func (h *maskingHandler) WithGroup(name string) slog.Handler {
	return &maskingHandler{
		mu:      h.mu,
		buf:     h.buf,
		inner:   h.inner.WithGroup(name),
		out:     h.out,
		secrets: h.secrets,
	}
}

// secretValues extracts every value from a config.Secrets that must never
// appear in plaintext in a log line. TelegramChatID is deliberately excluded
// — it is a numeric chat identifier, not authentication material, and SPEC.md
// Bölüm 13's concern is credentials (API keys/secrets, bot tokens).
func secretValues(s config.Secrets) []string {
	return []string{s.ExchangeAPIKey, s.ExchangeAPISecret, s.TelegramBotToken}
}

// setupLogging wires config.yaml's `log:` block (SPEC.md Bölüm 8:
// "level: info, file: ./data/swingbot.log") into log/slog's global
// default logger, so packages that already call slog.Default()/slog.Info
// etc. (e.g. internal/datafeed) — and any future one — actually honor the
// configured level and land in the configured file instead of silently
// falling back to slog's own zero-value default (info level, stderr,
// never touching log.file). This is pure plumbing: it introduces no new
// logging call sites, only makes the ones that already exist obey config.
//
// An empty log.File keeps logging on stderr (useful for an interactive
// `swingbot backtest` run); a non-empty one additionally writes there,
// which matters most for `swingbot paper start`/`live start` running
// unattended for days (SPEC.md Bölüm 12 Faz 4: "8 hafta kesintisiz
// çalışma") — without this, an overnight crash left no record beyond
// whatever terminal happened to be attached.
//
// secrets is the already-loaded config.Secrets (from .env/environment); its
// non-empty values are the ones maskingHandler scrubs out of every log line
// (SPEC.md Bölüm 13: "Sırlar loglara yazılmıyor (log yazıcıda maskeleme
// var)"). Passing an empty config.Secrets{} (e.g. no .env yet) is fine —
// secretValues + newMaskingHandler simply skip empty values, so logging
// behaves exactly as before masking existed.
func setupLogging(cfg config.LogConfig, secrets config.Secrets) error {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(cfg.Level)) {
	case "", "info":
		level = slog.LevelInfo
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("bilinmeyen log.level: %q (bilinenler: debug, info, warn, error)", cfg.Level)
	}

	var out io.Writer = os.Stderr
	if cfg.File != "" {
		if dir := filepath.Dir(cfg.File); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("log dizini oluşturulamadı (%s): %w", dir, err)
			}
		}
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		// Deliberately not closed: this file must stay open for the
		// process's entire lifetime (it is now the global slog output).
		// swingbot is a short-lived CLI process per invocation (even
		// `paper start`/`live start` exit only on signal/crash), so this
		// is not an accumulating leak — there is exactly one such logger
		// per process.
		out = io.MultiWriter(os.Stderr, f)
	}

	handler := newMaskingHandler(out, &slog.HandlerOptions{Level: level}, secretValues(secrets))
	slog.SetDefault(slog.New(handler))
	return nil
}
