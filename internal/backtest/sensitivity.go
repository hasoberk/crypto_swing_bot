// Parameter sensitivity sweeps and plateau detection (SPEC.md Bölüm 11.3).
//
// "Aradığın şey bir tepe değil, bir plato." — this file's whole purpose is
// turning that sentence into a number: run one full backtest per (axis,
// value) pair, then look at whether the values immediately next to the
// best one are close to it (plateau, plausibly real) or far from it
// (sharp peak, plausibly a coincidence of this exact dataset).
package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"

	"swingbot/internal/broker"
	"swingbot/internal/domain"
	"swingbot/internal/risk"
)

// ParamAxis is one parameter's sweep range for sensitivity analysis.
type ParamAxis struct {
	Name   string
	Values []float64
}

// SensitivityPoint is one (parameter, value) sweep result.
type SensitivityPoint struct {
	Param   string
	Value   float64
	Metrics Metrics
}

// SensitivityConfig configures ParamSensitivitySweep: one full-period
// backtest.Run per (parameter, value) combination (SPEC.md Bölüm 11.3).
type SensitivityConfig struct {
	NewStrategy StrategyFactory
	// BaseParams is the center point every axis sweeps around: for a given
	// axis, every OTHER parameter is held at BaseParams' value while the
	// swept parameter takes each of that axis's Values in turn — classic
	// one-parameter-at-a-time sensitivity analysis, matching SPEC.md
	// Bölüm 11.3's own example ("atr_stop_mult = 2.5'te Sharpe 1.8, 2.4 ve
	// 2.6'da 0.4"): only atr_stop_mult moves, nothing else does.
	BaseParams ParamSet
	Axes       []ParamAxis

	Candles     map[string][]domain.Candle
	Universe    []string
	Markets     map[string]domain.Market
	InitialCash float64
	Costs       broker.Costs
	RiskGate    RiskGate
	Breaker     *risk.Breaker
}

// ParamSensitivitySweep runs cfg.Axes against cfg.BaseParams and returns
// one SensitivityPoint per (axis, value) pair actually evaluated. A value
// whose sub-backtest fails outright (e.g. an infeasible warmup requirement
// for this date range) is silently omitted rather than aborting the whole
// sweep — a heatmap with one missing cell is far more useful than no
// heatmap at all.
func ParamSensitivitySweep(ctx context.Context, cfg SensitivityConfig) ([]SensitivityPoint, error) {
	if cfg.NewStrategy == nil {
		return nil, fmt.Errorf("backtest: SensitivityConfig.NewStrategy is required")
	}
	if len(cfg.Candles) == 0 {
		return nil, fmt.Errorf("backtest: SensitivityConfig.Candles is empty")
	}

	var out []SensitivityPoint
	for _, axis := range cfg.Axes {
		for _, v := range axis.Values {
			ps := cfg.BaseParams.Clone()
			ps[axis.Name] = v

			strat, err := cfg.NewStrategy(ps)
			if err != nil {
				return nil, fmt.Errorf("backtest: NewStrategy(%s=%v): %w", axis.Name, v, err)
			}

			res, err := Run(ctx, Config{
				Strategy: strat, Candles: cfg.Candles, Universe: cfg.Universe, Markets: cfg.Markets,
				InitialCash: cfg.InitialCash, Costs: cfg.Costs, RiskGate: cfg.RiskGate, Breaker: cfg.Breaker,
				Mode: "backtest",
			})
			if err != nil {
				continue
			}
			out = append(out, SensitivityPoint{Param: axis.Name, Value: v, Metrics: res.Metrics})
		}
	}
	return out, nil
}

// GroupByParam splits ParamSensitivitySweep's flat output back into one
// slice per axis, in first-seen order — DetectPlateau needs the points
// belonging to a single parameter, not the whole sweep at once.
func GroupByParam(points []SensitivityPoint) (order []string, byParam map[string][]SensitivityPoint) {
	byParam = make(map[string][]SensitivityPoint)
	for _, p := range points {
		if _, ok := byParam[p.Param]; !ok {
			order = append(order, p.Param)
		}
		byParam[p.Param] = append(byParam[p.Param], p)
	}
	return order, byParam
}

