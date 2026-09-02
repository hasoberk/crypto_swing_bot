// Go/no-go thresholds (SPEC.md Bölüm 11.4).
//
// The single rule this file exists to enforce in code, not just in prose:
// Thresholds must be written down BEFORE anyone looks at the locked
// segment (SPEC.md Bölüm 11.1) or at walk-forward results. See locked.go's
// ViewLockedSegment, which refuses to proceed unless a Thresholds value
// has already been recorded.
package backtest

import "fmt"

// Thresholds is SPEC.md Bölüm 11.4's five go/no-go criteria. Every field
// mirrors exactly one SPEC.md Bölüm 11.4 bullet; there is no sixth
// criterion, and none of the five are optional once recorded.
type Thresholds struct {
	// MinTradeCount: "İşlem sayısı >= 50 (istatistiksel anlam için)."
	MinTradeCount int
	// RequireBeatBenchmarkInAnyRegime: "Walk-forward birleşik getirisi,
	// BTC al-tut'u en az bir rejimde geçmeli." Kept as an explicit field
	// (not a hardcoded assumption) so a recorded Thresholds value is a
	// complete, self-contained written record of what was decided.
	RequireBeatBenchmarkInAnyRegime bool
	// RequireLowerMaxDrawdownThanBenchmark: "Maksimum düşüş, BTC al-tut'un
	// maksimum düşüşünden düşük olmalı."
	RequireLowerMaxDrawdownThanBenchmark bool
	// RequirePositiveAt2xCosts: "Komisyon 2x yapıldığında hâlâ pozitif
	// olmalı."
	RequirePositiveAt2xCosts bool
	// RequireParamPlateau: "Parametre platosu mevcut olmalı."
	RequireParamPlateau bool
}

// DefaultThresholds returns SPEC.md Bölüm 11.4's literal criteria. The
// only numeric knob among them is MinTradeCount (50); the other four are
// pass/fail rules, not tunable numbers.
//
// A caller MAY raise MinTradeCount for a stricter bar before ever looking
// at results. A caller must NOT loosen it, or flip any Require* field to
// false, after seeing results — SPEC.md Bölüm 11.4: "Bu eşikler
// sağlanmıyorsa strateji terk edilir... Aynı veri üzerinde onuncu
// denemende bulduğun 'çalışan' strateji, çalışan bir strateji değil,
// veriye uydurulmuş bir eğridir."
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinTradeCount:                        50,
		RequireBeatBenchmarkInAnyRegime:      true,
		RequireLowerMaxDrawdownThanBenchmark: true,
		RequirePositiveAt2xCosts:             true,
		RequireParamPlateau:                  true,
	}
}

// VerdictCriterion is one Thresholds row's pass/fail outcome.
type VerdictCriterion struct {
	Name   string
	Passed bool
	Detail string
}

// Verdict is EvaluateThresholds' answer: GEÇTİ (every criterion passed) or
// TERK EDİLDİ (SPEC.md Bölüm 11.4 — any single failed criterion means the
// strategy is abandoned, not iterated on with this same data).
type Verdict struct {
	Criteria []VerdictCriterion
	Passed   bool
}

