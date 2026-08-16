// Package domain contains the pure data types shared across the whole
// system: strategy, risk, broker, backtest, engine, etc. all speak this
// vocabulary.
//
// Binding rule (SPEC.md Bölüm 3, İ1): this package depends on nothing but
// the Go standard library and github.com/shopspring/decimal. It MUST NOT
// import store, exchange, broker or any other internal package. That
// asymmetry is what lets the compiler enforce "one code path" (İ1): a
// package that only sees domain (and indicator) cannot accidentally reach
// into a store or a live exchange connection.
package domain

import "time"

// Candle is a single OHLCV bar.
//
// Prices/volumes are float64 on purpose (per SPEC.md Bölüm 5.1) — they are
// read-only market data, not order quantities/prices that get rounded and
// sent to an exchange. Anything that represents an amount we actually
// trade (OrderRequest.Qty/Price, Position.Qty, ...) uses decimal.Decimal
// instead.
type Candle struct {
	OpenTime    time.Time
	Open        float64
	High        float64
	Low         float64
	Close       float64
	Volume      float64
	QuoteVolume float64
}
