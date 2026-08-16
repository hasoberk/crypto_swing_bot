package universe

import (
	"testing"
	"time"

	"swingbot/internal/domain"
)

var testAsOf = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

// makeCandles builds n daily candles ending at (and including) asOf,
// ascending by OpenTime. quoteVolumeAt/volumeAt let individual tests control
// the two fields Filter's checks actually look at (quote_volume for the
// liquidity median, volume for the zero-volume quality flag); every other
// OHLC field is a flat, always-valid bar (High==Low==Close==Open) since
// Filter's checks under test never touch price shape.
func makeCandles(asOf time.Time, n int, quoteVolumeAt func(i int) float64, volumeAt func(i int) float64) []domain.Candle {
	out := make([]domain.Candle, n)
	for i := 0; i < n; i++ {
		ot := asOf.AddDate(0, 0, -(n - 1 - i))
		v := 1.0
		if volumeAt != nil {
			v = volumeAt(i)
		}
		qv := 10_000_000.0
		if quoteVolumeAt != nil {
			qv = quoteVolumeAt(i)
		}
		out[i] = domain.Candle{
			OpenTime: ot, Open: 100, High: 100, Low: 100, Close: 100,
			Volume: v, QuoteVolume: qv,
		}
	}
	return out
}

func constVolume(v float64) func(int) float64 { return func(int) float64 { return v } }

func healthyMarket(symbol, base string) domain.Market {
	return domain.Market{
		Symbol:   symbol,
		Base:     base,
		Quote:    "USDT",
		Active:   true,
		ListedAt: testAsOf.AddDate(0, 0, -400),
	}
}

func TestFilter_IncludesHealthyCandidate(t *testing.T) {
	c := Candidate{
		Market:  healthyMarket("ABC/USDT", "ABC"),
		Candles: makeCandles(testAsOf, 400, constVolume(10_000_000), nil),
	}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})

	if len(res.Excluded) != 0 {
		t.Fatalf("expected no exclusions, got %+v", res.Excluded)
	}
	if len(res.Included) != 1 {
		t.Fatalf("expected 1 included symbol, got %d", len(res.Included))
	}
	got := res.Included[0]
	if got.Symbol != "ABC/USDT" || got.Base != "ABC" {
		t.Errorf("unexpected included symbol: %+v", got)
	}
	if got.MedianQuoteVolume30 != 10_000_000 {
		t.Errorf("MedianQuoteVolume30 = %v, want 10_000_000", got.MedianQuoteVolume30)
	}
}

func TestFilter_ExcludesLeveragedToken(t *testing.T) {
	cases := []string{"BTCUP/USDT", "BTCDOWN/USDT", "ETHBULL/USDT", "ETHBEAR/USDT", "BTC3L/USDT", "BTC3S/USDT"}
	for _, symbol := range cases {
		t.Run(symbol, func(t *testing.T) {
			base := symbol[:len(symbol)-len("/USDT")]
			c := Candidate{
				Market:  healthyMarket(symbol, base),
				Candles: makeCandles(testAsOf, 400, constVolume(10_000_000), nil),
			}
			res := Filter(testAsOf, []Candidate{c}, FilterParams{})
			if len(res.Included) != 0 {
				t.Fatalf("expected %s to be excluded, got included: %+v", symbol, res.Included)
			}
			if len(res.Excluded) != 1 || res.Excluded[0].Reason != ReasonLeveragedToken {
				t.Fatalf("expected ReasonLeveragedToken, got %+v", res.Excluded)
			}
		})
	}
}

// TestIsLeveraged_NoFalsePositiveOnSubstringMatch guards against the
// naive-but-wrong "symbol contains UP" check: SUPER/USDT contains "UP" as a
// substring (S-UP-ER) but is not a leveraged token. The default patterns
// require the match to be immediately followed by "/" (i.e. the leverage
// suffix sits right before the quote separator), which SUPER/USDT does not
// satisfy.
func TestIsLeveraged_NoFalsePositiveOnSubstringMatch(t *testing.T) {
	if isLeveraged("SUPER/USDT", DefaultExcludePatterns) {
		t.Fatal("SUPER/USDT must not be treated as a leveraged token")
	}
	if !isLeveraged("BTCUP/USDT", DefaultExcludePatterns) {
		t.Fatal("BTCUP/USDT must be treated as a leveraged token")
	}
}

