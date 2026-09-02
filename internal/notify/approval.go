package notify

import (
	"fmt"
	"strings"
	"time"
)

// Status mirrors store.ProposalStatus's string values (this package does
// not import internal/store — see notifier.go's binding-rule comment — so
// the enum is duplicated here rather than shared; both sides are simple
// string constants and SPEC.md Bölüm 4.1/6.8 is the single source of truth
// either was transcribed from).
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusApproved  Status = "APPROVED"
	StatusRejected  Status = "REJECTED"
	StatusExpired   Status = "EXPIRED"
	StatusSubmitted Status = "SUBMITTED"
	StatusFilled    Status = "FILLED"
	StatusFailed    Status = "FAILED"
)

// validNext is SPEC.md Bölüm 6.8's state machine:
//
//	PENDING --onayla--> APPROVED --emir--> SUBMITTED --dolum--> FILLED
//	   │                                        │
//	   ├──reddet--> REJECTED                    └──hata--> FAILED
//	   └──zaman aşımı--> EXPIRED
var validNext = map[Status]map[Status]bool{
	StatusPending:   {StatusApproved: true, StatusRejected: true, StatusExpired: true},
	StatusApproved:  {StatusSubmitted: true},
	StatusSubmitted: {StatusFilled: true, StatusFailed: true},
}

// CanTransition reports whether the SPEC.md Bölüm 6.8 state machine allows
// moving a proposal from `from` to `to`. Callers (internal/engine) should
// check this before persisting a status change — it is the single place
// that state machine is encoded, so engine and any future transport agree
// on exactly the same legal transitions.
func CanTransition(from, to Status) bool {
	return validNext[from][to]
}

// FormatProposalMessage renders SPEC.md Bölüm 6.8's exact template for an
// entry proposal (the "🟢 GİRİŞ ÖNERİSİ" message). It never truncates
// Reason (İ6): the whole rationale always reaches the operator.
func FormatProposalMessage(p Proposal) string {
	var b strings.Builder

	fmt.Fprintf(&b, "🟢 GİRİŞ ÖNERİSİ · %s\n\n", p.Symbol)
	fmt.Fprintf(&b, "Strateji   %s\n", p.Strategy)
	fmt.Fprintf(&b, "Referans   %.2f %s (%s)\n", p.RefPrice, p.QuoteAsset, formatAsOfLabel(p.RefPriceAsOf))
	if p.StopPrice > 0 && p.RefPrice > 0 {
		stopPct := (p.StopPrice/p.RefPrice - 1) * 100
		fmt.Fprintf(&b, "Stop       %.2f %s (%.1f%%)\n", p.StopPrice, p.QuoteAsset, stopPct)
	}
	fmt.Fprintf(&b, "Miktar     %s ≈ %.0f %s\n", p.QtyDisplay, p.NotionalQuote, p.QuoteAsset)
	fmt.Fprintf(&b, "Risk       %.0f %s (equity'nin %%%.1f'i)\n\n", p.RiskAmount, p.QuoteAsset, p.RiskPct*100)

	b.WriteString("Gerekçe\n")
	b.WriteString(p.Reason)
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Portföy sonrası\n")
	fmt.Fprintf(&b, "Pozisyon %d/%d · Maruziyet %%%.0f · Nakit %.0f %s\n\n",
		p.PortfolioAfter.OpenPositions, p.PortfolioAfter.MaxPositions,
		p.PortfolioAfter.ExposurePct*100, p.PortfolioAfter.CashAfter, p.QuoteAsset)

	fmt.Fprintf(&b, "Geçerlilik: %s\n", formatRemaining(p.ExpiresAt.Sub(p.AsOf)))

	return b.String()
}

// formatAsOfLabel renders a candle timestamp the way SPEC.md Bölüm 6.8's
// example does ("14 Ağu kapanış").
func formatAsOfLabel(t time.Time) string {
	if t.IsZero() {
		return "kapanış"
	}
	months := [...]string{"Oca", "Şub", "Mar", "Nis", "May", "Haz", "Tem", "Ağu", "Eyl", "Eki", "Kas", "Ara"}
	return fmt.Sprintf("%d %s kapanış", t.Day(), months[t.Month()-1])
}

