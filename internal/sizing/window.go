package sizing

import (
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// maxMilliValue (the largest Quantity.Value() that MilliValue() can convert
// without wrapping) is defined in decide.go, alongside the empirical
// measurements behind it.

// WindowVersion is the schema version of the persisted observation window.
// A window carrying any other value is discarded and rebuilt from scratch.
const WindowVersion = 1

const (
	dateLayout = "2006-01-02"
	timeLayout = "15:04"

	// dayCoverageMaxGap is the longest gap between two samples that still
	// counts as continuous observation for a day.
	dayCoverageMaxGap = time.Hour
	// coverageFirstBy and coverageLastBy bracket the day: sampling must have
	// started before 00:30 and still been running after 23:30.
	coverageFirstBy = "00:30"
	coverageLastBy  = "23:30"
)

// DayBucket holds the per-resource maximum of status.used observed on one day,
// plus the metadata needed to judge whether that day was observed continuously.
type DayBucket struct {
	Date   string            `json:"d"`
	N      int               `json:"n"`
	First  string            `json:"first"`
	Last   string            `json:"last"`
	MaxGap string            `json:"maxGap"`
	Peaks  map[string]string `json:"p"`
}

// Window is the rolling observation window persisted on the state Lease.
type Window struct {
	Version      int         `json:"v"`
	UID          string      `json:"uid"`
	LastSampleAt string      `json:"ls"`
	LastWriteAt  string      `json:"lw"`
	Days         []DayBucket `json:"days"`
}

// DecodeWindow parses a persisted window. Anything unparseable or written by a
// different schema version yields an empty window, which keeps the shrink path
// blocked until a full window has been rebuilt.
func DecodeWindow(raw string) Window {
	if raw == "" {
		return Window{Version: WindowVersion}
	}
	var w Window
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return Window{Version: WindowVersion}
	}
	if w.Version != WindowVersion {
		return Window{Version: WindowVersion}
	}
	if hasDuplicateDates(w.Days) {
		// IsComplete indexes days keeping the last bucket per date; Observe's
		// indexOf keeps updating the first. A window that somehow ended up
		// with two buckets for the same date would let the two disagree
		// about which bucket is authoritative — in the direction that can
		// only hide an uncovered day, never invent one, since the two
		// candidate buckets were both written by the same trusted path.
		// Treat it the same as any other corrupt window: discard and
		// rebuild, rather than risk reading the wrong one.
		return Window{Version: WindowVersion}
	}
	return w
}

// hasDuplicateDates reports whether any date appears more than once, which
// should never happen on a window this package wrote itself.
func hasDuplicateDates(days []DayBucket) bool {
	seen := make(map[string]bool, len(days))
	for _, bucket := range days {
		if seen[bucket.Date] {
			return true
		}
		seen[bucket.Date] = true
	}
	return false
}

