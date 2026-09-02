package backtest

import (
	"context"
	"errors"
	"testing"
)

// fakeLockedSegmentStore is an in-memory LockedSegmentStore double, so
// this package's tests never need internal/store (which would violate
// SPEC.md Bölüm 3's layering: internal/backtest does not depend on
// internal/store).
type fakeLockedSegmentStore struct {
	rec *LockedSegmentRecord
}

func (f *fakeLockedSegmentStore) Load(ctx context.Context) (LockedSegmentRecord, bool, error) {
	if f.rec == nil {
		return LockedSegmentRecord{}, false, nil
	}
	return *f.rec, true, nil
}

func (f *fakeLockedSegmentStore) Save(ctx context.Context, rec LockedSegmentRecord) error {
	f.rec = &rec
	return nil
}

func TestViewLockedSegment_FirstViewRecordsAndSucceeds(t *testing.T) {
	st := &fakeLockedSegmentStore{}
	th := DefaultThresholds()

	rec, alreadyViewed, err := ViewLockedSegment(context.Background(), st, LockedSegmentRecord{Thresholds: th, GitSHA: "abc123"}, false)
	if err != nil {
		t.Fatalf("ViewLockedSegment (first view): %v", err)
	}
	if alreadyViewed {
		t.Error("first view should report alreadyViewed = false")
	}
	if rec.ViewedAt.IsZero() {
		t.Error("first view should stamp a non-zero ViewedAt")
	}
	if rec.Thresholds != th {
		t.Errorf("recorded Thresholds = %+v, want %+v", rec.Thresholds, th)
	}
	if st.rec == nil {
		t.Fatal("first view should have persisted a record")
	}
}

func TestViewLockedSegment_SecondViewRefusedWithoutForce(t *testing.T) {
	st := &fakeLockedSegmentStore{}
	th := DefaultThresholds()

	first, _, err := ViewLockedSegment(context.Background(), st, LockedSegmentRecord{Thresholds: th}, false)
	if err != nil {
		t.Fatalf("first view: %v", err)
	}

	second, alreadyViewed, err := ViewLockedSegment(context.Background(), st, LockedSegmentRecord{Thresholds: th}, false)
	if !errors.Is(err, ErrLockedSegmentAlreadyViewed) {
		t.Fatalf("second view without force: err = %v, want ErrLockedSegmentAlreadyViewed", err)
	}
	if !alreadyViewed {
		t.Error("second view should report alreadyViewed = true")
	}
	if !second.ViewedAt.Equal(first.ViewedAt) {
		t.Errorf("second view's returned record should be the ORIGINAL one (ViewedAt %v), got %v", first.ViewedAt, second.ViewedAt)
	}
}

func TestViewLockedSegment_ForceAcknowledgesButWarns(t *testing.T) {
	st := &fakeLockedSegmentStore{}
	th := DefaultThresholds()

	first, _, err := ViewLockedSegment(context.Background(), st, LockedSegmentRecord{Thresholds: th}, false)
	if err != nil {
		t.Fatalf("first view: %v", err)
	}

	second, alreadyViewed, err := ViewLockedSegment(context.Background(), st, LockedSegmentRecord{Thresholds: th}, true)
	if err != nil {
		t.Fatalf("second view with force=true should not error, got %v", err)
	}
	if !alreadyViewed {
		t.Error("second view with force=true must still report alreadyViewed = true — the caller is responsible for surfacing a loud warning")
	}
	if !second.ViewedAt.Equal(first.ViewedAt) {
		t.Error("force=true should not overwrite the original ViewedAt")
	}
}

func TestViewLockedSegment_RequiresThresholdsRecordedFirst(t *testing.T) {
	st := &fakeLockedSegmentStore{}
	_, _, err := ViewLockedSegment(context.Background(), st, LockedSegmentRecord{}, false)
	if !errors.Is(err, ErrThresholdsNotRecorded) {
		t.Fatalf("ViewLockedSegment with zero-value Thresholds: err = %v, want ErrThresholdsNotRecorded", err)
	}
	if st.rec != nil {
		t.Error("a rejected view (missing Thresholds) must not persist anything")
	}
}