func TestFilter_ExcludesStablecoin(t *testing.T) {
	c := Candidate{
		Market:  healthyMarket("USDC/USDT", "USDC"),
		Candles: makeCandles(testAsOf, 400, constVolume(10_000_000), nil),
	}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{ExcludeStablecoins: true})
	if len(res.Included) != 0 {
		t.Fatalf("expected USDC/USDT to be excluded, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonStablecoin {
		t.Fatalf("expected ReasonStablecoin, got %+v", res.Excluded[0])
	}
}

func TestFilter_StablecoinExclusionRespectsFlag(t *testing.T) {
	c := Candidate{
		Market:  healthyMarket("USDC/USDT", "USDC"),
		Candles: makeCandles(testAsOf, 400, constVolume(10_000_000), nil),
	}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{ExcludeStablecoins: false})
	if len(res.Included) != 1 {
		t.Fatalf("expected USDC/USDT to be included when ExcludeStablecoins=false, got %+v", res.Excluded)
	}
}

func TestFilter_ExcludesWrongQuote(t *testing.T) {
	m := healthyMarket("ETH/BTC", "ETH")
	m.Quote = "BTC"
	c := Candidate{Market: m, Candles: makeCandles(testAsOf, 400, constVolume(10_000_000), nil)}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 0 {
		t.Fatalf("expected ETH/BTC to be excluded, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonWrongQuote {
		t.Fatalf("expected ReasonWrongQuote, got %+v", res.Excluded[0])
	}
}

func TestFilter_ExcludesInactiveAtDate(t *testing.T) {
	m := healthyMarket("XYZ/USDT", "XYZ")
	m.DelistedAt = testAsOf.AddDate(0, 0, -10) // delisted before asOf
	c := Candidate{Market: m, Candles: makeCandles(testAsOf.AddDate(0, 0, -10), 400, constVolume(10_000_000), nil)}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 0 {
		t.Fatalf("expected delisted symbol to be excluded, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonInactiveAtDate {
		t.Fatalf("expected ReasonInactiveAtDate, got %+v", res.Excluded[0])
	}
}

func TestFilter_ExcludesTooYoung(t *testing.T) {
	m := healthyMarket("NEW/USDT", "NEW")
	m.ListedAt = testAsOf.AddDate(0, 0, -60) // 60 days old, below 180-day default minimum
	c := Candidate{Market: m, Candles: makeCandles(testAsOf, 200, constVolume(10_000_000), nil)}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 0 {
		t.Fatalf("expected NEW/USDT to be excluded, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonTooYoung {
		t.Fatalf("expected ReasonTooYoung, got %+v", res.Excluded[0])
	}
}

func TestFilter_UnknownListingDateFallsBackToFirstCandle(t *testing.T) {
	m := healthyMarket("OLD/USDT", "OLD")
	m.ListedAt = time.Time{} // unknown, per SPEC.md Bölüm 4.1's "bilinmiyorsa NULL"

	t.Run("old enough via first candle", func(t *testing.T) {
		c := Candidate{Market: m, Candles: makeCandles(testAsOf, 200, constVolume(10_000_000), nil)} // first candle 199 days before asOf
		res := Filter(testAsOf, []Candidate{c}, FilterParams{})
		if len(res.Included) != 1 {
			t.Fatalf("expected inclusion via first-candle fallback, got excluded: %+v", res.Excluded)
		}
	})

	t.Run("too young via first candle", func(t *testing.T) {
		c := Candidate{Market: m, Candles: makeCandles(testAsOf, 100, constVolume(10_000_000), nil)} // first candle only 99 days before asOf
		res := Filter(testAsOf, []Candidate{c}, FilterParams{})
		if len(res.Included) != 0 {
			t.Fatalf("expected exclusion via first-candle fallback, got included: %+v", res.Included)
		}
		if res.Excluded[0].Reason != ReasonTooYoung {
			t.Fatalf("expected ReasonTooYoung, got %+v", res.Excluded[0])
		}
	})
}

func TestFilter_ExcludesInsufficientWarmup(t *testing.T) {
	m := healthyMarket("THIN/USDT", "THIN") // listed 400 days ago, so age check passes
	c := Candidate{Market: m, Candles: makeCandles(testAsOf, 50, constVolume(10_000_000), nil)}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 0 {
		t.Fatalf("expected THIN/USDT to be excluded, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonInsufficientData {
		t.Fatalf("expected ReasonInsufficientData, got %+v", res.Excluded[0])
	}
}

func TestFilter_ExcludesLowLiquidityUsingMedianNotMean(t *testing.T) {
	// 29 quiet days at 1_000_000 USDT/day plus one pump day at 200_000_000
	// within the last 30 days. The MEAN of these 30 values is
	// (29*1e6 + 2e8)/30 ≈ 7.63e6, which is ABOVE the 5_000_000 default
	// threshold — a mean-based filter would wrongly include this symbol.
	// The MEDIAN stays at 1_000_000, correctly below the threshold.
	c := Candidate{
		Market: healthyMarket("PUMP/USDT", "PUMP"),
		Candles: makeCandles(testAsOf, 200, func(i int) float64 {
			// last 30 candles are indices 170..199
			if i == 185 { // one pump day inside the last-30-day window
				return 200_000_000
			}
			if i >= 170 {
				return 1_000_000
			}
			return 10_000_000 // outside the lookback window, irrelevant
		}, nil),
	}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 0 {
		t.Fatalf("expected PUMP/USDT to be excluded by the median liquidity check, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonLowLiquidity {
		t.Fatalf("expected ReasonLowLiquidity, got %+v", res.Excluded[0])
	}
}

func TestFilter_ExcludesRecentQualityFlag(t *testing.T) {
	c := Candidate{
		Market: healthyMarket("FLAKY/USDT", "FLAKY"),
		Candles: makeCandles(testAsOf, 200, constVolume(10_000_000), func(i int) float64 {
			if i == 185 { // inside the last-30-day window
				return 0 // triggers datafeed.IssueZeroVolume
			}
			return 1
		}),
	}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 0 {
		t.Fatalf("expected FLAKY/USDT to be excluded for a recent quality flag, got included: %+v", res.Included)
	}
	if res.Excluded[0].Reason != ReasonRecentQualityFlag {
		t.Fatalf("expected ReasonRecentQualityFlag, got %+v", res.Excluded[0])
	}
}

func TestFilter_QualityFlagOutsideLookbackWindowIsIgnored(t *testing.T) {
	c := Candidate{
		Market: healthyMarket("OLDFLAG/USDT", "OLDFLAG"),
		Candles: makeCandles(testAsOf, 200, constVolume(10_000_000), func(i int) float64 {
			if i == 100 { // well outside the last 30 days
				return 0
			}
			return 1
		}),
	}
	res := Filter(testAsOf, []Candidate{c}, FilterParams{})
	if len(res.Included) != 1 {
		t.Fatalf("expected OLDFLAG/USDT to be included (stale flag outside lookback), got %+v", res.Excluded)
	}
}

func TestCapByMaxSymbols(t *testing.T) {
	scored := []ScoredSymbol{
		{Symbol: "A", Rank: 1, Score: 2.0},
		{Symbol: "B", Rank: 2, Score: 1.0},
		{Symbol: "C", Rank: 3, Score: 0.5},
	}
	var res Result
	got := CapByMaxSymbols(scored, 2, &res)
	if len(got) != 2 || got[0].Symbol != "A" || got[1].Symbol != "B" {
		t.Fatalf("unexpected capped universe: %+v", got)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Symbol != "C" || res.Excluded[0].Reason != ReasonCappedByMaxSymbols {
		t.Fatalf("expected C excluded with ReasonCappedByMaxSymbols, got %+v", res.Excluded)
	}
}

func TestCapByMaxSymbols_DisabledWhenZeroOrBelow(t *testing.T) {
	scored := []ScoredSymbol{{Symbol: "A"}, {Symbol: "B"}}
	var res Result
	got := CapByMaxSymbols(scored, 0, &res)
	if len(got) != 2 || len(res.Excluded) != 0 {
		t.Fatalf("expected cap disabled at maxSymbols<=0, got %+v excluded=%+v", got, res.Excluded)
	}
}
