package sizing

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const testUID = "3f2a1c8e-0000-0000-0000-000000000001"

func used(cpu string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse(cpu),
	}
}

// fillWindow samples every 5 minutes across the given number of completed
// days ending yesterday, so the window is fully covered.
//
//nolint:unparam // cpu parameter signature must be preserved for test reuse in later tasks
func fillWindow(now time.Time, days int, cpu string) Window {
	w := Window{Version: WindowVersion}
	start := now.UTC().AddDate(0, 0, -days).Truncate(24 * time.Hour)
	for t := start; t.Before(now.UTC().Truncate(24 * time.Hour)); t = t.Add(5 * time.Minute) {
		w.Observe(t, testUID, used(cpu), days)
	}
	return w
}

func TestWindow_CodecRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	original := fillWindow(now, 2, "4")

	encoded, err := EncodeWindow(original)
	if err != nil {
		t.Fatalf("EncodeWindow: %v", err)
	}
	decoded := DecodeWindow(encoded)

	if len(decoded.Days) != len(original.Days) {
		t.Fatalf("days = %d, want %d", len(decoded.Days), len(original.Days))
	}
	if decoded.UID != testUID {
		t.Errorf("uid = %q, want %q", decoded.UID, testUID)
	}
}

func TestDecodeWindow_Tolerant(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "not json", raw: "{{{"},
		{name: "unknown version", raw: `{"v":99,"days":[{"d":"2026-08-01"}]}`},
		{
			name: "duplicate date",
			// IsComplete keeps the last bucket per date, Observe keeps
			// updating the first — a window with two buckets for the same
			// date must be treated as corrupt, the same as an unparseable
			// one, rather than let the two disagree about which is real.
			raw: `{"v":1,"days":[` +
				`{"d":"2026-08-01","p":{"requests.cpu":"1"}},` +
				`{"d":"2026-08-01","p":{"requests.cpu":"2"}}` +
				`]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := DecodeWindow(tc.raw)
			if len(w.Days) != 0 {
				t.Fatalf("days = %d, want 0 (window must reset)", len(w.Days))
			}
			if w.Version != WindowVersion {
				t.Errorf("version = %d, want %d", w.Version, WindowVersion)
			}
		})
	}
}

func TestWindow_PeakAcrossCompletedDays(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := fillWindow(now, 3, "4")

	// A higher sample on the current day must not enter the completed-day peak.
	w.Observe(now, testUID, used("40"), 3)

	peak, ok := w.Peak(corev1.ResourceRequestsCPU, now, 3)
	if !ok {
		t.Fatal("Peak reported no data, want data")
	}
	if peak != 4000 {
		t.Fatalf("peak = %d milli, want 4000 (current day excluded)", peak)
	}
}

func TestWindow_Peak_SkipsOverflowingBucketButKeepsOthers(t *testing.T) {
	// Quantity.MilliValue() wraps above roughly 8 PiB (see decide.go for the
	// empirical measurements). A historical bucket carrying such a value
	// must not enter the peak as a wrapped, possibly negative, number — but
	// a real value on another day must still count.
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := Window{
		Version: WindowVersion,
		Days: []DayBucket{
			{
				Date:  now.UTC().AddDate(0, 0, -1).Format(dateLayout),
				Peaks: map[string]string{"requests.storage": "16Pi"},
			},
			{
				Date:  now.UTC().AddDate(0, 0, -2).Format(dateLayout),
				Peaks: map[string]string{"requests.storage": "3Pi"},
			},
		},
	}

	peak, ok := w.Peak(corev1.ResourceRequestsStorage, now, 14)

	if !ok {
		t.Fatal("Peak reported no data, want the 3Pi day")
	}
	wantQty := resource.MustParse("3Pi")
	want := wantQty.MilliValue()
	if peak != want {
		t.Fatalf("peak = %d, want %d (3Pi; the 16Pi day must be skipped, not wrapped)", peak, want)
	}
}

func TestWindow_Peak_AllBucketsOverflowingYieldsNoData(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := Window{
		Version: WindowVersion,
		Days: []DayBucket{
			{
				Date:  now.UTC().AddDate(0, 0, -1).Format(dateLayout),
				Peaks: map[string]string{"requests.storage": "16Pi"},
			},
		},
	}

	if peak, ok := w.Peak(corev1.ResourceRequestsStorage, now, 14); ok {
		t.Fatalf("Peak = %d, ok = true; want no usable data from an unmeasurable value", peak)
	}
}

func TestWindow_IsComplete(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	t.Run("fully sampled window is complete", func(t *testing.T) {
		w := fillWindow(now, 14, "4")
		if !w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
			t.Fatal("IsComplete = false, want true")
		}
	})

	t.Run("downtime invalidates the window", func(t *testing.T) {
		w := fillWindow(now, 14, "4")
		// Six-hour outage on the day before yesterday.
		gapDay := now.UTC().AddDate(0, 0, -2).Format("2006-01-02")
		for i := range w.Days {
			if w.Days[i].Date == gapDay {
				w.Days[i].MaxGap = "6h0m0s"
			}
		}
		if w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
			t.Fatal("IsComplete = true, want false after a 6h gap")
		}
	})

	t.Run("short window is incomplete", func(t *testing.T) {
		w := fillWindow(now, 3, "4")
		if w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
			t.Fatal("IsComplete = true, want false with only 3 days")
		}
	})

	t.Run("newly appeared resource is incomplete", func(t *testing.T) {
		w := fillWindow(now, 14, "4")
		res := corev1.ResourceRequestsStorage
		if w.IsComplete(res, now, 14) {
			t.Fatal("IsComplete = true, want false for an unobserved resource")
		}
	})
}

func TestWindow_UIDChangeResets(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := fillWindow(now, 14, "4")

	w.Observe(now, "a-different-uid", used("4"), 14)

	if len(w.Days) != 1 {
		t.Fatalf("days = %d, want 1 after a UID change", len(w.Days))
	}
	if w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
		t.Fatal("IsComplete = true, want false after a reset")
	}
}

func TestWindow_DropsFutureBuckets(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := Window{
		Version: WindowVersion,
		UID:     testUID,
		Days: []DayBucket{
			{Date: "2026-08-20", N: 5, Peaks: map[string]string{"requests.cpu": "99"}},
		},
	}

	w.Observe(now, testUID, used("4"), 14)

	for _, b := range w.Days {
		if b.Date == "2026-08-20" {
			t.Fatal("future-dated bucket survived, want it dropped")
		}
	}
}

func TestWindow_ObserveRecordsOutage(t *testing.T) {
	now := time.Date(2026, 8, 8, 6, 0, 0, 0, time.UTC)
	w := Window{Version: WindowVersion}

	w.Observe(now, testUID, used("4"), 14)
	w.Observe(now.Add(6*time.Hour), testUID, used("4"), 14)

	// The gap has to be recorded by Observe itself. Every other coverage test
	// sets MaxGap by hand, which would hide a regression here.
	if got := w.Days[0].MaxGap; got != "6h0m0s" {
		t.Fatalf("maxGap = %q, want 6h0m0s", got)
	}
	if w.Days[0].covered() {
		t.Fatal("day counts as covered after a six-hour outage")
	}
}

func TestWindow_OutageAcrossMidnightFailsBothDays(t *testing.T) {
	evening := time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC)
	w := Window{Version: WindowVersion}

	w.Observe(evening, testUID, used("4"), 14)
	w.Observe(evening.Add(8*time.Hour), testUID, used("4"), 14)

	if len(w.Days) != 2 {
		t.Fatalf("days = %d, want 2", len(w.Days))
	}
	if w.Days[0].covered() {
		t.Error("the day before the outage counts as covered")
	}
	if got := w.Days[1].MaxGap; got != "8h0m0s" {
		t.Errorf("new day maxGap = %q, want 8h0m0s", got)
	}
	if w.Days[1].covered() {
		t.Error("the day after the outage counts as covered")
	}
}

func TestWindow_PartiallySampledDayIsRejected(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := fillWindow(now, 14, "4")

	// A controller that ran for ten minutes leaves a bucket that exists but
	// was never observed to the end of the day. Counting it would be exactly
	// the false confidence the window is meant to prevent.
	short := now.UTC().AddDate(0, 0, -5).Format(dateLayout)
	for i := range w.Days {
		if w.Days[i].Date == short {
			w.Days[i].Last = "00:10"
		}
	}

	if w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
		t.Fatal("IsComplete = true for a day sampled only until 00:10")
	}
}

func TestWindow_CorruptStoredValuesFailClosed(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	t.Run("unreadable maxGap rejects the day", func(t *testing.T) {
		w := fillWindow(now, 14, "4")
		w.Days[3].MaxGap = "not-a-duration"

		if w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
			t.Fatal("IsComplete = true despite an unreadable maxGap")
		}
	})

	t.Run("unreadable peak rejects the window", func(t *testing.T) {
		w := fillWindow(now, 14, "4")
		w.Days[3].Peaks[string(corev1.ResourceRequestsCPU)] = "not-a-quantity"

		if w.IsComplete(corev1.ResourceRequestsCPU, now, 14) {
			t.Fatal("IsComplete = true despite an unreadable peak value")
		}
	})
}

func TestWindow_ObserveReportsChange(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := Window{Version: WindowVersion}

	if changed := w.Observe(now, testUID, used("4"), 14); !changed {
		t.Fatal("first sample reported no change, want change")
	}
	if changed := w.Observe(now.Add(5*time.Minute), testUID, used("3"), 14); changed {
		t.Fatal("lower sample reported a change, want none")
	}
	if changed := w.Observe(now.Add(10*time.Minute), testUID, used("5"), 14); !changed {
		t.Fatal("higher sample reported no change, want change")
	}
}
