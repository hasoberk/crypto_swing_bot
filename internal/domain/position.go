package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// Position is a currently open holding in a symbol.
type Position struct {
	Symbol     string
	Qty        decimal.Decimal
	EntryPrice float64
	EntryTime  time.Time
	StopPrice  float64
	HighWater  float64 // trailing stop için görülen en yüksek fiyat
	Strategy   string
}

// UnrealizedPnL returns the mark-to-market profit/loss in quote terms at
// the given last price.
func (p Position) UnrealizedPnL(lastPrice float64) float64 {
	qty, _ := p.Qty.Float64()
	return qty * (lastPrice - p.EntryPrice)
}
