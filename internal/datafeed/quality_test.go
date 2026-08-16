package datafeed

import (
	"testing"
	"time"

	"swingbot/internal/domain"
)

func TestValidateCandlesHappyPath(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	candles := dailyCandles(start, 10)
	now := start.AddDate(0, 0, 20) // well past close of the last candle

	clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, candles, now)
	if len(issues) != 0 {
		t.Fatalf("expected zero issues on well-formed data, got %d: %+v", len(issues), issues)
	}
	if len(clean) != len(candles) {
		t.Fatalf("expected all %d candles to survive, got %d", len(candles), len(clean))
	}
}

func TestValidateCandlesRejectsFutureCandle(t *testing.T) {
	start := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	now := start.Add(10 * time.Hour) // only 10h into the day: not yet closed
	candles := dailyCandles(start, 1)

	clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, candles, now)
	if len(clean) != 0 {
		t.Fatalf("expected the unclosed candle to be rejected, got %d clean candles", len(clean))
	}
	if len(issues) != 1 || issues[0].Kind != IssueFutureCandle || issues[0].Severity != SeverityCritical {
		t.Fatalf("expected exactly one critical future_candle issue, got %+v", issues)
	}
}

func TestValidateCandlesRejectsInvalidOHLC(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 5)

	cases := []struct {
		name string
		c    domain.Candle
	}{
		{"high_below_low", domain.Candle{OpenTime: start, Open: 10, High: 5, Low: 8, Close: 6, Volume: 1}},
		{"close_above_high", domain.Candle{OpenTime: start, Open: 10, High: 12, Low: 9, Close: 20, Volume: 1}},
		{"close_below_low", domain.Candle{OpenTime: start, Open: 10, High: 12, Low: 9, Close: 1, Volume: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, []domain.Candle{tc.c}, now)
			if len(clean) != 0 {
				t.Fatalf("expected invalid OHLC candle to be rejected, got %d clean", len(clean))
			}
			if len(issues) != 1 || issues[0].Kind != IssueInvalidOHLC || issues[0].Severity != SeverityCritical {
				t.Fatalf("expected exactly one critical invalid_ohlc issue, got %+v", issues)
			}
		})
	}
}

func TestValidateCandlesFlagsZeroVolumeButKeeps(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 5)
	c := domain.Candle{OpenTime: start, Open: 10, High: 11, Low: 9, Close: 10, Volume: 0}

	clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, []domain.Candle{c}, now)
	if len(clean) != 1 {
		t.Fatalf("expected zero-volume candle to be kept (flagged, not rejected), got %d clean", len(clean))
	}
	if len(issues) != 1 || issues[0].Kind != IssueZeroVolume || issues[0].Severity != SeverityWarn {
		t.Fatalf("expected exactly one warn zero_volume issue, got %+v", issues)
	}
}

func TestValidateCandlesFlagsOutlierJumpButKeeps(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 5)
	candles := []domain.Candle{
		{OpenTime: start, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1},
		{OpenTime: start.AddDate(0, 0, 1), Open: 100, High: 201, Low: 99, Close: 200, Volume: 1}, // +100%
	}

	clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, candles, now)
	if len(clean) != 2 {
		t.Fatalf("expected outlier candle to be kept (flagged, not rejected), got %d clean", len(clean))
	}
	var found bool
	for _, iss := range issues {
		if iss.Kind == IssueOutlierJump {
			found = true
			if iss.Severity != SeverityWarn {
				t.Errorf("expected outlier_jump to be a warn, got %s", iss.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected an outlier_jump issue, got %+v", issues)
	}
}

func TestValidateCandlesDetectsMissingCandleGap(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 10)
	candles := []domain.Candle{
		{OpenTime: start, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1},
		// day 2 missing entirely
		{OpenTime: start.AddDate(0, 0, 2), Open: 100, High: 101, Low: 99, Close: 100, Volume: 1},
	}

	clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, candles, now)
	if len(clean) != 2 {
		t.Fatalf("missing-candle detection should not drop any candle, got %d clean", len(clean))
	}
	var found bool
	for _, iss := range issues {
		if iss.Kind == IssueMissingCandle {
			found = true
			if iss.Severity != SeverityWarn {
				t.Errorf("expected missing_candle to be a warn, got %s", iss.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected a missing_candle issue, got %+v", issues)
	}
}

func TestValidateCandlesSortsBeforeValidating(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 10)
	// Deliberately out of order input.
	candles := []domain.Candle{
		{OpenTime: start.AddDate(0, 0, 1), Open: 101, High: 102, Low: 100, Close: 101, Volume: 1},
		{OpenTime: start, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1},
	}
	clean, issues := ValidateCandles("BTC/USDT", "1d", 24*time.Hour, candles, now)
	if len(issues) != 0 {
		t.Fatalf("expected zero issues once sorted (no real gap/outlier), got %+v", issues)
	}
	if len(clean) != 2 || !clean[0].OpenTime.Equal(start) {
		t.Fatalf("expected clean output sorted ascending by open_time, got %+v", clean)
	}
}

func TestHasCritical(t *testing.T) {
	if HasCritical(nil) {
		t.Error("HasCritical(nil) should be false")
	}
	warnOnly := []QualityIssue{{Severity: SeverityWarn}}
	if HasCritical(warnOnly) {
		t.Error("HasCritical should be false when only warn issues are present")
	}
	withCritical := []QualityIssue{{Severity: SeverityWarn}, {Severity: SeverityCritical}}
	if !HasCritical(withCritical) {
		t.Error("HasCritical should be true when a critical issue is present")
	}
}
