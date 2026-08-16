package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// OrderRequest is what a caller (broker.Submit) hands to an Exchange or
// PaperBroker to place an order. ClientOrderID is the İ5 idempotency key:
// callers generate it themselves (not the exchange), so a retried
// CreateOrder call after a network error can be recognized as a duplicate
// instead of opening a second position.
type OrderRequest struct {
	ClientOrderID string // İ5: idempotency, çağıran üretir
	Symbol        string
	Side          Side
	Type          string // "market" | "limit"
	Qty           decimal.Decimal
	Price         decimal.Decimal // limit için
}

// Order is the exchange's (or PaperBroker's) view of an order after it has
// been submitted, including fill state.
type Order struct {
	ID            string
	ClientOrderID string
	Symbol        string
	Side          Side
	Type          string
	Qty           decimal.Decimal
	Price         decimal.Decimal
	FilledQty     decimal.Decimal
	AvgPrice      decimal.Decimal
	Fee           decimal.Decimal
	Status        string
	CreatedAt     time.Time
}

// IsFilled reports whether the order has been completely filled.
func (o Order) IsFilled() bool {
	return o.FilledQty.GreaterThanOrEqual(o.Qty) && o.Qty.IsPositive()
}
