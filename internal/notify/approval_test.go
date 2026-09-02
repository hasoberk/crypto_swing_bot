package notify

import (
	"strings"
	"testing"
	"time"
)

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusPending, StatusApproved, true},
		{StatusPending, StatusRejected, true},
		{StatusPending, StatusExpired, true},
		{StatusPending, StatusSubmitted, false}, // must go through APPROVED first
		{StatusApproved, StatusSubmitted, true},
		{StatusApproved, StatusFilled, false}, // must go through SUBMITTED
		{StatusSubmitted, StatusFilled, true},
		{StatusSubmitted, StatusFailed, true},
		{StatusSubmitted, StatusApproved, false}, // no going backwards
		{StatusRejected, StatusApproved, false},  // terminal
		{StatusExpired, StatusApproved, false},   // terminal
		{StatusFilled, StatusSubmitted, false},   // terminal
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestParseCallback(t *testing.T) {
	action, id, ok := ParseCallback(EncodeCallback(CallbackApprove, "prop-123"))
	if !ok || action != CallbackApprove || id != "prop-123" {
		t.Fatalf("ParseCallback round-trip = (%q, %q, %v)", action, id, ok)
	}

	for _, bad := range []string{"", "noaction", "approve:", ":prop-123", "approve"} {
		if _, _, ok := ParseCallback(bad); ok {
			t.Errorf("ParseCallback(%q) ok = true, want false", bad)
		}
	}
}

func TestFormatProposalMessageIncludesReasonVerbatim(t *testing.T) {
	// İ6: the full rationale must reach the operator, unmodified/untruncated.
	reason := "20 günlük kırılım. Fiyat SMA200'ün %18 üzerinde.\nATR(14)/fiyat = %4.0 (limit %8). Evren skoru: 2. / 47"
	p := Proposal{
		ID: "prop-1", Symbol: "SOL/USDT", Strategy: "trendfollow",
		RefPrice: 142.30, StopPrice: 128.07, QtyDisplay: "12.6 SOL",
		NotionalQuote: 1793, QuoteAsset: "USDT", RiskAmount: 180, RiskPct: 0.01,
		Reason:         reason,
		PortfolioAfter: PortfolioAfter{OpenPositions: 3, MaxPositions: 5, ExposurePct: 0.62, CashAfter: 6810},
		AsOf:           time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
		ExpiresAt:      time.Date(2026, 8, 14, 4, 0, 0, 0, time.UTC),
	}
	msg := FormatProposalMessage(p)

	for _, want := range []string{
		"🟢 GİRİŞ ÖNERİSİ · SOL/USDT",
		"Strateji   trendfollow",
		reason,
		"Pozisyon 3/5",
		"Maruziyet %62",
		"Nakit 6810 USDT",
		"Geçerlilik: 4 saat",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("FormatProposalMessage missing %q, got:\n%s", want, msg)
		}
	}
}

func TestFormatRemaining(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{4 * time.Hour, "4 saat"},
		{1 * time.Hour, "1 saat"},
		{0, "süresi doldu"},
		{-time.Minute, "süresi doldu"},
	}
	for _, c := range cases {
		if got := formatRemaining(c.d); got != c.want {
			t.Errorf("formatRemaining(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestFormatSummaryIncludesBenchmarks(t *testing.T) {
	// İ3: no performance output without a benchmark alongside it.
	msg := FormatSummary(time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC), 11500, 4000, 0.65, 0.15, 0.22, 0.10, 3)
	for _, want := range []string{"Strateji", "BTC al-tut", "Top-10 eşit", "+15.00%", "+22.00%", "+10.00%"} {
		if !strings.Contains(msg, want) {
			t.Errorf("FormatSummary missing %q, got:\n%s", want, msg)
		}
	}
}
