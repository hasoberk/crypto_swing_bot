package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// Market describes a tradable symbol on an exchange: its quote/base assets
// and the exchange filters (tick/step/min notional) that any order for it
// must respect.
type Market struct {
	Symbol      string // "BTC/USDT"
	Base        string
	Quote       string
	Active      bool
	TickSize    decimal.Decimal // fiyat adımı
	StepSize    decimal.Decimal // miktar adımı
	MinNotional decimal.Decimal // minimum emir tutarı
	ListedAt    time.Time
	DelistedAt  time.Time // zero ise aktif
}

// IsListedAt reports whether the market was active (listed, not yet
// delisted) at instant t. Used by universe.Build to reconstruct the
// tradable universe of a past date without survivorship bias.
func (m Market) IsListedAt(t time.Time) bool {
	if !m.ListedAt.IsZero() && t.Before(m.ListedAt) {
		return false
	}
	if !m.DelistedAt.IsZero() && !t.Before(m.DelistedAt) {
		return false
	}
	return true
}
