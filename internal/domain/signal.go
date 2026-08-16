package domain

import "time"

// Side is the direction of an order.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// SignalKind is the kind of intent a Strategy expresses for a symbol on a
// given AsOf date.
type SignalKind string

const (
	SignalEnter SignalKind = "enter"
	SignalExit  SignalKind = "exit"
	SignalStop  SignalKind = "stop_update"
)

// Signal is a strategy's raw intent. It deliberately carries no quantity —
// sizing is risk's job, not strategy's. A strategy never knows how much
// capital is available; only risk.Sizer turns a Signal + Portfolio into an
// actual order quantity. Do not add a Qty field here: it would let a
// strategy silently take over sizing decisions and break the İ1/risk
// separation the whole system depends on.
type Signal struct {
	AsOf      time.Time // sinyali üreten mumun OpenTime'ı
	Symbol    string
	Kind      SignalKind
	Score     float64            // sıralama için; enter sinyallerinde anlamlı
	RefPrice  float64            // sinyal mumunun kapanışı
	StopPrice float64            // enter için zorunlu
	Reason    string             // İ6: insan okunur gerekçe
	Metrics   map[string]float64 // karara giren tüm değerler
}