// EvaluateThresholds grades a completed WalkForwardResult against t
// (which MUST already have been recorded before wf was computed — see
// locked.go). costs2x is the SAME walk-forward re-run's combined Metrics
// at doubled broker.Costs (the caller runs that separately; this function
// only grades the outcome, so it stays a pure, easily-tested function that
// never re-runs anything itself). plateaus is one PlateauVerdict per swept
// parameter (SPEC.md Bölüm 11.3) — ALL of them must show a plateau for
// criterion 5 to pass: a single sharp peak among several swept parameters
// still means the strategy built around that value is fragile.
func EvaluateThresholds(t Thresholds, wf *WalkForwardResult, plateaus []PlateauVerdict, costs2x Metrics) Verdict {
	var v Verdict

	tradeCount := len(wf.CombinedTrades)
	v.Criteria = append(v.Criteria, VerdictCriterion{
		Name:   "İşlem sayısı",
		Passed: tradeCount >= t.MinTradeCount,
		Detail: fmt.Sprintf("%d işlem (eşik >= %d)", tradeCount, t.MinTradeCount),
	})

	beatAny, beatDetail := beatsBenchmarkInAnyRegime(wf)
	v.Criteria = append(v.Criteria, VerdictCriterion{
		Name:   "Rejim bazlı BTC karşılaştırması",
		Passed: !t.RequireBeatBenchmarkInAnyRegime || beatAny,
		Detail: beatDetail,
	})

	combinedBTC := Compute(wf.CombinedBenchBTC, nil)
	// Both MaxDrawdown values are negative fractions (or zero); "düşük" =
	// less negative, i.e. strictly greater than the benchmark's.
	ddOK := wf.Metrics.MaxDrawdown > combinedBTC.MaxDrawdown
	v.Criteria = append(v.Criteria, VerdictCriterion{
		Name:   "Maksimum düşüş",
		Passed: !t.RequireLowerMaxDrawdownThanBenchmark || ddOK,
		Detail: fmt.Sprintf("strateji %.2f%%, BTC al-tut %.2f%%", wf.Metrics.MaxDrawdown*100, combinedBTC.MaxDrawdown*100),
	})

	v.Criteria = append(v.Criteria, VerdictCriterion{
		Name:   "Komisyon 2x duyarlılığı",
		Passed: !t.RequirePositiveAt2xCosts || costs2x.TotalReturn > 0,
		Detail: fmt.Sprintf("2x maliyetle toplam getiri %.2f%%", costs2x.TotalReturn*100),
	})

	plateauOK := len(plateaus) > 0
	var sharp []string
	for _, p := range plateaus {
		if !p.IsPlateau {
			plateauOK = false
			sharp = append(sharp, p.Param)
		}
	}
	plateauDetail := fmt.Sprintf("%d parametre tarandı", len(plateaus))
	if len(sharp) > 0 {
		plateauDetail += fmt.Sprintf(", keskin tepe: %v", sharp)
	}
	if len(plateaus) == 0 {
		plateauDetail = "hiç parametre taranmadı — plato belgelenemedi"
	}
	v.Criteria = append(v.Criteria, VerdictCriterion{
		Name:   "Parametre platosu",
		Passed: !t.RequireParamPlateau || plateauOK,
		Detail: plateauDetail,
	})

	v.Passed = true
	for _, c := range v.Criteria {
		if !c.Passed {
			v.Passed = false
		}
	}
	return v
}

// beatsBenchmarkInAnyRegime groups wf.Windows by Regime and reports
// whether the strategy's mean per-window test return beats BTC's mean
// per-window test return in at least one regime bucket (SPEC.md Bölüm
// 11.4: "en az bir rejimde").
func beatsBenchmarkInAnyRegime(wf *WalkForwardResult) (bool, string) {
	type bucket struct {
		stratReturnSum, btcReturnSum float64
		windows                      int
	}
	buckets := map[Regime]*bucket{}
	for _, w := range wf.Windows {
		if len(w.TestEquity) == 0 || len(w.TestBenchBTC) == 0 {
			continue
		}
		stratRet := w.TestEquity[len(w.TestEquity)-1].Equity/w.TestEquity[0].Equity - 1
		btcRet := w.TestBenchBTC[len(w.TestBenchBTC)-1].Equity/w.TestBenchBTC[0].Equity - 1
		b, ok := buckets[w.Regime]
		if !ok {
			b = &bucket{}
			buckets[w.Regime] = b
		}
		b.stratReturnSum += stratRet
		b.btcReturnSum += btcRet
		b.windows++
	}

	any := false
	var details []string
	for _, regime := range []Regime{RegimeBull, RegimeBear, RegimeSideways} {
		b, ok := buckets[regime]
		if !ok || b.windows == 0 {
			continue
		}
		stratAvg := b.stratReturnSum / float64(b.windows)
		btcAvg := b.btcReturnSum / float64(b.windows)
		beat := stratAvg > btcAvg
		if beat {
			any = true
		}
		mark := ""
		if beat {
			mark = " ✓"
		}
		details = append(details, fmt.Sprintf("%s: strateji %.2f%% vs BTC %.2f%% (%d pencere)%s",
			regime, stratAvg*100, btcAvg*100, b.windows, mark))
	}
	if len(details) == 0 {
		return false, "hiçbir pencerede rejim/karşılaştırma verisi yok"
	}
	return any, joinSemicolon(details)
}

func joinSemicolon(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