// PlateauVerdict is DetectPlateau's answer for one parameter axis.
type PlateauVerdict struct {
	Param     string
	IsPlateau bool
	BestValue float64
	BestScore float64
	// RelativeDrop is (best - neighborAvg) / |best|: how far the values
	// immediately next to the best one fall, relative to the best itself.
	// SPEC.md's own example (Sharpe 1.8 at the center vs 0.4 at both
	// neighbors, an ~78% relative drop) is called out as "a coincidence";
	// its other example (0.9–1.1 across a wide neighborhood, an ~18% drop)
	// is called "may be real".
	RelativeDrop float64
	Detail       string
}

// DefaultPlateauDropThreshold is the maximum RelativeDrop DetectPlateau
// still calls a plateau. Fixed and documented here — SPEC.md Bölüm 11.4's
// "önceden yaz, sonuçları gördükten sonra değil" rule applies just as much
// to this heuristic as to the go/no-go thresholds themselves; it must not
// be nudged after looking at any particular sweep's numbers.
const DefaultPlateauDropThreshold = 0.35

// DetectPlateau scores axis's points (all sharing one Param — see
// GroupByParam) with metricOf (typically Sharpe or Calmar; SPEC.md Bölüm
// 7.4 flags Sharpe as noisy for crypto's fat tails, so callers may prefer
// Calmar), finds the best-scoring value, and classifies the immediate
// neighborhood around it as a plateau or a sharp, likely-overfit peak.
//
// With fewer than 3 points there is no neighborhood to judge at all;
// DetectPlateau returns IsPlateau=false with an explanatory Detail — this
// is "cannot confirm a plateau from too little data", not a claim that a
// peak was found.
func DetectPlateau(param string, points []SensitivityPoint, metricOf func(Metrics) float64, dropThreshold float64) PlateauVerdict {
	if dropThreshold <= 0 {
		dropThreshold = DefaultPlateauDropThreshold
	}
	if len(points) < 3 {
		return PlateauVerdict{
			Param: param, IsPlateau: false,
			Detail: fmt.Sprintf("yalnızca %d nokta — plato/tepe ayrımı için en az 3 komşu değer gerekir", len(points)),
		}
	}

	sorted := make([]SensitivityPoint, len(points))
	copy(sorted, points)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Value < sorted[j].Value })

	bestIdx := 0
	bestScore := metricOf(sorted[0].Metrics)
	for i := 1; i < len(sorted); i++ {
		if s := metricOf(sorted[i].Metrics); s > bestScore {
			bestScore, bestIdx = s, i
		}
	}

	var neighborSum float64
	var neighborCount int
	if bestIdx > 0 {
		neighborSum += metricOf(sorted[bestIdx-1].Metrics)
		neighborCount++
	}
	if bestIdx < len(sorted)-1 {
		neighborSum += metricOf(sorted[bestIdx+1].Metrics)
		neighborCount++
	}
	if neighborCount == 0 {
		// Unreachable given len(points) >= 3, kept defensively.
		return PlateauVerdict{Param: param, IsPlateau: false, BestValue: sorted[bestIdx].Value, BestScore: bestScore,
			Detail: "en iyi değerin komşusu yok — plato doğrulanamıyor"}
	}
	neighborAvg := neighborSum / float64(neighborCount)

	var relDrop float64
	if bestScore != 0 {
		relDrop = (bestScore - neighborAvg) / math.Abs(bestScore)
	}
	isPlateau := relDrop <= dropThreshold

	detail := fmt.Sprintf(
		"en iyi %s=%.4g (skor %.4f), komşu ortalaması %.4f, göreli düşüş %.1f%% (eşik %.0f%%)",
		param, sorted[bestIdx].Value, bestScore, neighborAvg, relDrop*100, dropThreshold*100,
	)
	if !isPlateau {
		detail += " — TEPE (keskin), plato değil: bu değer tesadüfi olabilir (SPEC.md Bölüm 11.3)"
	}
	return PlateauVerdict{
		Param: param, IsPlateau: isPlateau, BestValue: sorted[bestIdx].Value, BestScore: bestScore,
		RelativeDrop: relDrop, Detail: detail,
	}
}