// EncodeWindow serialises a window for storage in a Lease annotation.
func EncodeWindow(w Window) (string, error) {
	w.Version = WindowVersion
	raw, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Observe folds one sample of status.used into the window. It returns true when
// the window changed in a way worth persisting: a new day, a pruned bucket, or
// a peak that rose.
func (w *Window) Observe(
	now time.Time,
	uid string,
	usedList corev1.ResourceList,
	windowDays int,
) bool {
	if w.UID != "" && w.UID != uid {
		// The quota was deleted and recreated under the same name. The old
		// history describes a different object and must not drive a shrink.
		*w = Window{Version: WindowVersion}
	}
	w.UID = uid
	w.Version = WindowVersion

	changed := w.prune(now, windowDays)

	stamp := now.UTC()
	today := stamp.Format(dateLayout)
	idx := w.indexOf(today)
	if idx < 0 {
		w.Days = append(w.Days, DayBucket{
			Date:  today,
			First: stamp.Format(timeLayout),
			Peaks: map[string]string{},
		})
		idx = len(w.Days) - 1
		changed = true
	}
	bucket := &w.Days[idx]
	if bucket.Peaks == nil {
		bucket.Peaks = map[string]string{}
	}

	// The gap is measured against the previous sample regardless of which day
	// it fell on, so an outage spanning midnight invalidates the new day too.
	if last, err := time.Parse(time.RFC3339, w.LastSampleAt); err == nil {
		gap := stamp.Sub(last)
		// A stored value that cannot be parsed is left untouched: covered()
		// rejects the day, and overwriting it here would quietly repair a
		// bucket whose real observation history is unknown.
		if previous, ok := parseGap(bucket.MaxGap); ok && gap > previous {
			bucket.MaxGap = gap.Truncate(time.Second).String()
		}
	}
	w.LastSampleAt = stamp.Format(time.RFC3339)
	bucket.Last = stamp.Format(timeLayout)
	bucket.N++

	for res, qty := range usedList {
		key := string(res)
		previous, ok := bucket.Peaks[key]
		if !ok {
			bucket.Peaks[key] = qty.String()
			changed = true
			continue
		}
		parsed, err := resource.ParseQuantity(previous)
		if err != nil || qty.Cmp(parsed) > 0 {
			bucket.Peaks[key] = qty.String()
			changed = true
		}
	}

	return changed
}

// Peak returns the highest value observed for a resource across the completed
// days of the window, in milli-units.
func (w Window) Peak(res corev1.ResourceName, now time.Time, windowDays int) (int64, bool) {
	var (
		best  int64
		found bool
	)
	today := now.UTC().Format(dateLayout)
	oldest := now.UTC().AddDate(0, 0, -windowDays).Format(dateLayout)

	for _, bucket := range w.Days {
		if bucket.Date >= today || bucket.Date < oldest {
			continue
		}
		raw, ok := bucket.Peaks[string(res)]
		if !ok {
			continue
		}
		qty, err := resource.ParseQuantity(raw)
		if err != nil {
			continue
		}
		if qty.Value() > maxMilliValue {
			// MilliValue() would wrap above this size; a wrapped historical
			// peak could read as huge-negative and mask real usage, or as
			// small-positive and understate it — either way it must not
			// enter the max() below.
			continue
		}
		if milli := qty.MilliValue(); !found || milli > best {
			best = milli
			found = true
		}
	}
	return best, found
}

// IsComplete reports whether every one of the windowDays completed days before
// today was observed continuously and carries a value for res.
func (w Window) IsComplete(res corev1.ResourceName, now time.Time, windowDays int) bool {
	byDate := make(map[string]DayBucket, len(w.Days))
	for _, bucket := range w.Days {
		byDate[bucket.Date] = bucket
	}

	for i := 1; i <= windowDays; i++ {
		date := now.UTC().AddDate(0, 0, -i).Format(dateLayout)
		bucket, ok := byDate[date]
		if !ok || !bucket.covered() {
			return false
		}
		raw, ok := bucket.Peaks[string(res)]
		if !ok {
			return false
		}
		// Peak silently skips a value it cannot parse. Accepting the day here
		// would let the window claim to be complete while the peak was
		// computed from fewer days than it reports — a lower peak on
		// supposedly full history, which is what makes a quota shrink too far.
		if _, err := resource.ParseQuantity(raw); err != nil {
			return false
		}
	}
	return true
}

// covered reports whether a day was observed from before 00:30 until after
// 23:30 without a gap longer than dayCoverageMaxGap. The First/Last comparison
// is a lexicographic one on zero-padded "HH:MM" strings, which orders correctly.
func (b DayBucket) covered() bool {
	if b.First == "" || b.Last == "" {
		return false
	}
	gap, ok := parseGap(b.MaxGap)
	if !ok || gap > dayCoverageMaxGap {
		return false
	}
	return b.First <= coverageFirstBy && b.Last >= coverageLastBy
}

// prune drops buckets dated in the future (the clock went backwards) and any
// bucket older than the window. It reports whether anything was removed.
func (w *Window) prune(now time.Time, windowDays int) bool {
	today := now.UTC().Format(dateLayout)
	oldest := now.UTC().AddDate(0, 0, -windowDays).Format(dateLayout)

	kept := make([]DayBucket, 0, len(w.Days))
	for _, bucket := range w.Days {
		if bucket.Date > today || bucket.Date < oldest {
			continue
		}
		kept = append(kept, bucket)
	}
	if len(kept) == len(w.Days) {
		return false
	}
	w.Days = kept
	return true
}

func (w Window) indexOf(date string) int {
	for i, bucket := range w.Days {
		if bucket.Date == date {
			return i
		}
	}
	return -1
}

// parseGap reads a stored gap. It reports ok=false when the value cannot be
// parsed, so callers reject the day instead of reading a corrupt value as "no
// gap at all". That is the dangerous direction: it would make a barely
// observed day look perfectly covered.
func parseGap(raw string) (time.Duration, bool) {
	if raw == "" {
		return 0, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return d, true
}
