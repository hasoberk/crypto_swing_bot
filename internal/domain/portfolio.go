package domain

// Portfolio is a point-in-time snapshot of account state as seen by a
// Broker: available cash, open positions, and total equity.
type Portfolio struct {
	Cash      float64
	Positions map[string]Position
	Equity    float64 // Cash + pozisyonların güncel değeri
}

// Exposure returns the fraction of Equity currently held in positions
// (Equity - Cash) / Equity, in [0, 1] under normal conditions. Returns 0
// if Equity is zero to avoid a division by zero.
func (p Portfolio) Exposure() float64 {
	if p.Equity == 0 {
		return 0
	}
	return (p.Equity - p.Cash) / p.Equity
}
