package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"swingbot/internal/config"
)

// TestMaskingHandler_ScrubsKnownSecrets verifies that a secret value passed
// as an ordinary slog attribute never reaches the underlying writer in
// plaintext, whether it's the direct attribute value or buried inside a
// wrapped error string (the realistic case this defends against — see
// logging.go's maskingHandler doc comment).
func TestMaskingHandler_ScrubsKnownSecrets(t *testing.T) {
	const apiKey = "gizli-deger-123"
	const apiSecret = "cok-gizli-hmac-secret-456"
	const botToken = "123456:AA-telegram-bot-token"

	var out bytes.Buffer
	h := newMaskingHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}, []string{apiKey, apiSecret, botToken})
	logger := slog.New(h)

	logger.Info("test", "token", apiKey)

	wrapped := errors.New("binance yanıtı: signature=" + apiSecret + " geçersiz")
	logger.Error("borsa isteği başarısız", "err", wrapped)

	logger.Warn("telegram gönderilemedi", "detay", "bot"+botToken+"üzerinden ulaşılamadı")

	got := out.String()

	for _, secret := range []string{apiKey, apiSecret, botToken} {
		if strings.Contains(got, secret) {
			t.Fatalf("log çıktısı sır değerini düz metin içeriyor (%q):\n%s", secret, got)
		}
	}

	if wantCount := strings.Count(got, maskedPlaceholder); wantCount != 3 {
		t.Fatalf("beklenen 3 maskeleme yer tutucusu, bulunan %d:\n%s", wantCount, got)
	}
}

// TestMaskingHandler_EmptySecretsDoNotBreakLogging ensures an unset secret
// (empty string — e.g. Telegram not configured) is never turned into a
// masking rule; an empty-string rule would match (and destroy) every log
// line, since strings.Contains(anything, "") is always true.
func TestMaskingHandler_EmptySecretsDoNotBreakLogging(t *testing.T) {
	var out bytes.Buffer
	h := newMaskingHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}, []string{"", "", ""})
	logger := slog.New(h)

	logger.Info("normal log satırı", "sembol", "BTC/USDT", "fiyat", 65000)

	got := out.String()
	if strings.Contains(got, maskedPlaceholder) {
		t.Fatalf("boş sır değerleri normal logu bozmamalı, ama maskeleme oldu:\n%s", got)
	}
	if !strings.Contains(got, "normal log satırı") || !strings.Contains(got, "BTC/USDT") {
		t.Fatalf("beklenen içerik log çıktısında yok:\n%s", got)
	}
}

// TestSecretValues_ExcludesTelegramChatID documents/locks in that the chat
// ID is deliberately not treated as a secret (SPEC.md Bölüm 13's concern is
// authentication material, not a numeric channel identifier).
func TestSecretValues_ExcludesTelegramChatID(t *testing.T) {
	secrets := config.Secrets{
		ExchangeAPIKey:    "key",
		ExchangeAPISecret: "secret",
		TelegramBotToken:  "token",
		TelegramChatID:    "-100123456789",
	}

	for _, v := range secretValues(secrets) {
		if v == secrets.TelegramChatID {
			t.Fatalf("TelegramChatID sır listesine dahil edilmemeli")
		}
	}
}

// TestSetupLogging_NoSecrets exercises setupLogging itself (not just the
// handler in isolation) with an empty config.Secrets{}, confirming the
// level/file wiring this function already did still works once masking is
// layered on top of it.
func TestSetupLogging_NoSecrets(t *testing.T) {
	if err := setupLogging(config.LogConfig{Level: "debug"}, config.Secrets{}); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	// setupLogging sets the global default logger; restore a harmless
	// default afterwards so this test doesn't leak state into others.
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})

	if err := setupLogging(config.LogConfig{Level: "yanlış-seviye"}, config.Secrets{}); err == nil {
		t.Fatalf("bilinmeyen log.level için hata bekleniyordu, nil geldi")
	}
}