// formatRemaining renders a duration the way SPEC.md Bölüm 6.8's "Geçerlilik:
// 4 saat" line does — whole hours when d is an exact number of hours (the
// common case, config.yaml's approval_ttl_hours), otherwise falls back to
// Go's own duration formatting so a non-whole-hour TTL is never lied about.
func formatRemaining(d time.Duration) string {
	if d <= 0 {
		return "süresi doldu"
	}
	if d%time.Hour == 0 {
		h := int(d / time.Hour)
		if h == 1 {
			return "1 saat"
		}
		return fmt.Sprintf("%d saat", h)
	}
	return d.Round(time.Minute).String()
}

// CallbackApprove/CallbackReject are the inline-keyboard callback_data
// prefixes SPEC.md Bölüm 6.8's "[ ✅ Onayla ] [ ❌ Reddet ]" buttons carry,
// followed by the proposal ID (e.g. "approve:01J...", "reject:01J...").
// telegram.go registers handlers on exactly these prefixes.
const (
	CallbackApprove = "approve"
	CallbackReject  = "reject"
)

// EncodeCallback builds the callback_data for action ("approve"/"reject")
// on proposalID.
func EncodeCallback(action, proposalID string) string {
	return action + ":" + proposalID
}

// ParseCallback splits a callback_data string back into its action and
// proposal ID. ok is false for anything that does not match
// "<action>:<id>" with a non-empty id.
func ParseCallback(data string) (action, proposalID string, ok bool) {
	action, proposalID, found := strings.Cut(data, ":")
	if !found || action == "" || proposalID == "" {
		return "", "", false
	}
	return action, proposalID, true
}

// FormatSummary renders SPEC.md Bölüm 6.7 adım 15's daily summary — İ3
// requires every performance output to carry a benchmark alongside it, so
// benchStrategyPct/benchBTCPct/benchTop10Pct (cumulative return since the
// paper/live run began, as fractions) are mandatory parameters rather than
// optional trailing ones.
func FormatSummary(asOf time.Time, equity, cash float64, exposurePct, stratCumPct, btcCumPct, top10CumPct float64, openPositions int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 Günlük özet · %s\n\n", asOf.Format("2006-01-02"))
	fmt.Fprintf(&b, "Equity     %.0f (nakit %.0f, maruziyet %%%.0f)\n", equity, cash, exposurePct*100)
	fmt.Fprintf(&b, "Açık pozisyon: %d\n\n", openPositions)
	fmt.Fprintf(&b, "Getiri (kümülatif)\n")
	fmt.Fprintf(&b, "  Strateji     %+.2f%%\n", stratCumPct*100)
	fmt.Fprintf(&b, "  BTC al-tut   %+.2f%%\n", btcCumPct*100)
	fmt.Fprintf(&b, "  Top-10 eşit  %+.2f%%\n", top10CumPct*100)
	return b.String()
}

// FormatStopTriggered renders the "stop tetiklenmesi" event notification
// (SPEC.md Bölüm 6.8's list of always-notified events).
func FormatStopTriggered(symbol string, exitPrice, pnlQuote, pnlPct float64, quoteAsset string) string {
	return fmt.Sprintf("Sembol      %s\nÇıkış fiyatı %.2f %s\nK/Z         %.2f %s (%+.1f%%)",
		symbol, exitPrice, quoteAsset, pnlQuote, quoteAsset, pnlPct*100)
}

// FormatBreakerTripped renders İ7's mandatory breaker-trip notification.
func FormatBreakerTripped(reason, detail string, at time.Time) string {
	return fmt.Sprintf("Gerekçe   %s\nDetay     %s\nZaman     %s UTC\n\nYeni giriş engellendi. Çıkışlar/stoplar çalışmaya devam ediyor.\nKapatmak için: swingbot breaker reset --confirm",
		reason, detail, at.UTC().Format("2006-01-02 15:04:05"))
}
