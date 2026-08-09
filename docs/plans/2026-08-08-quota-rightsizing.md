# Bidirectional Quota Rightsizing — Implementation Plan

> Work task-by-task. Every task ends with an independently testable
> deliverable and a commit. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the grow-only quota logic with a single target formula driven
by an observed demand window, so quotas follow real demand in both directions.

**Architecture:** A new `internal/sizing` package holds a pure decision
function (no Kubernetes client, clock injected). The controller becomes an
orchestrator: sample `status.used` into a rolling window on the state Lease,
call `sizing.Decide`, act on the resulting `Decision`. Shrinking is gated
behind a complete window, a long cooldown, a per-PR step cap and a hard floor,
and shrink PRs are never auto-merged.

**Tech Stack:** Go 1.26, controller-runtime 0.24, k8s.io/api 0.36,
go-github v75, Ginkgo/Gomega + envtest, prometheus/client_golang.

**Design document:** `docs/design/2026-08-08-quota-rightsizing.md` — read it
before starting. Section references below (`spec 3.2` etc.) point into it.

## Global Constraints

- Go module path: `github.com/payback159/namespace-resizer`.
- All new annotations use the `resizer.io/` prefix.
- Lint: golangci-lint v2 config in `.golangci.yml`. Active linters include
  `lll` (max 120 columns), `gocyclo`, `dupl`, `prealloc`, `goconst`,
  `unparam`, and `revive` with `comment-spacings` + `import-shadowing`.
  Never shadow an imported package name with a local variable.
- Every source file keeps the existing Apache-2.0 header only if the file it
  replaces had one; new files in `internal/sizing` do not need it (matching
  `internal/lock/lease.go`, which has none).
- Tests are plain `go test` with `gomega.NewWithT(t)` or bare `testing`
  assertions, matching `internal/lock/lease_test.go` and
  `internal/controller/automerge_test.go`. Only `suite_test.go` uses Ginkgo.
- Run `make test` before every commit. It runs `manifests generate fmt vet
  setup-envtest` and then `go test ./...`.
- Run `make lint` before every commit.
- Existing behaviour must not regress: with no new annotations set, grow
  behaviour after Task 8 must match today's behaviour via the migration
  fallbacks in `sizing.ParsePolicy`.
- Never bypass git hooks. Commit messages follow the repo's conventional
  style (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`), English,
  no trailers.

## File Structure

**Created**

| File | Responsibility |
|---|---|
| `internal/sizing/quantity.go` | Milli-value → `resource.Quantity`, three branches (memory/storage, countable, divisible) |
| `internal/sizing/policy.go` | `Policy` struct, defaults, annotation parsing incl. deprecation migration |
| `internal/sizing/window.go` | `Window`/`DayBucket` types, JSON codec, `Observe`, `Peak`, `IsComplete` |
| `internal/sizing/deficit.go` | Pure event-deficit helpers moved out of the controller |
| `internal/sizing/decide.go` | `Decide(Input) Decision` — target formula, tolerance band, shrink gates |
| `internal/lock/state.go` | `State` struct + `GetState`/`MutateState` over the Lease |
| `internal/controller/observation.go` | `Observer` — write-behind sampling of `status.used` into the Lease |
| `internal/controller/metrics.go` | Prometheus gauges/counters from `Decision` |

**Modified**

| File | Change |
|---|---|
| `internal/controller/resourcequota_controller.go` | Orchestration only; threshold path removed |
| `internal/controller/resourcequota_utils.go` | Keeps only client-dependent helpers |
| `internal/controller/fake_git_provider.go` | `ClosePR`, direction-aware `FindOpenPR` |
| `internal/git/github.go` | `ClosePR`, direction label, `FindOpenPR` returns direction |
| `internal/config/constants.go` | New annotation constants, old ones deprecated |
| `cmd/main.go` | `--enable-shrink` flag, `Observer` wiring |
| `docs/ARCHITECTURE.md`, `docs/INSTALLATION.md`, `docs/OPERATIONS.md` | Documentation |

**Deleted**

| File | Reason |
|---|---|
| `internal/controller/limits_test.go` | Covers `getPodRequests`, which moves to `internal/sizing/deficit_test.go` |

---

## Stage 1 — Observation and decision, without shrink PRs

After Task 9 the system is fully functional and already delivers value
(visibility of waste) without a single shrink PR being possible.

---

### Task 1: Quantity conversion with a countable-resource branch

Fixes the latent bug in spec 9.1: `convertToReadableFormat` routes everything
except memory/storage through `NewMilliQuantity`, so a target of 11.25 pods
becomes `"11250m"`, which Kubernetes rejects as a pod quota.

**Files:**
- Create: `internal/sizing/quantity.go`
- Test: `internal/sizing/quantity_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func Quantize(res corev1.ResourceName, milli int64, format resource.Format) resource.Quantity`
  - `func IsCountable(res corev1.ResourceName) bool`

- [ ] **Step 1: Write the failing test**

Create `internal/sizing/quantity_test.go`:

```go
package sizing

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestQuantize(t *testing.T) {
	cases := []struct {
		name   string
		res    corev1.ResourceName
		milli  int64
		format resource.Format
		want   string
	}{
		{
			name:   "cpu keeps milli precision",
			res:    corev1.ResourceRequestsCPU,
			milli:  11250,
			format: resource.DecimalSI,
			want:   "11250m",
		},
		{
			name:   "memory rounds up to whole Mi",
			res:    corev1.ResourceRequestsMemory,
			milli:  100*1024*1024*1000 + 1,
			format: resource.BinarySI,
			want:   "101Mi",
		},
		{
			name:   "pods round up to a whole number",
			res:    corev1.ResourcePods,
			milli:  11250,
			format: resource.DecimalSI,
			want:   "12",
		},
		{
			name:   "count/ keys round up to a whole number",
			res:    corev1.ResourceName("count/deployments.apps"),
			milli:  3200,
			format: resource.DecimalSI,
			want:   "4",
		},
		{
			name:   "storage rounds up to whole Mi",
			res:    corev1.ResourceRequestsStorage,
			milli:  5 * 1024 * 1024 * 1000,
			format: resource.BinarySI,
			want:   "5Mi",
		},
		{
			// A storage-class scoped key counts claims. The scope contains
			// the word "storage", which must not route it into bytes.
			name:   "storage-class scoped claim count stays an integer",
			res:    corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims"),
			milli:  11250,
			format: resource.DecimalSI,
			want:   "12",
		},
		{
			name:   "storage-class scoped storage request stays bytes",
			res:    corev1.ResourceName("gold.storageclass.storage.k8s.io/requests.storage"),
			milli:  5 * 1024 * 1024 * 1000,
			format: resource.BinarySI,
			want:   "5Mi",
		},
		{
			name:   "count/ key whose group contains storage stays an integer",
			res:    corev1.ResourceName("count/csistoragecapacities.storage.k8s.io"),
			milli:  3200,
			format: resource.DecimalSI,
			want:   "4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Quantize(tc.res, tc.milli, tc.format)
			if got.String() != tc.want {
				t.Fatalf("Quantize(%s, %d) = %s, want %s",
					tc.res, tc.milli, got.String(), tc.want)
			}
		})
	}
}

func TestIsCountable(t *testing.T) {
	countable := []corev1.ResourceName{
		corev1.ResourcePods,
		corev1.ResourceSecrets,
		corev1.ResourcePersistentVolumeClaims,
		corev1.ResourceName("count/jobs.batch"),
		corev1.ResourceName("count/csistoragecapacities.storage.k8s.io"),
		corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims"),
	}
	for _, res := range countable {
		if !IsCountable(res) {
			t.Errorf("IsCountable(%s) = false, want true", res)
		}
	}

	divisible := []corev1.ResourceName{
		corev1.ResourceRequestsCPU,
		corev1.ResourceLimitsMemory,
		corev1.ResourceRequestsStorage,
		corev1.ResourceName("gold.storageclass.storage.k8s.io/requests.storage"),
	}
	for _, res := range divisible {
		if IsCountable(res) {
			t.Errorf("IsCountable(%s) = true, want false", res)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sizing/... -run 'TestQuantize|TestIsCountable' -v`
Expected: build failure — `undefined: Quantize`, `undefined: IsCountable`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/sizing/quantity.go`:

```go
package sizing

import (
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	bytesPerMi  = 1024 * 1024
	countPrefix = "count/"
)

// measureOf strips the scope from a quota key so classification looks only at
// what is being measured. ResourceQuota keys can be scoped by storage class,
// and the scope contains the word "storage" whatever it measures:
// "gold.storageclass.storage.k8s.io/persistentvolumeclaims" counts claims
// while ".../requests.storage" measures bytes. Keys carrying the "count/"
// prefix are returned whole — there the prefix is the classification.
func measureOf(res corev1.ResourceName) string {
	name := string(res)
	if strings.HasPrefix(name, countPrefix) {
		return name
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// IsCountable reports whether a quota key counts objects rather than
// measuring a divisible amount. Countable keys only accept whole numbers,
// so a fractional target must be rounded up before it is written back.
func IsCountable(res corev1.ResourceName) bool {
	if strings.HasPrefix(string(res), countPrefix) {
		return true
	}
	switch corev1.ResourceName(measureOf(res)) {
	case corev1.ResourcePods,
		corev1.ResourceServices,
		corev1.ResourceReplicationControllers,
		corev1.ResourceQuotas,
		corev1.ResourceSecrets,
		corev1.ResourceConfigMaps,
		corev1.ResourcePersistentVolumeClaims,
		corev1.ResourceServicesNodePorts,
		corev1.ResourceServicesLoadBalancers:
		return true
	}
	return false
}

// Quantize converts a computed milli-value back into a Quantity that
// Kubernetes accepts for the given quota key. Rounding is always upwards so
// the result never falls below the computed target.
//
// Countable keys are tested first. A substring test for "storage" would
// otherwise claim scoped keys such as
// "gold.storageclass.storage.k8s.io/persistentvolumeclaims", which counts
// claims and has to stay an integer.
func Quantize(res corev1.ResourceName, milli int64, format resource.Format) resource.Quantity {
	if IsCountable(res) {
		whole := int64(math.Ceil(float64(milli) / 1000.0))
		return *resource.NewQuantity(whole, resource.DecimalSI)
	}

	measure := measureOf(res)
	if strings.Contains(measure, "memory") || strings.Contains(measure, "storage") {
		// Milli-bytes back to bytes, then up to the next whole Mi so the
		// rendered value stays readable ("101Mi" instead of raw bytes).
		bytes := float64(milli) / 1000.0
		mi := math.Ceil(bytes / float64(bytesPerMi))
		return *resource.NewQuantity(int64(mi)*bytesPerMi, resource.BinarySI)
	}

	return *resource.NewMilliQuantity(milli, format)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sizing/... -v`
Expected: PASS for both tests.

- [ ] **Step 5: Lint**

Run: `make lint`
Expected: no findings in `internal/sizing`.

- [ ] **Step 6: Commit**

```bash
git add internal/sizing/quantity.go internal/sizing/quantity_test.go
git commit -m "feat(sizing): quantity conversion with countable-resource branch

Object-count quota keys such as pods or count/deployments.apps only accept
whole numbers. Routing them through NewMilliQuantity produced values like
\"11250m\", which the API server rejects. Quantize adds a dedicated branch
that rounds those keys up to the next integer."
```

---

### Task 2: Policy parsing with deprecation migration

Implements spec 7.1 and 7.2. The migration chain is what keeps existing
installations on their current grow behaviour once the threshold path is
removed in Task 8.

**Files:**
- Create: `internal/sizing/policy.go`
- Test: `internal/sizing/policy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Policy struct { Headroom map[corev1.ResourceName]float64; Min
    map[corev1.ResourceName]resource.Quantity; Tolerance float64; WindowDays
    int; ShrinkCooldown, ShrinkPRTTL, GrowCooldown time.Duration;
    MaxShrinkStep float64; Enabled, ShrinkEnabled bool }`
  - `func DefaultPolicy() Policy`
  - `func ParsePolicy(annotations map[string]string, base Policy) (Policy, []string)`
  - `func (p Policy) HeadroomFor(res corev1.ResourceName) float64`
  - `func (p Policy) MinFor(res corev1.ResourceName) (resource.Quantity, bool)`
  - `const DefaultKey corev1.ResourceName = "default"`

- [ ] **Step 1: Write the failing test**

Create `internal/sizing/policy_test.go`:

```go
package sizing

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestParsePolicy_Defaults(t *testing.T) {
	p, warnings := ParsePolicy(nil, DefaultPolicy())

	if got := p.HeadroomFor(corev1.ResourceRequestsCPU); got != 0.25 {
		t.Errorf("headroom = %v, want 0.25", got)
	}
	if p.Tolerance != 0.15 {
		t.Errorf("tolerance = %v, want 0.15", p.Tolerance)
	}
	if p.WindowDays != 14 {
		t.Errorf("windowDays = %d, want 14", p.WindowDays)
	}
	if p.ShrinkCooldown != 7*24*time.Hour {
		t.Errorf("shrinkCooldown = %v, want 168h", p.ShrinkCooldown)
	}
	if p.MaxShrinkStep != 0.25 {
		t.Errorf("maxShrinkStep = %v, want 0.25", p.MaxShrinkStep)
	}
	if p.GrowCooldown != 60*time.Minute {
		t.Errorf("growCooldown = %v, want 60m", p.GrowCooldown)
	}
	if !p.Enabled || !p.ShrinkEnabled {
		t.Errorf("enabled = %v, shrinkEnabled = %v, want true/true",
			p.Enabled, p.ShrinkEnabled)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestParsePolicy_MigrationChain(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		res         corev1.ResourceName
		want        float64
		wantWarning bool
	}{
		{
			name:        "headroom wins over increment and threshold",
			annotations: map[string]string{
				"resizer.io/cpu-headroom":   "0.4",
				"resizer.io/cpu-increment":  "0.2",
				"resizer.io/cpu-threshold":  "80",
			},
			res:  corev1.ResourceRequestsCPU,
			want: 0.4,
		},
		{
			name:        "increment is taken verbatim",
			annotations: map[string]string{"resizer.io/cpu-increment": "0.2"},
			res:         corev1.ResourceRequestsCPU,
			want:        0.2,
			wantWarning: true,
		},
		{
			name:        "percent suffix on increment",
			annotations: map[string]string{"resizer.io/cpu-increment": "20%"},
			res:         corev1.ResourceRequestsCPU,
			want:        0.2,
			wantWarning: true,
		},
		{
			name:        "threshold 80 derives headroom 0.25",
			annotations: map[string]string{"resizer.io/memory-threshold": "80"},
			res:         corev1.ResourceRequestsMemory,
			want:        0.25,
			wantWarning: true,
		},
		{
			name:        "bare headroom applies as default",
			annotations: map[string]string{"resizer.io/headroom": "0.5"},
			res:         corev1.ResourceName("hugepages-2Mi"),
			want:        0.5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, warnings := ParsePolicy(tc.annotations, DefaultPolicy())
			got := p.HeadroomFor(tc.res)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("HeadroomFor(%s) = %v, want %v", tc.res, got, tc.want)
			}
			if tc.wantWarning && len(warnings) == 0 {
				t.Errorf("expected a deprecation warning, got none")
			}
			if !tc.wantWarning && len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
		})
	}
}

func TestParsePolicy_ScalarsAndMin(t *testing.T) {
	p, _ := ParsePolicy(map[string]string{
		"resizer.io/tolerance":            "0.1",
		"resizer.io/window-days":          "30",
		"resizer.io/shrink-cooldown-days": "14",
		"resizer.io/max-shrink-step":      "15%",
		"resizer.io/shrink-pr-ttl-days":   "3",
		"resizer.io/cooldown-minutes":     "120",
		"resizer.io/enabled":              "false",
		"resizer.io/shrink-enabled":       "false",
		"resizer.io/requests.cpu-min":     "2",
	}, DefaultPolicy())

	if p.Tolerance != 0.1 {
		t.Errorf("tolerance = %v, want 0.1", p.Tolerance)
	}
	if p.WindowDays != 30 {
		t.Errorf("windowDays = %d, want 30", p.WindowDays)
	}
	if p.ShrinkCooldown != 14*24*time.Hour {
		t.Errorf("shrinkCooldown = %v, want 336h", p.ShrinkCooldown)
	}
	if p.MaxShrinkStep != 0.15 {
		t.Errorf("maxShrinkStep = %v, want 0.15", p.MaxShrinkStep)
	}
	if p.ShrinkPRTTL != 3*24*time.Hour {
		t.Errorf("shrinkPRTTL = %v, want 72h", p.ShrinkPRTTL)
	}
	if p.GrowCooldown != 120*time.Minute {
		t.Errorf("growCooldown = %v, want 120m", p.GrowCooldown)
	}
	if p.Enabled || p.ShrinkEnabled {
		t.Errorf("enabled = %v, shrinkEnabled = %v, want false/false",
			p.Enabled, p.ShrinkEnabled)
	}
	min, ok := p.MinFor(corev1.ResourceRequestsCPU)
	if !ok || min.String() != "2" {
		t.Errorf("MinFor(requests.cpu) = %v/%v, want 2/true", min.String(), ok)
	}
	if _, ok := p.MinFor(corev1.ResourceRequestsMemory); ok {
		t.Errorf("MinFor(requests.memory) reported a value, want none")
	}
}

func TestHeadroomFor_FamilyFallback(t *testing.T) {
	p, _ := ParsePolicy(map[string]string{
		"resizer.io/cpu-headroom": "0.6",
	}, DefaultPolicy())

	if got := p.HeadroomFor(corev1.ResourceLimitsCPU); got != 0.6 {
		t.Errorf("limits.cpu headroom = %v, want 0.6 via cpu family", got)
	}
	if got := p.HeadroomFor(corev1.ResourceRequestsMemory); got != 0.25 {
		t.Errorf("requests.memory headroom = %v, want 0.25 default", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sizing/... -run TestParsePolicy -v`
Expected: build failure — `undefined: ParsePolicy`, `undefined: DefaultPolicy`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/sizing/policy.go`:

```go
package sizing

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// AnnotationPrefix is the namespace annotation prefix all resizer settings use.
const AnnotationPrefix = "resizer.io/"

// DefaultKey is the map key holding the namespace-wide fallback value.
const DefaultKey corev1.ResourceName = "default"

// Policy is the effective configuration for one quota, after merging global
// defaults with namespace annotations.
type Policy struct {
	Headroom map[corev1.ResourceName]float64
	Min      map[corev1.ResourceName]resource.Quantity

	Tolerance      float64
	WindowDays     int
	MaxShrinkStep  float64
	ShrinkCooldown time.Duration
	ShrinkPRTTL    time.Duration
	GrowCooldown   time.Duration

	Enabled       bool
	ShrinkEnabled bool
}

// DefaultPolicy returns the built-in defaults from spec 7.2.
func DefaultPolicy() Policy {
	return Policy{
		Headroom:       map[corev1.ResourceName]float64{DefaultKey: 0.25},
		Min:            map[corev1.ResourceName]resource.Quantity{},
		Tolerance:      0.15,
		WindowDays:     14,
		MaxShrinkStep:  0.25,
		ShrinkCooldown: 7 * 24 * time.Hour,
		ShrinkPRTTL:    7 * 24 * time.Hour,
		GrowCooldown:   60 * time.Minute,
		Enabled:        true,
		ShrinkEnabled:  true,
	}
}

// HeadroomFor resolves the headroom for a quota key: exact match first, then
// the resource family (cpu/memory/storage), then the namespace default.
func (p Policy) HeadroomFor(res corev1.ResourceName) float64 {
	if v, ok := p.Headroom[res]; ok {
		return v
	}
	if family, ok := resourceFamily(res); ok {
		if v, ok := p.Headroom[family]; ok {
			return v
		}
	}
	if v, ok := p.Headroom[DefaultKey]; ok {
		return v
	}
	return 0.25
}

// MinFor returns the configured absolute lower bound for a quota key.
func (p Policy) MinFor(res corev1.ResourceName) (resource.Quantity, bool) {
	q, ok := p.Min[res]
	return q, ok
}

func resourceFamily(res corev1.ResourceName) (corev1.ResourceName, bool) {
	name := string(res)
	switch {
	case strings.Contains(name, "cpu"):
		return corev1.ResourceCPU, true
	case strings.Contains(name, "memory"):
		return corev1.ResourceMemory, true
	case strings.Contains(name, "storage"):
		return corev1.ResourceStorage, true
	}
	return "", false
}

// ParsePolicy merges namespace annotations onto base. It returns the effective
// policy plus one warning per deprecated annotation that was honoured.
func ParsePolicy(annotations map[string]string, base Policy) (Policy, []string) {
	out := base
	out.Headroom = copyFloatMap(base.Headroom)
	out.Min = copyQuantityMap(base.Min)

	// Collected separately so precedence is applied deterministically,
	// independent of Go's random map iteration order.
	fromHeadroom := map[corev1.ResourceName]float64{}
	fromIncrement := map[corev1.ResourceName]float64{}
	fromThreshold := map[corev1.ResourceName]float64{}
	var warnings []string

	for key, value := range annotations {
		if !strings.HasPrefix(key, AnnotationPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, AnnotationPrefix)

		switch {
		case strings.HasSuffix(name, "headroom"):
			if v, ok := parseFraction(value); ok {
				fromHeadroom[suffixKey(name, "headroom")] = v
			}
		case strings.HasSuffix(name, "increment"):
			if v, ok := parseFraction(value); ok {
				fromIncrement[suffixKey(name, "increment")] = v
			}
		case strings.HasSuffix(name, "threshold"):
			if v, err := strconv.ParseFloat(value, 64); err == nil && v > 0 {
				fromThreshold[suffixKey(name, "threshold")] = 100.0/v - 1.0
			}
		case strings.HasSuffix(name, "-min"):
			if q, err := resource.ParseQuantity(value); err == nil {
				out.Min[corev1.ResourceName(strings.TrimSuffix(name, "-min"))] = q
			}
		case name == "tolerance":
			if v, ok := parseFraction(value); ok {
				out.Tolerance = v
			}
		case name == "max-shrink-step":
			if v, ok := parseFraction(value); ok {
				out.MaxShrinkStep = v
			}
		case name == "window-days":
			if v, err := strconv.Atoi(value); err == nil && v > 0 {
				out.WindowDays = v
			}
		case name == "shrink-cooldown-days":
			if v, err := strconv.Atoi(value); err == nil && v >= 0 {
				out.ShrinkCooldown = time.Duration(v) * 24 * time.Hour
			}
		case name == "shrink-pr-ttl-days":
			if v, err := strconv.Atoi(value); err == nil && v > 0 {
				out.ShrinkPRTTL = time.Duration(v) * 24 * time.Hour
			}
		case name == "cooldown-minutes":
			if v, err := strconv.Atoi(value); err == nil && v >= 0 {
				out.GrowCooldown = time.Duration(v) * time.Minute
			}
		case name == "enabled":
			out.Enabled = value != "false"
		case name == "shrink-enabled":
			out.ShrinkEnabled = value != "false"
		}
	}

	// Precedence: headroom > increment > threshold (spec 7.1). A deprecated
	// annotation only produces a warning when nothing overrode it.
	for res, v := range fromThreshold {
		if _, overridden := fromHeadroom[res]; overridden {
			continue
		}
		if _, overridden := fromIncrement[res]; overridden {
			continue
		}
		out.Headroom[res] = v
		warnings = append(warnings, deprecationWarning(res, "threshold"))
	}
	for res, v := range fromIncrement {
		if _, overridden := fromHeadroom[res]; overridden {
			continue
		}
		out.Headroom[res] = v
		warnings = append(warnings, deprecationWarning(res, "increment"))
	}
	for res, v := range fromHeadroom {
		out.Headroom[res] = v
	}

	return out, warnings
}

// suffixKey turns "cpu-headroom" into "cpu" and a bare "headroom" into the
// namespace default key.
func suffixKey(name, suffix string) corev1.ResourceName {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(name, suffix), "-")
	if trimmed == "" {
		return DefaultKey
	}
	return corev1.ResourceName(trimmed)
}

// parseFraction accepts "0.25" and "25%" and returns a fraction.
func parseFraction(value string) (float64, bool) {
	if strings.HasSuffix(value, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
		if err != nil {
			return 0, false
		}
		return v / 100.0, true
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func deprecationWarning(res corev1.ResourceName, old string) string {
	prefix := string(res) + "-"
	if res == DefaultKey {
		prefix = ""
	}
	return fmt.Sprintf(
		"annotation %s%s%s is deprecated, use %s%sheadroom instead",
		AnnotationPrefix, prefix, old, AnnotationPrefix, prefix)
}

func copyFloatMap(in map[corev1.ResourceName]float64) map[corev1.ResourceName]float64 {
	out := make(map[corev1.ResourceName]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyQuantityMap(
	in map[corev1.ResourceName]resource.Quantity,
) map[corev1.ResourceName]resource.Quantity {
	out := make(map[corev1.ResourceName]resource.Quantity, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sizing/... -v`
Expected: PASS for all four policy tests.

- [ ] **Step 5: Lint**

Run: `make lint`
Expected: no findings. If `gocyclo` flags `ParsePolicy`, extract the scalar
cases into a helper `parseScalar(name, value string, out *Policy) bool` and
call it from the default branch of the switch.

- [ ] **Step 6: Commit**

```bash
git add internal/sizing/policy.go internal/sizing/policy_test.go
git commit -m "feat(sizing): policy parsing with deprecation migration

Introduces the headroom-based Policy that replaces the threshold and
increment settings. Existing annotations keep working through a fallback
chain (headroom > increment > threshold), so installations that only set
the old keys retain their current grow behaviour."
```

---

### Task 3: Observation window — codec, sampling, coverage

Implements spec 4.1–4.3. The subtle part is `maxGap`: a controller that ran
ten minutes a day would otherwise produce 14 day buckets and a dangerously low
peak. Coverage is therefore derived from the largest gap between consecutive
samples, not from the presence of a bucket.

**Files:**
- Create: `internal/sizing/window.go`
- Test: `internal/sizing/window_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const WindowVersion = 1`
  - `type DayBucket struct { Date, First, Last, MaxGap string; N int; Peaks map[string]string }`
  - `type Window struct { Version int; UID, LastSampleAt, LastWriteAt string; Days []DayBucket }`
  - `func DecodeWindow(raw string) Window`
  - `func EncodeWindow(w Window) (string, error)`
  - `func (w *Window) Observe(now time.Time, uid string, used corev1.ResourceList, windowDays int) bool`
  - `func (w Window) Peak(res corev1.ResourceName, now time.Time, windowDays int) (int64, bool)`
  - `func (w Window) IsComplete(res corev1.ResourceName, now time.Time, windowDays int) bool`

- [ ] **Step 1: Write the failing test**

Create `internal/sizing/window_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sizing/... -run TestWindow -v`
Expected: build failure — `undefined: Window`, `undefined: DecodeWindow`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/sizing/window.go`:

```go
package sizing

import (
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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
	return w
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sizing/... -v`
Expected: PASS for all window tests.

If `TestWindow_IsComplete/fully_sampled_window_is_complete` fails, check that
`fillWindow` starts exactly at midnight — `Truncate(24 * time.Hour)` on a UTC
time does that, and the first sample of each day must land at `00:00`.

- [ ] **Step 5: Lint**

Run: `make lint`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/sizing/window.go internal/sizing/window_test.go
git commit -m "feat(sizing): rolling observation window with coverage tracking

The window records a per-resource daily maximum of status.used together with
the largest gap between consecutive samples. Coverage is derived from that
gap rather than from the mere presence of a day bucket, so controller
downtime invalidates the affected days instead of producing an artificially
low peak."
```

---

### Task 4: Move the pure deficit helpers into the sizing package

`calculateWorkloadDeficit` needs the Kubernetes client and stays in the
controller. The four helpers it builds on are pure and belong next to the
decision logic. This is a move, not a rewrite: the existing behaviour and its
test cases carry over unchanged.

**Files:**
- Create: `internal/sizing/deficit.go`
- Create: `internal/sizing/deficit_test.go`
- Modify: `internal/controller/resourcequota_utils.go` — delete the moved
  functions, call the `sizing` package instead
- Delete: `internal/controller/limits_test.go` (covers `getPodRequests`) and
  `internal/controller/event_parser_test.go` (covers `parseEventMessage`) —
  both functions move, so their cases move with them
- Leave alone: `internal/controller/utils_test.go` holds only
  `TestConvertToReadableFormat`, which covers a function this task does not
  touch. It is removed later, together with `convertToReadableFormat` itself.
- Leave alone: `internal/controller/smart_calculation_test.go` covers
  `calculateWorkloadDeficit`, which needs the Kubernetes client and stays in
  the controller.

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func ParseEventMessage(message string) (corev1.ResourceName, resource.Quantity, error)`
  - `func WorkloadKey(name string) string`
  - `func PodRequests(spec corev1.PodSpec) map[corev1.ResourceName]int64`
  - `func PVCRequests(templates []corev1.PersistentVolumeClaim) map[corev1.ResourceName]int64`

- [ ] **Step 1: Move the functions verbatim**

Create `internal/sizing/deficit.go` containing exactly the bodies of
`parseEventMessage`, `getWorkloadKey`, `getPodRequests` and `getPVCRequests`
from `internal/controller/resourcequota_utils.go`, renamed to the exported
names above. Do not change any logic — the effective-request rule
`max(sum(app containers), max(init containers))` and the short-to-long key
mapping (`cpu` → `requests.cpu`) must stay byte-for-byte equivalent.

Add the package doc comment at the top:

```go
// Package sizing computes quota resize decisions. It has no Kubernetes client
// dependency: every function takes plain values and an explicit clock, so the
// time-dependent shrink gates can be tested exhaustively.
package sizing
```

Put this comment in `deficit.go` only — a package may declare its doc comment
once.

- [ ] **Step 2: Port the existing tests**

Create `internal/sizing/deficit_test.go`. Move the test bodies from
`internal/controller/limits_test.go` (`TestGetPodRequests_Limits`) and
`internal/controller/event_parser_test.go` (`TestParseEventMessage`),
renaming the calls to the exported functions. Change `package controller` to
`package sizing` and drop imports that are no longer needed.

`WorkloadKey` and `PVCRequests` have no existing test. Add a small table for
each: `WorkloadKey` covers the pod-suffix case (`app-a-6b474476c4-xfg2z` →
`app-a-6b474476c4`), the StatefulSet case (`web-0` → `web`) and a name with
no hyphen at all (returned unchanged). `PVCRequests` covers a single template
and two templates, asserting the storage requests are summed and mapped onto
`requests.storage`.

Add one case that the old suite lacked, covering the init-container rule:

```go
func TestPodRequests_InitContainerDominates(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name: "migrate",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
		}},
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				},
			},
		}},
	}

	reqs := PodRequests(spec)

	if got := reqs[corev1.ResourceRequestsCPU]; got != 2000 {
		t.Fatalf("requests.cpu = %d milli, want 2000 (init container wins)", got)
	}
}
```

- [ ] **Step 3: Update the controller call sites**

In `internal/controller/resourcequota_utils.go`, delete the four moved
functions and route `calculateWorkloadDeficit` through the sizing package:

```go
	key := sizing.WorkloadKey(evt.InvolvedObject.Name)
```

```go
		reqs := sizing.PodRequests(podSpec)

		if len(pvcTemplates) > 0 {
			pvcReqs := sizing.PVCRequests(pvcTemplates)
			for k, v := range pvcReqs {
				reqs[k] += v
			}
		}
```

In `internal/controller/resourcequota_controller.go`, `analyzeEvents` calls
`parseEventMessage`; change it to `sizing.ParseEventMessage`. Add the import
`"github.com/payback159/namespace-resizer/internal/sizing"` to both files.

Delete `internal/controller/limits_test.go`.

- [ ] **Step 4: Run the full suite**

Run: `make test`
Expected: PASS. The controller package must still compile — if
`convertToReadableFormat` is now the only remaining pure helper there, leave
it for Task 8, which replaces it with `sizing.Quantize`.

- [ ] **Step 5: Lint**

Run: `make lint`
Expected: no findings. `goconst` may flag repeated resource-name literals in
the new test file; the `.golangci.yml` exclusion for `_test\.go` already
covers that.

- [ ] **Step 6: Commit**

```bash
git add internal/sizing/deficit.go internal/sizing/deficit_test.go \
        internal/controller/resourcequota_utils.go \
        internal/controller/resourcequota_controller.go
git rm internal/controller/limits_test.go internal/controller/event_parser_test.go
git commit -m "refactor(sizing): move pure deficit helpers out of the controller

Event message parsing, workload keys and pod/PVC request aggregation carry no
Kubernetes client dependency and belong next to the decision logic. Behaviour
is unchanged; the existing cases move with the code and a case for the
init-container effective-request rule is added."
```

---

### Task 5: The decision function

The heart of the change. Implements the target formula (spec 3), the tolerance
band (spec 3.2) and the shrink gates (spec 3.3). The `lock` gate from the spec
is enforced by the reconciler, which holds the Lease, not by `Decide`.

**Files:**
- Create: `internal/sizing/decide.go`
- Test: `internal/sizing/decide_test.go`

**Interfaces:**
- Consumes: `Policy`, `HeadroomFor`, `MinFor` (Task 2); `Window`, `Peak`,
  `IsComplete` (Task 3); `Quantize` (Task 1).
- Produces:
  - `type Direction int` with `DirectionNone`, `DirectionGrow`, `DirectionShrink`
  - `func (d Direction) String() string` returning `"none"`, `"grow"`, `"shrink"`
  - `type Gate string` with `GateEnabled`, `GateWindow`, `GateRecentGrow`, `GateCooldown`
  - `type Input struct { Now time.Time; Hard, Used corev1.ResourceList;
    Deficits map[corev1.ResourceName]int64; Window Window; Policy Policy;
    LastGrow, LastShrink time.Time }`
  - `type Decision struct { Direction Direction; Targets, ShrinkPreview
    map[corev1.ResourceName]resource.Quantity; Reason string; BlockedBy []Gate }`
  - `func Decide(in Input) Decision`

`ShrinkPreview` exists for the dry-run rollout in spec 8: it holds the shrink
targets that *would* be proposed, even when a gate blocked them. `Targets`
stays strict — callers may only ever act on `Targets`.

- [ ] **Step 1: Write the failing test**

Create `internal/sizing/decide_test.go`:

```go
package sizing

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// baseInput builds an Input whose window is fully covered at the given peak,
// so every shrink gate passes unless a test deliberately breaks one.
func baseInput(hard, usedNow, peak string) Input {
	policy := DefaultPolicy()
	return Input{
		Now: testNow,
		Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU: resource.MustParse(hard),
		},
		Used: corev1.ResourceList{
			corev1.ResourceRequestsCPU: resource.MustParse(usedNow),
		},
		Window: fillWindow(testNow, policy.WindowDays, peak),
		Policy: policy,
	}
}

func targetCPU(t *testing.T, d Decision) string {
	t.Helper()
	qty, ok := d.Targets[corev1.ResourceRequestsCPU]
	if !ok {
		t.Fatalf("no target for requests.cpu, decision = %+v", d)
	}
	return qty.String()
}

func TestDecide_GrowOnDeficit(t *testing.T) {
	in := baseInput("10", "10", "10")
	in.Deficits = map[corev1.ResourceName]int64{
		corev1.ResourceRequestsCPU: 6000,
	}

	got := Decide(in)

	if got.Direction != DirectionGrow {
		t.Fatalf("direction = %v, want grow", got.Direction)
	}
	// peak = used + deficit = 16, target = 16 * 1.25 = 20
	if want := "20"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s", targetCPU(t, got), want)
	}
}

func TestDecide_ShrinkIsStepCapped(t *testing.T) {
	// hard 16, peak 4, used 3.5 -> target 5, but the 25% cap allows only 12.
	in := baseInput("16", "3500m", "4")

	got := Decide(in)

	if got.Direction != DirectionShrink {
		t.Fatalf("direction = %v, want shrink (blocked by %v)", got.Direction, got.BlockedBy)
	}
	if want := "12"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s", targetCPU(t, got), want)
	}
}

func TestDecide_ToleranceBandIsQuiet(t *testing.T) {
	// used = peak = 4, target = 5. The band is hard in [4.35 .. 5.88].
	for _, hard := range []string{"4500m", "5", "5800m"} {
		t.Run("hard="+hard, func(t *testing.T) {
			got := Decide(baseInput(hard, "4", "4"))
			if got.Direction != DirectionNone {
				t.Fatalf("direction = %v, want none for hard=%s", got.Direction, hard)
			}
		})
	}
}

func TestDecide_HardFloorFromCurrentUsage(t *testing.T) {
	// The window says 1 CPU, but 8 CPU are in use right now. The floor wins.
	in := baseInput("16", "8", "1")

	got := Decide(in)

	if got.Direction != DirectionShrink {
		t.Fatalf("direction = %v, want shrink", got.Direction)
	}
	// floor = 8 * 1.25 = 10, which is above the step cap of 12? No: the cap
	// allows 12 and the floor allows 10, so the higher of the two wins.
	if want := "12"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s", targetCPU(t, got), want)
	}
}

func TestDecide_MinAnnotationIsRespected(t *testing.T) {
	// Demand alone would justify shrinking all the way to the step cap of 12.
	// The configured minimum of 13 stops it one notch short, and 13 is still
	// below the shrink threshold of 16 * 0.85 = 13.6, so a shrink happens.
	in := baseInput("16", "1", "1")
	in.Policy.Min = map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("13"),
	}

	got := Decide(in)

	if got.Direction != DirectionShrink {
		t.Fatalf("direction = %v, want shrink", got.Direction)
	}
	if want := "13"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s (min annotation, not the step cap of 12)",
			targetCPU(t, got), want)
	}
}

func TestDecide_MinAnnotationCanCloseTheBand(t *testing.T) {
	// A minimum of 14 lifts the target into the tolerance band around the
	// current limit of 16 (13.6 .. 18.4). The minimum raises the target; it
	// does not authorise a change that the band forbids.
	in := baseInput("16", "1", "1")
	in.Policy.Min = map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("14"),
	}

	got := Decide(in)

	if got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none — target 14 sits inside the band",
			got.Direction)
	}
}

func TestDecide_GrowBeatsShrink(t *testing.T) {
	policy := DefaultPolicy()
	in := Input{
		Now: testNow,
		Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("64Gi"),
		},
		Used: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("1Gi"),
		},
		Window: fillWindow(testNow, policy.WindowDays, "4"),
		Policy: policy,
	}

	got := Decide(in)

	if got.Direction != DirectionGrow {
		t.Fatalf("direction = %v, want grow", got.Direction)
	}
	if _, ok := got.Targets[corev1.ResourceRequestsMemory]; ok {
		t.Fatal("memory shrink target leaked into a grow decision")
	}
}

func TestDecide_ShrinkGates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Input)
		wantBad Gate
	}{
		{
			name:    "shrink disabled",
			mutate:  func(in *Input) { in.Policy.ShrinkEnabled = false },
			wantBad: GateEnabled,
		},
		{
			name:    "window incomplete",
			mutate:  func(in *Input) { in.Window = fillWindow(testNow, 3, "4") },
			wantBad: GateWindow,
		},
		{
			name:    "grow inside the window",
			mutate:  func(in *Input) { in.LastGrow = testNow.Add(-48 * time.Hour) },
			wantBad: GateRecentGrow,
		},
		{
			name:    "shrink cooldown running",
			mutate:  func(in *Input) { in.LastShrink = testNow.Add(-24 * time.Hour) },
			wantBad: GateCooldown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput("16", "3500m", "4")
			tc.mutate(&in)

			got := Decide(in)

			if got.Direction != DirectionNone {
				t.Fatalf("direction = %v, want none", got.Direction)
			}
			found := false
			for _, gate := range got.BlockedBy {
				if gate == tc.wantBad {
					found = true
				}
			}
			if !found {
				t.Fatalf("BlockedBy = %v, want it to contain %q", got.BlockedBy, tc.wantBad)
			}
		})
	}
}

func TestDecide_BlockedShrinkStillReportsAPreview(t *testing.T) {
	in := baseInput("16", "3500m", "4")
	in.Policy.ShrinkEnabled = false

	got := Decide(in)

	if got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none", got.Direction)
	}
	if len(got.Targets) != 0 {
		t.Fatalf("Targets = %v, want empty when a gate blocks", got.Targets)
	}
	qty, ok := got.ShrinkPreview[corev1.ResourceRequestsCPU]
	if !ok {
		t.Fatal("ShrinkPreview is empty, want the would-be target for dry-run metrics")
	}
	if qty.String() != "12" {
		t.Fatalf("preview = %s, want 12", qty.String())
	}
}

func TestDecide_GatesDoNotBlockGrow(t *testing.T) {
	in := baseInput("10", "10", "10")
	in.Deficits = map[corev1.ResourceName]int64{corev1.ResourceRequestsCPU: 6000}
	in.Policy.ShrinkEnabled = false
	in.LastShrink = testNow.Add(-1 * time.Hour)
	in.Window = Window{Version: WindowVersion}

	got := Decide(in)

	if got.Direction != DirectionGrow {
		t.Fatalf("direction = %v, want grow regardless of shrink gates", got.Direction)
	}
}

func TestDecide_DisabledNamespaceDoesNothing(t *testing.T) {
	in := baseInput("16", "3500m", "4")
	in.Policy.Enabled = false

	if got := Decide(in); got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none for a disabled namespace", got.Direction)
	}
}

func TestDecide_ZeroHardIsSkipped(t *testing.T) {
	in := baseInput("0", "0", "4")

	if got := Decide(in); got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none when hard is zero", got.Direction)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sizing/... -run TestDecide -v`
Expected: build failure — `undefined: Decide`, `undefined: DirectionGrow`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/sizing/decide.go`:

```go
package sizing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Direction is the kind of change a Decision asks for.
type Direction int

const (
	// DirectionNone means the quota is inside the tolerance band, or a shrink
	// was blocked by a gate.
	DirectionNone Direction = iota
	// DirectionGrow raises limits. It always wins over a pending shrink.
	DirectionGrow
	// DirectionShrink lowers limits and is never auto-merged.
	DirectionShrink
)

func (d Direction) String() string {
	switch d {
	case DirectionGrow:
		return "grow"
	case DirectionShrink:
		return "shrink"
	default:
		return "none"
	}
}

// Gate names a precondition that must hold before a shrink is proposed.
type Gate string

const (
	// GateEnabled covers the global flag and the namespace opt-out.
	GateEnabled Gate = "enabled"
	// GateWindow requires a fully covered observation window.
	GateWindow Gate = "window"
	// GateRecentGrow blocks a shrink that follows a grow inside the window.
	GateRecentGrow Gate = "recent-grow"
	// GateCooldown enforces the long shrink cooldown.
	GateCooldown Gate = "cooldown"
)

// Input is everything Decide needs. It carries no client and no ambient clock.
type Input struct {
	Now  time.Time
	Hard corev1.ResourceList
	Used corev1.ResourceList
	// Deficits holds per-resource milli-values derived from FailedCreate
	// events, or nil when no event is pending.
	Deficits   map[corev1.ResourceName]int64
	Window     Window
	Policy     Policy
	LastGrow   time.Time
	LastShrink time.Time
}

// Decision is the result of one evaluation.
type Decision struct {
	Direction Direction
	// Targets is what the caller may act on. It is empty unless Direction is
	// Grow or Shrink.
	Targets map[corev1.ResourceName]resource.Quantity
	// ShrinkPreview holds the shrink targets that would be proposed if no gate
	// blocked them. It exists so the dry-run rollout can report what the
	// controller would do while shrinking is still switched off. Never act on
	// it — only Targets is authoritative.
	ShrinkPreview map[corev1.ResourceName]resource.Quantity
	Reason        string
	BlockedBy     []Gate
}

// Decide computes the target for every quota key and folds the per-resource
// candidates into a single direction. Growth always wins: a quota that needs
// to grow anywhere discards all shrink candidates, because a PR that raises
// one resource while lowering another cannot be reviewed or merged sensibly.
func Decide(in Input) Decision {
	if !in.Policy.Enabled {
		return Decision{Direction: DirectionNone}
	}

	growTargets := map[corev1.ResourceName]resource.Quantity{}
	shrinkTargets := map[corev1.ResourceName]resource.Quantity{}
	var growReasons, shrinkReasons []string

	for res, hard := range in.Hard {
		hardMilli := hard.MilliValue()
		if hardMilli == 0 {
			continue
		}

		used, ok := in.Used[res]
		if !ok {
			// A hard key the API server has not reported usage for. Reading
			// the usage as zero would make it a shrink candidate aiming at
			// nothing, so skip it exactly as the metric loop this replaces did.
			continue
		}
		usedMilli := used.MilliValue()

		headroom := in.Policy.HeadroomFor(res)
		targetMilli, driver := targetFor(in, res, usedMilli, headroom)

		switch {
		case float64(targetMilli) > float64(hardMilli)*(1+in.Policy.Tolerance):
			qty := Quantize(res, targetMilli, hard.Format)
			if qty.Cmp(hard) <= 0 {
				// Rounding erased the increase; nothing to propose.
				continue
			}
			growTargets[res] = qty
			growReasons = append(growReasons,
				describe(res, hard, qty, driver))

		case float64(targetMilli) < float64(hardMilli)*(1-in.Policy.Tolerance):
			capped := int64(float64(hardMilli) * (1 - in.Policy.MaxShrinkStep))
			if capped > targetMilli {
				targetMilli = capped
				driver = "step cap"
			}
			qty := Quantize(res, targetMilli, hard.Format)
			if qty.Cmp(hard) >= 0 {
				// Rounding erased the decrease; nothing to propose.
				continue
			}
			shrinkTargets[res] = qty
			shrinkReasons = append(shrinkReasons,
				describe(res, hard, qty, driver))
		}
	}

	if len(growTargets) > 0 {
		sort.Strings(growReasons)
		return Decision{
			Direction: DirectionGrow,
			Targets:   growTargets,
			Reason:    strings.Join(growReasons, "\n"),
		}
	}

	if len(shrinkTargets) == 0 {
		return Decision{Direction: DirectionNone}
	}

	if blocked := shrinkGates(in, shrinkTargets); len(blocked) > 0 {
		return Decision{
			Direction:     DirectionNone,
			ShrinkPreview: shrinkTargets,
			BlockedBy:     blocked,
		}
	}

	sort.Strings(shrinkReasons)
	return Decision{
		Direction:     DirectionShrink,
		Targets:       shrinkTargets,
		ShrinkPreview: shrinkTargets,
		Reason:    strings.Join(shrinkReasons, "\n"),
	}
}

// targetFor implements the formula from spec 3 and reports which term decided
// the outcome, for the PR body.
func targetFor(
	in Input,
	res corev1.ResourceName,
	usedMilli int64,
	headroom float64,
) (int64, string) {
	peakMilli := usedMilli
	driver := "current usage"

	if p, ok := in.Window.Peak(res, in.Now, in.Policy.WindowDays); ok && p > peakMilli {
		peakMilli = p
		driver = fmt.Sprintf("%d-day peak", in.Policy.WindowDays)
	}
	if deficit, ok := in.Deficits[res]; ok && deficit > 0 {
		if need := usedMilli + deficit; need > peakMilli {
			peakMilli = need
			driver = "pending shortage"
		}
	}

	target := int64(float64(peakMilli) * (1 + headroom))

	if floor := int64(float64(usedMilli) * (1 + headroom)); floor > target {
		target = floor
		driver = "current usage floor"
	}
	if min, ok := in.Policy.MinFor(res); ok && min.MilliValue() > target {
		target = min.MilliValue()
		driver = "configured minimum"
	}

	return target, driver
}

// shrinkGates returns every gate from spec 3.3 that currently blocks a shrink.
// The lock gate is enforced by the reconciler, which owns the Lease.
func shrinkGates(in Input, targets map[corev1.ResourceName]resource.Quantity) []Gate {
	var blocked []Gate

	if !in.Policy.ShrinkEnabled {
		blocked = append(blocked, GateEnabled)
	}
	for res := range targets {
		if !in.Window.IsComplete(res, in.Now, in.Policy.WindowDays) {
			blocked = append(blocked, GateWindow)
			break
		}
	}
	windowSpan := time.Duration(in.Policy.WindowDays) * 24 * time.Hour
	if !in.LastGrow.IsZero() && in.Now.Sub(in.LastGrow) < windowSpan {
		blocked = append(blocked, GateRecentGrow)
	}
	if !in.LastShrink.IsZero() && in.Now.Sub(in.LastShrink) < in.Policy.ShrinkCooldown {
		blocked = append(blocked, GateCooldown)
	}

	return blocked
}

func describe(
	res corev1.ResourceName,
	from resource.Quantity,
	to resource.Quantity,
	driver string,
) string {
	return fmt.Sprintf("- `%s`: %s -> %s (driven by %s)",
		res, from.String(), to.String(), driver)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sizing/... -v`
Expected: PASS for all `TestDecide` cases.

`TestDecide_HardFloorFromCurrentUsage` is the one to watch: the floor is
`8 * 1.25 = 10` and the step cap is `16 * 0.75 = 12`. The cap is the higher
value and therefore the target. If the assertion fails with `10`, the cap is
being applied before the floor instead of after — `targetFor` must compute the
floor, and `Decide` must apply the cap afterwards.

**If any expected value here disagrees with the code, the expected value is
what to question — do not add logic to satisfy it.** The tolerance band and
the configured minimum are independent: the minimum raises the target, and the
band then decides whether that target is far enough from the current limit to
act on. A target that the minimum lifted *into* the band means no change, and
`TestDecide_MinAnnotationCanCloseTheBand` pins exactly that. A special case
that let the minimum bypass the band would emit pull requests for changes of a
few percent and defeat the band's purpose.

- [ ] **Step 5: Lint**

Run: `make lint`
Expected: no findings. `min` is a Go builtin since 1.21; `revive` may complain
about shadowing it in `targetFor`. If it does, rename the variable to
`lowerBound`.

- [ ] **Step 6: Commit**

```bash
git add internal/sizing/decide.go internal/sizing/decide_test.go
git commit -m "feat(sizing): bidirectional decision function

Decide computes target = max(window peak, current usage, pending shortage) *
(1 + headroom) for every quota key and classifies it against a tolerance band.
Growth always wins over shrinking, and shrinking is additionally gated on a
complete observation window, no recent growth, and an expired cooldown."
```

---

### Task 6: Lease state accessor

Implements spec 6.4. Today the controller reads the Lease once per concern
(`GetLock`, `GetLastModified`) and writes it through several single-purpose
methods. The new fields would multiply that. `State` collapses it into one
read and one guarded read-modify-write.

**Files:**
- Create: `internal/lock/state.go`
- Test: `internal/lock/state_test.go`
- Modify: `internal/lock/lease.go` — new annotation constants only

**Interfaces:**
- Consumes: `LeaseLocker` (existing).
- Produces:
  - `type State struct { PRID int; PRDirection string; LastModified, LastGrow, LastShrink time.Time; Window string }`
  - `func (l *LeaseLocker) GetState(ctx context.Context, targetNS, quotaName string) (State, error)`
  - `func (l *LeaseLocker) MutateState(ctx context.Context, targetNS, quotaName string, fn func(*State)) error`
  - Constants `AnnotationLastGrow`, `AnnotationLastShrink`, `AnnotationPRDirection`, `AnnotationWindow`

- [ ] **Step 1: Write the failing test**

Create `internal/lock/state_test.go`:

```go
package lock

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newStateLocker() *LeaseLocker {
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewLeaseLocker(c)
}

func TestGetState_MissingLeaseIsZero(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()

	state, err := locker.GetState(context.Background(), testNamespace, testQuotaName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.PRDirection).To(BeEmpty())
	g.Expect(state.Window).To(BeEmpty())
	g.Expect(state.LastGrow.IsZero()).To(BeTrue())
}

func TestMutateState_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()
	ctx := context.Background()
	grownAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	err := locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 42
		s.PRDirection = "shrink"
		s.LastGrow = grownAt
		s.Window = `{"v":1,"days":[]}`
	})
	g.Expect(err).NotTo(HaveOccurred())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(42))
	g.Expect(state.PRDirection).To(Equal("shrink"))
	g.Expect(state.LastGrow.Equal(grownAt)).To(BeTrue())
	g.Expect(state.Window).To(Equal(`{"v":1,"days":[]}`))
}

func TestMutateState_ClearingPRIDReleasesTheLock(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()
	ctx := context.Background()

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 7
		s.PRDirection = "grow"
	})).To(Succeed())

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 0
		s.PRDirection = ""
		s.LastShrink = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	})).To(Succeed())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.PRDirection).To(BeEmpty())
	g.Expect(state.LastShrink.IsZero()).To(BeFalse())
}

func TestMutateState_PreservesExistingLastModified(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()
	ctx := context.Background()
	modified := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)

	g.Expect(locker.SetLastModified(ctx, testNamespace, testQuotaName, modified)).To(Succeed())

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.Window = "{}"
	})).To(Succeed())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.LastModified.Equal(modified)).To(BeTrue())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lock/... -run 'TestGetState|TestMutateState' -v`
Expected: build failure — `locker.GetState undefined`.

- [ ] **Step 3: Add the annotation constants**

In `internal/lock/lease.go`, extend the existing `const` block:

```go
	// AnnotationLastGrow stores when the controller last proposed a growth.
	AnnotationLastGrow = "resizer.io/last-grow"
	// AnnotationLastShrink stores when the controller last proposed, closed or
	// expired a shrink. It drives the shrink cooldown gate.
	AnnotationLastShrink = "resizer.io/last-shrink"
	// AnnotationPRDirection records whether the open PR grows or shrinks.
	AnnotationPRDirection = "resizer.io/pr-direction"
	// AnnotationWindow stores the JSON-encoded observation window.
	AnnotationWindow = "resizer.io/observation-window"
```

- [ ] **Step 4: Write minimal implementation**

Create `internal/lock/state.go`:

```go
package lock

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// State is the complete controller-owned state for one quota, as persisted on
// its Lease. Reading and writing it as a unit keeps the number of API round
// trips constant as more fields are added.
type State struct {
	// PRID is the open pull request, or 0 when the lock is free.
	PRID int
	// PRDirection is "grow", "shrink", or empty when no PR is open.
	PRDirection string

	LastModified time.Time
	LastGrow     time.Time
	LastShrink   time.Time

	// Window is the raw JSON observation window. The lock package does not
	// interpret it; sizing.DecodeWindow does.
	Window string
}

// GetState reads the full state in a single API call. A missing Lease yields
// the zero State, which is the correct starting point for a new quota.
func (l *LeaseLocker) GetState(ctx context.Context, targetNS, quotaName string) (State, error) {
	leaseName := l.getLeaseName(targetNS, quotaName)

	var lease coordinationv1.Lease
	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if err != nil {
		if errors.IsNotFound(err) {
			return State{}, nil
		}
		return State{}, err
	}
	return stateFromLease(&lease), nil
}

// MutateState applies fn to the current state and writes the result back,
// retrying on optimistic-concurrency conflicts. The Lease is created if it does
// not exist yet, so callers need no separate bootstrap step.
func (l *LeaseLocker) MutateState(
	ctx context.Context,
	targetNS, quotaName string,
	fn func(*State),
) error {
	leaseName := l.getLeaseName(targetNS, quotaName)

	if err := l.ensureLeaseExists(ctx, leaseName, targetNS, quotaName); err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var lease coordinationv1.Lease
		key := client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}
		if err := l.client.Get(ctx, key, &lease); err != nil {
			return err
		}

		state := stateFromLease(&lease)
		fn(&state)
		applyStateToLease(&state, &lease)

		return l.client.Update(ctx, &lease)
	})
}

func stateFromLease(lease *coordinationv1.Lease) State {
	state := State{
		PRDirection:  lease.Annotations[AnnotationPRDirection],
		Window:       lease.Annotations[AnnotationWindow],
		LastModified: parseStamp(lease.Annotations[AnnotationLastModified]),
		LastGrow:     parseStamp(lease.Annotations[AnnotationLastGrow]),
		LastShrink:   parseStamp(lease.Annotations[AnnotationLastShrink]),
	}
	if lease.Spec.HolderIdentity != nil {
		var id int
		if _, err := fmt.Sscanf(*lease.Spec.HolderIdentity, "pr-%d", &id); err == nil {
			state.PRID = id
		}
	}
	return state
}

func applyStateToLease(state *State, lease *coordinationv1.Lease) {
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	setStamp(lease.Annotations, AnnotationLastModified, state.LastModified)
	setStamp(lease.Annotations, AnnotationLastGrow, state.LastGrow)
	setStamp(lease.Annotations, AnnotationLastShrink, state.LastShrink)
	setString(lease.Annotations, AnnotationPRDirection, state.PRDirection)
	setString(lease.Annotations, AnnotationWindow, state.Window)

	if state.PRID == 0 {
		lease.Spec.HolderIdentity = nil
		return
	}
	identity := fmt.Sprintf("pr-%d", state.PRID)
	lease.Spec.HolderIdentity = &identity
}

func parseStamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	stamp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return stamp
}

func setStamp(annotations map[string]string, key string, value time.Time) {
	if value.IsZero() {
		delete(annotations, key)
		return
	}
	annotations[key] = value.UTC().Format(time.RFC3339)
}

func setString(annotations map[string]string, key, value string) {
	if value == "" {
		delete(annotations, key)
		return
	}
	annotations[key] = value
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/lock/... -v`
Expected: PASS, including the pre-existing lease and GC tests.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/lock/state.go internal/lock/state_test.go internal/lock/lease.go
git commit -m "feat(lock): single-round-trip state accessor for the Lease

GetState reads PR id, direction, the three timestamps and the observation
window in one call; MutateState writes them back atomically with conflict
retries. The existing single-purpose methods stay until their call sites move."
```

---

### Task 7: The observer

Implements spec 4.1. The write-behind cache is what makes `maxGap` meaningful:
if `lastSampleAt` were only ever read back from the Lease, a five-minute
sampling interval with hourly writes would compute a one-hour gap on every
sample and permanently invalidate every day.

**Files:**
- Create: `internal/controller/observation.go`
- Test: `internal/controller/observation_test.go`

**Interfaces:**
- Consumes: `lock.LeaseLocker`, `lock.State` (Task 6); `sizing.Window`,
  `DecodeWindow`, `EncodeWindow` (Task 3).
- Produces:
  - `type Observer struct { ... }`
  - `func NewObserver(locker *lock.LeaseLocker, now func() time.Time) *Observer`
  - `func (o *Observer) Observe(ctx context.Context, quota *corev1.ResourceQuota, windowDays int) (sizing.Window, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/observation_test.go`:

```go
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/lock"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func observedQuota(usedCPU string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "compute",
			Namespace: "team-a",
			UID:       types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("16"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse(usedCPU),
			},
		},
	}
}

func newObserverHarness() (*Observer, *lock.LeaseLocker, *time.Time) {
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	locker := lock.NewLeaseLocker(c)

	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	clock := &now
	return NewObserver(locker, func() time.Time { return *clock }), locker, clock
}

func TestObserver_PersistsFirstSample(t *testing.T) {
	g := NewWithT(t)
	observer, locker, _ := newObserverHarness()
	ctx := context.Background()

	window, err := observer.Observe(ctx, observedQuota("4"), 14)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(window.Days).To(HaveLen(1))

	state, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.Window).NotTo(BeEmpty())
}

func TestObserver_SkipsWriteWhenNothingChanged(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()
	quota := observedQuota("4")

	_, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	before, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())

	// Five minutes later, same usage: no new peak, no day roll-over, and the
	// hourly heartbeat has not elapsed.
	*clock = clock.Add(5 * time.Minute)
	_, err = observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	after, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after.Window).To(Equal(before.Window))
}

func TestObserver_WritesOnHeartbeat(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()
	quota := observedQuota("4")

	_, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())
	before, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())

	*clock = clock.Add(90 * time.Minute)
	_, err = observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	after, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after.Window).NotTo(Equal(before.Window))
}

func TestObserver_TracksRisingPeak(t *testing.T) {
	g := NewWithT(t)
	observer, _, clock := newObserverHarness()
	ctx := context.Background()

	_, err := observer.Observe(ctx, observedQuota("4"), 14)
	g.Expect(err).NotTo(HaveOccurred())

	*clock = clock.Add(10 * time.Minute)
	window, err := observer.Observe(ctx, observedQuota("9"), 14)
	g.Expect(err).NotTo(HaveOccurred())

	peaks := window.Days[0].Peaks
	g.Expect(peaks).To(HaveKeyWithValue(string(corev1.ResourceRequestsCPU), "9"))
}

func TestObserver_ReloadsFromLeaseOnColdCache(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()

	_, err := observer.Observe(ctx, observedQuota("4"), 14)
	g.Expect(err).NotTo(HaveOccurred())

	// A fresh Observer stands in for a restarted controller.
	restarted := NewObserver(locker, func() time.Time { return *clock })
	*clock = clock.Add(20 * time.Minute)

	window, err := restarted.Observe(ctx, observedQuota("4"), 14)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(window.Days).To(HaveLen(1))
	g.Expect(window.Days[0].N).To(BeNumerically(">=", 2))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/... -run TestObserver -v`
Expected: build failure — `undefined: NewObserver`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/controller/observation.go`:

```go
package controller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
)

// observationHeartbeat is the longest a sample may go unpersisted. It bounds
// how much history a controller crash can lose, and therefore how stale the
// lastSampleAt fallback can be after a restart.
const observationHeartbeat = time.Hour

// Observer folds status.used into a rolling window and persists it on the state
// Lease. It keeps a write-behind cache: every reconcile updates the in-memory
// window, but the Lease is written only when a peak rises, a day rolls over, or
// the heartbeat elapses. Without the cache the persisted lastSampleAt would lag
// a full heartbeat behind, and every sample would look like an hour-long gap.
type Observer struct {
	locker *lock.LeaseLocker
	now    func() time.Time

	mu     sync.Mutex
	cached map[string]sizing.Window
}

// NewObserver builds an Observer. now is injected so tests can drive the clock.
func NewObserver(locker *lock.LeaseLocker, now func() time.Time) *Observer {
	if now == nil {
		now = time.Now
	}
	return &Observer{
		locker: locker,
		now:    now,
		cached: map[string]sizing.Window{},
	}
}

// Observe records one sample and returns the current window. A cold cache — a
// freshly started controller — falls back to the persisted window, so the first
// sample after a restart correctly measures the outage as a gap.
func (o *Observer) Observe(
	ctx context.Context,
	quota *corev1.ResourceQuota,
	windowDays int,
) (sizing.Window, error) {
	key := quota.Namespace + "/" + quota.Name

	o.mu.Lock()
	defer o.mu.Unlock()

	window, cached := o.cached[key]
	if !cached {
		state, err := o.locker.GetState(ctx, quota.Namespace, quota.Name)
		if err != nil {
			return sizing.Window{}, err
		}
		window = sizing.DecodeWindow(state.Window)
	}

	now := o.now()
	changed := window.Observe(now, string(quota.UID), quota.Status.Used, windowDays)

	if changed || o.heartbeatElapsed(window, now) {
		window.LastWriteAt = now.UTC().Format(time.RFC3339)
		encoded, err := sizing.EncodeWindow(window)
		if err != nil {
			return sizing.Window{}, err
		}
		err = o.locker.MutateState(ctx, quota.Namespace, quota.Name, func(s *lock.State) {
			s.Window = encoded
		})
		if err != nil {
			return sizing.Window{}, err
		}
	}

	o.cached[key] = window
	return window, nil
}

// Forget drops the cached window for a quota, so the next Observe reloads it
// from the Lease. Used when the reconciler sees the quota disappear.
func (o *Observer) Forget(namespace, name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cached, namespace+"/"+name)
}

func (o *Observer) heartbeatElapsed(window sizing.Window, now time.Time) bool {
	if window.LastWriteAt == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, window.LastWriteAt)
	if err != nil {
		return true
	}
	return now.Sub(last) >= observationHeartbeat
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/... -run TestObserver -v`
Expected: PASS for all five observer tests.

If `TestObserver_SkipsWriteWhenNothingChanged` fails, check that
`window.Observe` returns `false` for a sample that neither raises a peak nor
starts a day — `prune` must not report a change when it removed nothing.

- [ ] **Step 5: Lint and commit**

```bash
make lint
git add internal/controller/observation.go internal/controller/observation_test.go
git commit -m "feat(controller): write-behind observer for the demand window

Every reconcile folds status.used into an in-memory window; the Lease is
written only when a peak rises, a day rolls over, or an hourly heartbeat
elapses. Keeping the sample timestamp in memory is what makes the gap
measurement meaningful — reading it back from a lagging Lease would make
every sample look like an hour-long outage."
```

---

### Task 8: Rewire the controller onto the sizing package

Removes the threshold path (spec 3) and reduces `analyzeEvents` to what it is
actually good at: turning `FailedCreate` events into per-resource deficits. The
target arithmetic moves into `sizing.Decide`.

This task changes behaviour only in ways the migration chain neutralises: with
no new annotations set, `HeadroomFor` returns the value derived from the old
`*-threshold` or `*-increment` annotation, so grow decisions land on the same
numbers as before.

**Files:**
- Modify: `internal/controller/resourcequota_controller.go`
- Modify: `internal/controller/resourcequota_utils.go` — delete
  `convertToReadableFormat` and `ResizerConfig`/`parseConfig`
- Modify: `internal/controller/resourcequota_controller_test.go`,
  `internal/controller/event_analysis_test.go`,
  `internal/controller/event_analysis_scaling_test.go`,
  `internal/controller/event_analysis_multiburst_test.go`,
  `internal/controller/smart_calculation_test.go`
- Modify: `cmd/main.go`

**Interfaces:**
- Consumes: `sizing.Decide`, `sizing.Input`, `sizing.ParsePolicy`,
  `sizing.DefaultPolicy` (Tasks 2, 5); `Observer.Observe` (Task 7);
  `lock.GetState` (Task 6).
- Produces:
  - `ResourceQuotaReconciler` gains fields `Observer *Observer` and
    `BasePolicy sizing.Policy`
  - `func (r *ResourceQuotaReconciler) collectDeficits(ctx context.Context,
    quota corev1.ResourceQuota, since time.Time) (map[corev1.ResourceName]int64, error)`

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/resourcequota_controller_test.go`:

```go
func TestReconcile_GrowUsesHeadroomFromLegacyThreshold(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-a",
			Annotations: map[string]string{
				"resizer.io/cpu-threshold": "80",
			},
		},
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "compute",
			Namespace: "team-a",
			UID:       types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(c)
	provider := &FakeGitProvider{CreatePRID: 5}

	reconciler := &ResourceQuotaReconciler{
		Client:     c,
		Scheme:     scheme,
		Recorder:   record.NewFakeRecorder(10),
		GitProvider: provider,
		Locker:     locker,
		Observer:   NewObserver(locker, time.Now),
		BasePolicy: sizing.DefaultPolicy(),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	// used == hard, so target = 10 * 1.25 = 12.5, well above the band.
	g.Expect(provider.CreatePRCalls).To(Equal(1))
	g.Expect(provider.LastLimits).To(HaveKey(corev1.ResourceRequestsCPU))
	g.Expect(provider.LastDirection).To(Equal("grow"))
}

func TestReconcile_QuietInsideToleranceBand(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "compute", Namespace: "team-a", UID: types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(c)
	provider := &FakeGitProvider{}

	reconciler := &ResourceQuotaReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		GitProvider: provider, Locker: locker,
		Observer:   NewObserver(locker, time.Now),
		BasePolicy: sizing.DefaultPolicy(),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	// target = 8 * 1.25 = 10, exactly the current hard value: no action.
	g.Expect(provider.CreatePRCalls).To(Equal(0))
}
```

The test references `FakeGitProvider.LastLimits` and `LastDirection`. Add both
in this step, since the fake is test-only support code:

```go
	// LastLimits records the limits passed to the most recent CreatePR or
	// UpdatePR call.
	LastLimits map[corev1.ResourceName]resource.Quantity
	// LastDirection records the direction passed to the most recent CreatePR.
	LastDirection string
```

Assign them in `CreatePR` (`f.LastLimits = newLimits`) and `UpdatePR`. The
`direction` parameter arrives in Task 10; until then set
`f.LastDirection = "grow"` unconditionally in `CreatePR` and add a `//
Direction becomes a real parameter in the shrink-PR task.` comment.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/... -run TestReconcile_ -v`
Expected: build failure — `unknown field Observer in struct literal`.

- [ ] **Step 3: Replace the recommendation engine**

In `internal/controller/resourcequota_controller.go`:

Add the two reconciler fields:

```go
type ResourceQuotaReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	GitProvider     git.Provider
	Locker          *lock.LeaseLocker
	Observer        *Observer
	BasePolicy      sizing.Policy
	EnableAutoMerge bool
}
```

Replace the body of `Reconcile` between fetching the namespace and the GitOps
section:

```go
	policy, warnings := sizing.ParsePolicy(ns.Annotations, r.BasePolicy)
	for _, warning := range warnings {
		logger.V(1).Info("deprecated annotation in use", "detail", warning)
	}
	if !policy.Enabled {
		logger.V(1).Info("Namespace is opted out", "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	window, err := r.Observer.Observe(ctx, &quota, policy.WindowDays)
	if err != nil {
		logger.Error(err, "failed to record observation")
		return ctrl.Result{}, err
	}

	state, err := r.Locker.GetState(ctx, req.Namespace, quota.Name)
	if err != nil {
		logger.Error(err, "failed to read lease state")
		return ctrl.Result{}, err
	}

	deficits, err := r.collectDeficits(ctx, quota, state.LastModified)
	deficitScanFailed := err != nil
	if err != nil {
		// A failed event scan must not stop the metric-driven path; it only
		// means a pending shortage may be reacted to one reconcile later.
		//
		// It is not symmetric, though: a missing deficit can only lower the
		// target, so a failed scan could tip a quota from "no action" into
		// "shrink". The shrink branch below therefore requires a successful
		// scan.
		logger.Error(err, "failed to collect event deficits")
	}

	decision := sizing.Decide(sizing.Input{
		Now:        time.Now(),
		Hard:       quota.Status.Hard,
		Used:       quota.Status.Used,
		Deficits:   deficits,
		Window:     window,
		Policy:     policy,
		LastGrow:   state.LastGrow,
		LastShrink: state.LastShrink,
	})

	// The metrics task adds a recordDecision call right here.
	logger.V(1).Info("Sizing decision",
		"direction", decision.Direction.String(),
		"targets", decision.Targets,
		"blockedBy", decision.BlockedBy)

	if state.PRID != 0 {
		return r.handleActivePR(ctx, req, quota, ns, state, decision)
	}

	if decision.Direction == sizing.DirectionGrow {
		return r.handleNewProposal(ctx, req, quota, ns, policy, state, decision)
	}

	if decision.Direction == sizing.DirectionShrink {
		// Shrink PRs are wired up in the shrink-PR task. Until then the
		// decision is observable through metrics only. deficitScanFailed is
		// already checked here so the guard exists before the shrink branch
		// becomes actionable.
		if deficitScanFailed {
			logger.Info("Shrink suppressed: the event scan failed, so the " +
				"target may be understated")
		} else {
			logger.Info("Shrink recommended but not yet actionable",
				"targets", decision.Targets)
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}
```

Delete `calculateRecommendations` entirely.

- [ ] **Step 4: Reduce analyzeEvents to deficit collection**

Rename `analyzeEvents` to `collectDeficits` and cut everything after the
deficit aggregation. The resource-name resolution stays — deficits must be
keyed by the quota's own keys:

```go
// collectDeficits scans recent FailedCreate events and returns, per quota key,
// the additional milli-value that the blocked workloads asked for. Events older
// than the last successful change are skipped so a single shortage cannot be
// counted twice.
func (r *ResourceQuotaReconciler) collectDeficits(
	ctx context.Context,
	quota corev1.ResourceQuota,
	since time.Time,
) (map[corev1.ResourceName]int64, error) {
	logger := log.FromContext(ctx)

	var eventList corev1.EventList
	if err := r.List(ctx, &eventList, client.InNamespace(quota.Namespace)); err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-1 * time.Hour)

	// Keyed by resource, then by workload: the maximum per workload, summed
	// across workloads. Retries of one workload must not accumulate.
	perWorkload := make(map[corev1.ResourceName]map[string]int64)

	for _, evt := range eventList.Items {
		if evt.LastTimestamp.Time.Before(cutoff) {
			continue
		}
		if !since.IsZero() && evt.LastTimestamp.Time.Before(since) {
			continue
		}
		if evt.Type != corev1.EventTypeWarning || evt.Reason != reasonFailedCreate {
			continue
		}
		if !strings.Contains(evt.Message, "exceeded quota") ||
			!strings.Contains(evt.Message, quota.Name) {
			continue
		}

		resName, reqQty, err := sizing.ParseEventMessage(evt.Message)
		if err != nil {
			logger.Error(err, "Failed to parse event message", "message", evt.Message)
			continue
		}
		if !r.isObjectAlive(ctx, evt.InvolvedObject, quota.Namespace) {
			continue
		}

		key, workloadDeficits := r.calculateWorkloadDeficit(ctx, evt, resName, reqQty)
		for rName, value := range workloadDeficits {
			if _, ok := perWorkload[rName]; !ok {
				perWorkload[rName] = make(map[string]int64)
			}
			if value > perWorkload[rName][key] {
				perWorkload[rName][key] = value
			}
		}
	}

	deficits := make(map[corev1.ResourceName]int64, len(perWorkload))
	for resName, workloads := range perWorkload {
		var total int64
		for _, value := range workloads {
			total += value
		}
		quotaKey, ok := resolveQuotaKey(quota, resName)
		if !ok {
			continue
		}
		deficits[quotaKey] += total
	}

	return deficits, nil
}

// resolveQuotaKey maps a resource name derived from an event onto the key the
// quota actually uses, trying the short and the long form in turn.
func resolveQuotaKey(
	quota corev1.ResourceQuota,
	res corev1.ResourceName,
) (corev1.ResourceName, bool) {
	if _, ok := quota.Status.Hard[res]; ok {
		return res, true
	}

	candidates := map[corev1.ResourceName]corev1.ResourceName{
		corev1.ResourceCPU:             corev1.ResourceRequestsCPU,
		corev1.ResourceMemory:          corev1.ResourceRequestsMemory,
		corev1.ResourceStorage:         corev1.ResourceRequestsStorage,
		corev1.ResourceRequestsCPU:     corev1.ResourceCPU,
		corev1.ResourceRequestsMemory:  corev1.ResourceMemory,
		corev1.ResourceRequestsStorage: corev1.ResourceStorage,
	}
	if alt, ok := candidates[res]; ok {
		if _, ok := quota.Status.Hard[alt]; ok {
			return alt, true
		}
	}
	return "", false
}
```

- [ ] **Step 5: Update the two PR handlers**

`handleActivePR` and `handleNewProposal` change signature: they take
`state lock.State` and `decision sizing.Decision` instead of
`prID int`, `recommendations map[...]`, `needsResize bool`, `config ResizerConfig`.

Inside them, replace:
- `prID` with `state.PRID`
- `needsResize` with `decision.Direction == sizing.DirectionGrow`
- `recommendations` with `decision.Targets`
- `config.Cooldown` with `policy.GrowCooldown`
- `r.Locker.GetLastModified(...)` with `state.LastModified`

The `UpdatePR` guard is `== DirectionGrow`, not `!= DirectionNone`. Anything
looser lets a shrink decision rewrite an open grow pull request with lowered
limits, which the auto-merge block directly above would then merge — breaking
the rule that a shrink is never auto-merged, before shrink pull requests are
even supposed to exist. The shrink-PR task widens this deliberately, together
with the direction state that makes it safe.

In `handleNewProposal`, after a successful `CreatePR`, replace the
`AcquireLock` call so the grow timestamp is recorded in the same write:

```go
	err = r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
		s.PRID = newPRID
		s.PRDirection = sizing.DirectionGrow.String()
		s.LastGrow = time.Now()
	})
	if err != nil {
		logger.Error(err, "failed to record the new pull request")
		return ctrl.Result{}, err
	}
```

- [ ] **Step 6: Delete the dead code**

From `internal/controller/resourcequota_utils.go` remove `ResizerConfig`,
`GetThreshold`, `GetIncrement`, `parseConfig` and `convertToReadableFormat`.
The only remaining functions there are `mapEventToQuota`,
`calculateWorkloadDeficit` and `isObjectAlive`.

Delete `internal/controller/utils_test.go` in the same step: it contains only
`TestConvertToReadableFormat`, and leaving it would fail to compile. Its
subject matter is already covered by `TestQuantize` in `internal/sizing`,
including the object-count cases the old function got wrong.

Update the tests that referenced the removed helpers: in
`smart_calculation_test.go` and the three `event_analysis_*_test.go` files,
replace `parseConfig(...)` with `sizing.ParsePolicy(annotations,
sizing.DefaultPolicy())` and assertions on `analyzeEvents` recommendations with
assertions on `collectDeficits` milli-values. The deficit numbers themselves do
not change — only the assertion target moves from a computed limit to the raw
deficit.

- [ ] **Step 7: Wire main.go**

In `cmd/main.go`, build the base policy and the observer:

```go
	basePolicy := sizing.DefaultPolicy()
	// Shrinking stays off until the rollout flag lands; the decision is still
	// computed so the dry-run metrics show what would happen.
	basePolicy.ShrinkEnabled = false

	observer := controller.NewObserver(locker, time.Now)
```

and pass both into the reconciler literal:

```go
		Observer:   observer,
		BasePolicy: basePolicy,
```

- [ ] **Step 8: Run the full suite**

Run: `make test`
Expected: PASS. `TestReconcile_GrowUsesHeadroomFromLegacyThreshold` proves the
migration keeps existing installations on their current numbers.

- [ ] **Step 9: Lint and commit**

```bash
make lint
git rm internal/controller/utils_test.go
git add internal/controller cmd/main.go
git commit -m "refactor(controller): drive resizing from the sizing package

The 80% threshold rule is replaced by a single target formula evaluated in
sizing.Decide. Event analysis is reduced to producing per-resource deficits;
all target arithmetic now lives in one place. Installations that only set the
legacy threshold or increment annotations keep their current grow behaviour
through the migration chain."
```

---

### Task 9: Dry-run metrics

Implements spec 8. This is what makes the flag-off rollout useful: the shrink
recommendation is visible for weeks before the first PR can exist.

**Files:**
- Create: `internal/controller/metrics.go`
- Test: `internal/controller/metrics_test.go`
- Modify: `go.mod` — promote `github.com/prometheus/client_golang` to a direct
  dependency

**Interfaces:**
- Consumes: `sizing.Decision`, `sizing.Gate`, `sizing.Direction` (Task 5).
- Produces:
  - `func recordDecision(namespace, quota string, hard corev1.ResourceList, d sizing.Decision)`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/metrics_test.go`:

```go
package controller

import (
	"strings"
	"testing"

	"github.com/payback159/namespace-resizer/internal/sizing"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRecordDecision_ExposesWasteRatio(t *testing.T) {
	quotaTarget.Reset()
	quotaWasteRatio.Reset()
	shrinkBlockedBy.Reset()

	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}
	decision := sizing.Decision{
		Direction: sizing.DirectionNone,
		ShrinkPreview: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("4"),
		},
		BlockedBy: []sizing.Gate{sizing.GateEnabled},
	}

	recordDecision("team-a", "compute", hard, decision)

	expected := `
# HELP resizer_quota_waste_ratio Current hard limit divided by the computed target.
# TYPE resizer_quota_waste_ratio gauge
resizer_quota_waste_ratio{namespace="team-a",quota="compute",resource="requests.cpu"} 4
`
	err := testutil.CollectAndCompare(
		quotaWasteRatio, strings.NewReader(expected), "resizer_quota_waste_ratio")
	if err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(shrinkBlockedBy.WithLabelValues(
		"team-a", "compute", "enabled")); got != 1 {
		t.Fatalf("shrinkBlockedBy{gate=enabled} = %v, want 1", got)
	}
}

func TestRecordDecision_ClearsStaleGates(t *testing.T) {
	shrinkBlockedBy.Reset()
	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}

	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionNone,
		BlockedBy: []sizing.Gate{sizing.GateCooldown},
	})

	// Assert the gate went up first. Without this, a recordDecision that did
	// nothing at all would satisfy the assertion below, because reading an
	// unset gauge yields zero.
	blocked := testutil.ToFloat64(shrinkBlockedBy.WithLabelValues(
		"team-a", "compute", "cooldown"))
	if blocked != 1 {
		t.Fatalf("gate = %v after a blocked decision, want 1", blocked)
	}

	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionGrow,
		Targets: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("20"),
		},
	})

	got := testutil.ToFloat64(shrinkBlockedBy.WithLabelValues(
		"team-a", "compute", "cooldown"))
	if got != 0 {
		t.Fatalf("stale gate still set to %v, want 0", got)
	}
}

func TestRecordDecision_ClearsResolvedTargets(t *testing.T) {
	quotaTarget.Reset()
	quotaWasteRatio.Reset()

	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}

	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionNone,
		ShrinkPreview: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("4"),
		},
		BlockedBy: []sizing.Gate{sizing.GateEnabled},
	})

	if got := testutil.CollectAndCount(quotaWasteRatio); got != 1 {
		t.Fatalf("waste ratio series = %d, want 1 after a shrink preview", got)
	}

	// The quota now tracks demand: no target, so the series must disappear
	// rather than freeze at a waste ratio that is no longer true.
	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionNone,
	})

	if got := testutil.CollectAndCount(quotaWasteRatio); got != 0 {
		t.Fatalf("waste ratio series = %d, want 0 once the target is gone", got)
	}
	if got := testutil.CollectAndCount(quotaTarget); got != 0 {
		t.Fatalf("target series = %d, want 0 once the target is gone", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/... -run TestRecordDecision -v`
Expected: build failure — `undefined: quotaTarget`.

- [ ] **Step 3: Add the dependency**

```bash
go get github.com/prometheus/client_golang@latest
go mod tidy
```

- [ ] **Step 4: Write minimal implementation**

Create `internal/controller/metrics.go`:

```go
package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/payback159/namespace-resizer/internal/sizing"
)

var quotaLabels = []string{"namespace", "quota", "resource"}

var (
	quotaTarget = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "resizer_quota_target",
		Help: "Computed target for a quota resource, in milli-units.",
	}, quotaLabels)

	quotaWasteRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "resizer_quota_waste_ratio",
		Help: "Current hard limit divided by the computed target.",
	}, quotaLabels)

	shrinkBlockedBy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "resizer_shrink_blocked_by",
		Help: "1 while the named gate blocks a pending shrink, 0 otherwise.",
	}, []string{"namespace", "quota", "gate"})

	decisionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "resizer_decision_total",
		Help: "Decisions taken, by direction.",
	}, []string{"namespace", "quota", "direction"})
)

// allGates is iterated on every record so a gate that stopped blocking is
// actively reset to 0 instead of keeping its last value forever.
var allGates = []sizing.Gate{
	sizing.GateEnabled,
	sizing.GateWindow,
	sizing.GateRecentGrow,
	sizing.GateCooldown,
}

func init() {
	metrics.Registry.MustRegister(
		quotaTarget, quotaWasteRatio, shrinkBlockedBy, decisionTotal)
}

// recordDecision publishes one evaluation. It reports the shrink preview as
// well as the acted-on targets, so the waste is visible while the shrink path
// is still switched off.
func recordDecision(
	namespace, quota string,
	hard corev1.ResourceList,
	decision sizing.Decision,
) {
	decisionTotal.WithLabelValues(namespace, quota, decision.Direction.String()).Inc()

	targets := decision.Targets
	if len(targets) == 0 {
		targets = decision.ShrinkPreview
	}

	// Iterate the quota's own keys, not the targets, so a resource that no
	// longer needs one has its series removed instead of left behind. A gauge
	// frozen at its last value would keep reporting waste that has already
	// been resolved — the mirror image of the stale gate this function is
	// careful to avoid below.
	for res, current := range hard {
		labels := []string{namespace, quota, string(res)}

		target, wanted := targets[res]
		targetMilli := target.MilliValue()
		if !wanted || targetMilli == 0 {
			quotaTarget.DeleteLabelValues(labels...)
			quotaWasteRatio.DeleteLabelValues(labels...)
			continue
		}

		quotaTarget.WithLabelValues(labels...).Set(float64(targetMilli))
		quotaWasteRatio.WithLabelValues(labels...).
			Set(float64(current.MilliValue()) / float64(targetMilli))
	}

	blocked := make(map[sizing.Gate]bool, len(decision.BlockedBy))
	for _, gate := range decision.BlockedBy {
		blocked[gate] = true
	}
	for _, gate := range allGates {
		value := 0.0
		if blocked[gate] {
			value = 1.0
		}
		shrinkBlockedBy.WithLabelValues(namespace, quota, string(gate)).Set(value)
	}
}
```

- [ ] **Step 5: Wire the call site**

In `internal/controller/resourcequota_controller.go`, replace the placeholder
log line left by the controller-rewiring task:

```go
	// The metrics task adds a recordDecision call right here.
	logger.V(1).Info("Sizing decision",
		"direction", decision.Direction.String(),
		"targets", decision.Targets,
		"blockedBy", decision.BlockedBy)
```

with:

```go
	recordDecision(req.Namespace, quota.Name, quota.Status.Hard, decision)
	logger.V(1).Info("Sizing decision",
		"direction", decision.Direction.String(),
		"targets", decision.Targets,
		"blockedBy", decision.BlockedBy)
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/controller/... -run TestRecordDecision -v`
Expected: PASS. `waste_ratio` is `16 / 4 = 4`.

- [ ] **Step 7: Enable the metrics endpoint in the sample deployment**

`config/default/kustomization.yaml` already ships the metrics service. Confirm
`--metrics-bind-address` is not left at `0` in
`config/manager/manager.yaml`; if it is, document in `docs/OPERATIONS.md` that
the dry-run requires setting it to `:8443`. Do not change the default here —
that is a deployment decision, not a code one.

- [ ] **Step 8: Lint and commit**

```bash
make lint
git add internal/controller/metrics.go internal/controller/metrics_test.go \
        internal/controller/resourcequota_controller.go go.mod go.sum
git commit -m "feat(controller): expose sizing decisions as metrics

Publishes the computed target, the resulting waste ratio and the gate that
currently blocks a shrink. The shrink preview is reported even when a gate
blocks it, which is what makes the flag-off rollout observable."
```

---

**Stage 1 is complete.** The controller now decides through `sizing.Decide`,
records the observation window, and reports what it would shrink — without any
shrink PR being possible. Verify before continuing:

```bash
make test && make lint
```

---

## Stage 2 — Shrink pull requests

---

### Task 10: Direction-aware Git provider

Implements spec 6.1. The label is not cosmetic: `FindOpenPR` recovers PRs that
were created but never locked. Without a direction it would adopt an orphaned
shrink PR as a grow and, with auto-merge on, merge it unreviewed.

**Files:**
- Modify: `internal/git/github.go`
- Modify: `internal/git/log_provider.go` — `LogOnlyProvider` and
  `StatefulLogProvider` implement the same interface and must follow
- Modify: `internal/git/github_prmgmt_test.go`
- Modify: `internal/git/log_provider_test.go`
- Modify: `internal/controller/fake_git_provider.go`
- Modify: every call site of `CreatePR`/`FindOpenPR` in
  `internal/controller/resourcequota_controller.go`

There are **four** implementations of `git.Provider`: `GitHubProvider`,
`LogOnlyProvider`, `StatefulLogProvider` and the test-only
`FakeGitProvider`. Changing the interface breaks all four; the compiler will
name each one.

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `const DirectionGrow = "grow"`, `const DirectionShrink = "shrink"` in `git`
  - `CreatePR(ctx, quotaName, namespace, direction string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error)`
  - `FindOpenPR(ctx, namespace, quotaName string) (int, string, error)`
  - `ClosePR(ctx context.Context, prID int, comment string) error`

- [ ] **Step 1: Write the failing test**

Add to `internal/git/github_prmgmt_test.go`, following the existing
`httptest`-based pattern in that file:

```go
func TestFindOpenPR_ReturnsDirectionFromLabel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{
			"number": 42,
			"head": {"ref": "resize/team-a-compute-1700000000"},
			"labels": [
				{"name": "resizer/managed"},
				{"name": "resizer/direction:shrink"}
			]
		}]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	id, direction, err := provider.FindOpenPR(context.Background(), "team-a", "compute")

	if err != nil {
		t.Fatalf("FindOpenPR: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}
	if direction != DirectionShrink {
		t.Errorf("direction = %q, want %q", direction, DirectionShrink)
	}
}

func TestFindOpenPR_DefaultsToGrowWithoutLabel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{
			"number": 7,
			"head": {"ref": "resize/team-a-compute-1700000000"},
			"labels": [{"name": "resizer/managed"}]
		}]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	_, direction, err := provider.FindOpenPR(context.Background(), "team-a", "compute")

	if err != nil {
		t.Fatalf("FindOpenPR: %v", err)
	}
	// PRs created before this feature carry no direction label. Treating them
	// as grow keeps the old behaviour for them and never enables an
	// unreviewed shrink merge.
	if direction != DirectionGrow {
		t.Errorf("direction = %q, want %q", direction, DirectionGrow)
	}
}

func TestClosePR_CommentsThenCloses(t *testing.T) {
	var commented, closed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/42/comments",
		func(w http.ResponseWriter, r *http.Request) {
			commented = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id": 1}`)
		})
	mux.HandleFunc("/repos/o/r/pulls/42",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				closed = true
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"number": 42, "state": "closed"}`)
		})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	err := provider.ClosePR(context.Background(), 42, "superseded by a shortage")

	if err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	if !commented {
		t.Error("no comment was posted; the reason must survive in the PR")
	}
	if !closed {
		t.Error("PR was not closed")
	}
}
```

If `newTestProvider` does not already exist in that file, add it:

```go
// newTestProvider points a GitHubProvider at a local test server.
func newTestProvider(t *testing.T, handler http.Handler) (*GitHubProvider, func()) {
	t.Helper()
	server := httptest.NewServer(handler)

	client := github.NewClient(server.Client())
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client.BaseURL = baseURL

	provider := &GitHubProvider{
		client:       client,
		owner:        "o",
		repo:         "r",
		clusterName:  "test",
		pathTemplate: template.Must(template.New("path").Parse("managed/{{ .Namespace }}")),
	}
	return provider, server.Close
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/git/... -run 'TestFindOpenPR|TestClosePR' -v`
Expected: compile error — `provider.ClosePR undefined`, and
`FindOpenPR` returning two values instead of three.

- [ ] **Step 3: Extend the provider**

In `internal/git/github.go`, add the constants next to the existing ones:

```go
// Pull request directions. They are persisted as a GitHub label so an
// orphaned PR can be classified without any local state.
const (
	DirectionGrow   = "grow"
	DirectionShrink = "shrink"

	labelManaged        = "resizer/managed"
	labelDirectionPrefix = "resizer/direction:"
)
```

Update the `Provider` interface:

```go
type Provider interface {
	GetPRStatus(ctx context.Context, prID int) (*PRStatus, error)
	MergePR(ctx context.Context, prID int, method string) error
	CreatePR(ctx context.Context, quotaName, namespace, direction string,
		annotations map[string]string,
		newLimits map[corev1.ResourceName]resource.Quantity) (int, error)
	UpdatePR(ctx context.Context, prID int, quotaName, namespace string,
		annotations map[string]string,
		newLimits map[corev1.ResourceName]resource.Quantity) error
	// FindOpenPR returns the number and the direction of an existing open PR
	// managed by the resizer, or 0 and an empty direction if none exists.
	FindOpenPR(ctx context.Context, namespace, quotaName string) (int, string, error)
	// ClosePR posts comment on the pull request and then closes it without
	// merging.
	ClosePR(ctx context.Context, prID int, comment string) error
}
```

In `CreatePR`, take the direction, use it in the title and add it as a label:

```go
	title := fmt.Sprintf("Resize Quota %s in %s", quotaName, namespace)
	if direction == DirectionShrink {
		title = fmt.Sprintf("Shrink Quota %s in %s", quotaName, namespace)
	}
```

```go
	labels := []string{
		labelManaged,
		fmt.Sprintf("resizer/ns:%s", namespace),
		labelDirectionPrefix + direction,
	}
	if err := g.addLabels(ctx, pr.GetNumber(), labels); err != nil {
		logger := log.FromContext(ctx)
		if direction != DirectionShrink {
			// A grow pull request with no label is recovered as grow anyway,
			// so the only cost is a less precise audit trail.
			logger.Error(err, "failed to label pull request",
				"pr", pr.GetNumber(), "direction", direction)
			return pr.GetNumber(), nil
		}
		// An unlabelled shrink is indistinguishable from a grow once this
		// process forgets it, and grow pull requests are the ones eligible
		// for auto-merge. Take the pull request back rather than leave a
		// mergeable shrink behind.
		logger.Error(err, "failed to label shrink pull request, closing it again",
			"pr", pr.GetNumber())
		if closeErr := g.ClosePR(ctx, pr.GetNumber(), unlabelledShrinkComment); closeErr != nil {
			return 0, fmt.Errorf(
				"failed to label shrink PR %d (%w) and failed to close it again: %w",
				pr.GetNumber(), err, closeErr)
		}
		return 0, fmt.Errorf("failed to label shrink PR %d, closed it again: %w",
			pr.GetNumber(), err)
	}
```

```go
const unlabelledShrinkComment = "Closing this pull request: its direction " +
	"label could not be attached, and an unlabelled shrink proposal would " +
	"later be mistaken for a growth proposal. A replacement will be opened " +
	"on the next reconcile."
```

The retry exists because GitHub occasionally answers with a 5xx while a
freshly created pull request is still materialising as an issue. Closing a PR
we cannot label is a real cost, so it should not happen for a blip. The
backoff is a variable purely so tests need not sleep through it:

```go
// labelAttempts is how often CreatePR tries to attach the labels before it
// treats the failure as final.
const labelAttempts = 3

// labelRetryBackoff is a variable so tests can zero it.
var labelRetryBackoff = 500 * time.Millisecond

func (g *GitHubProvider) addLabels(ctx context.Context, prNumber int, labels []string) error {
	var err error
	for attempt := 1; attempt <= labelAttempts; attempt++ {
		if _, _, err = g.client.Issues.AddLabelsToIssue(
			ctx, g.owner, g.repo, prNumber, labels); err == nil {
			return nil
		}
		if attempt == labelAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("labelling PR %d cancelled: %w", prNumber, ctx.Err())
		case <-time.After(labelRetryBackoff):
		}
	}
	return fmt.Errorf("failed to label PR %d after %d attempts: %w",
		prNumber, labelAttempts, err)
}
```

`internal/git/github.go` has no logger today — the `fmt.Printf` this replaces
is the only diagnostic in the whole file. Import
`"sigs.k8s.io/controller-runtime/pkg/log"`, which `log_provider.go` in the
same package already uses, and take the logger from the context as that file
does.

Extend `FindOpenPR` to read the label:

```go
		for _, pr := range prs {
			if pr.Head == nil || !strings.HasPrefix(pr.Head.GetRef(), prefix) {
				continue
			}
			return pr.GetNumber(), directionFromLabels(pr.Labels), nil
		}
```

```go
// directionFromLabels reads the direction label and never invents a value it
// did not recognise.
//
// No direction label at all means the pull request predates the label;
// classifying it as grow preserves the behaviour those pull requests were
// opened under. A label that is present but is not exactly "grow" is a
// different case: labels are writable by anyone with repository access, so an
// unrecognised value is evidence that something other than this controller
// wrote it. Reading it as shrink is the safe direction — shrink proposals are
// never auto-merged, so the cost of being wrong is one human review, whereas
// reading it as grow would cost an unreviewed merge of lowered limits.
func directionFromLabels(labels []*github.Label) string {
	for _, label := range labels {
		name := label.GetName()
		if !strings.HasPrefix(name, labelDirectionPrefix) {
			continue
		}
		if strings.TrimPrefix(name, labelDirectionPrefix) == DirectionGrow {
			return DirectionGrow
		}
		return DirectionShrink
	}
	return DirectionGrow
}
```

Add `ClosePR`:

```go
// ClosePR records why the pull request is being abandoned and then closes it.
// The comment is posted first: if closing fails the reason is still visible,
// whereas a closed PR with no explanation is confusing for reviewers.
func (g *GitHubProvider) ClosePR(ctx context.Context, prID int, comment string) error {
	body := &github.IssueComment{Body: github.Ptr(comment)}
	if _, _, err := g.client.Issues.CreateComment(
		ctx, g.owner, g.repo, prID, body); err != nil {
		return fmt.Errorf("failed to comment on PR %d: %w", prID, err)
	}

	update := &github.PullRequest{State: github.Ptr("closed")}
	if _, _, err := g.client.PullRequests.Edit(
		ctx, g.owner, g.repo, prID, update); err != nil {
		return fmt.Errorf("failed to close PR %d: %w", prID, err)
	}
	return nil
}
```

- [ ] **Step 4: Extend the fake provider**

In `internal/controller/fake_git_provider.go`:

```go
	// ExistingPRDirection is returned alongside ExistingPR by FindOpenPR.
	ExistingPRDirection string
	// ClosedPRID and ClosedComment record the most recent ClosePR call.
	ClosedPRID  int
	ClosedComment string
	// ClosePRCalls counts ClosePR invocations.
	ClosePRCalls int
```

```go
func (f *FakeGitProvider) CreatePR(
	ctx context.Context,
	quotaName, namespace, direction string,
	annotations map[string]string,
	newLimits map[corev1.ResourceName]resource.Quantity,
) (int, error) {
	f.CreatePRCalls++
	f.LastLimits = newLimits
	f.LastDirection = direction
	if f.CreatePRID != 0 {
		return f.CreatePRID, nil
	}
	return 1, nil
}

func (f *FakeGitProvider) FindOpenPR(
	ctx context.Context,
	namespace, quotaName string,
) (int, string, error) {
	f.FindOpenPRCalls++
	direction := f.ExistingPRDirection
	if f.ExistingPR != 0 && direction == "" {
		direction = git.DirectionGrow
	}
	return f.ExistingPR, direction, nil
}

func (f *FakeGitProvider) ClosePR(ctx context.Context, prID int, comment string) error {
	f.ClosePRCalls++
	f.ClosedPRID = prID
	f.ClosedComment = comment
	return nil
}
```

Remove the placeholder `f.LastDirection = "grow"` assignment introduced in
Task 8 — the direction is a real parameter now.

- [ ] **Step 5: Update the log-only providers**

`internal/git/log_provider.go` holds two more implementations. Both are used
in observer mode and must keep compiling. For each of `LogOnlyProvider` and
`StatefulLogProvider`:

```go
func (p *LogOnlyProvider) CreatePR(
	ctx context.Context,
	quotaName, namespace, direction string,
	annotations map[string]string,
	newLimits map[corev1.ResourceName]resource.Quantity,
) (int, error) {
	log.FromContext(ctx).Info("Would create pull request",
		"namespace", namespace, "quota", quotaName,
		"direction", direction, "limits", newLimits)
	return 0, nil
}

func (p *LogOnlyProvider) FindOpenPR(
	ctx context.Context,
	namespace, quotaName string,
) (int, string, error) {
	return 0, "", nil
}

func (p *LogOnlyProvider) ClosePR(ctx context.Context, prID int, comment string) error {
	log.FromContext(ctx).Info("Would close pull request",
		"prID", prID, "comment", comment)
	return nil
}
```

`StatefulLogProvider` tracks PRs in memory, so it stores and returns the
direction. Add a `Direction string` field to its `PRDetails` struct, set it in
`CreatePR`, return it from `FindOpenPR`, and have `ClosePR` mark the entry
closed the same way `MergePR` already does — read that method and mirror it,
minus the merged flag. Update the affected cases in
`internal/git/log_provider_test.go`.

- [ ] **Step 6: Update the controller call sites**

In `handleNewProposal`, pass the direction and unpack three return values:

```go
	existingPRID, existingDirection, err := r.GitProvider.FindOpenPR(
		ctx, req.Namespace, quota.Name)
```

```go
	newPRID, err := r.GitProvider.CreatePR(
		ctx, quota.Name, req.Namespace, decision.Direction.String(),
		ns.Annotations, decision.Targets)
```

When adopting an orphaned PR, persist the direction it reported:

```go
		err = r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
			s.PRID = existingPRID
			s.PRDirection = existingDirection
		})
```

After creating a PR, persist the direction that was actually passed to
`CreatePR` rather than the constant `grow` Task 8 left there, and stamp the
matching timestamp:

```go
	err = r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
		s.PRID = newPRID
		s.PRDirection = decision.Direction.String()
		if decision.Direction == sizing.DirectionShrink {
			s.LastShrink = time.Now()
		} else {
			s.LastGrow = time.Now()
		}
	})
```

This is behaviour-neutral today — `handleNewProposal` is still grow-only — but
it removes the divergence where the label on the pull request and the direction
in the lease are written from two different sources fourteen lines apart. The
lease is what the auto-merge gate in Task 11 reads.

- [ ] **Step 7: Cover the write half of the safety boundary**

Steps 1–6 test that a direction can be read back. Nothing yet asserts that the
right direction is ever written. Add:

`internal/git/github_prmgmt_test.go`

- `TestCreatePR_AttachesDirectionLabel` — a shrink creation whose label
  handler captures the request body and asserts it contains
  `resizer/direction:shrink`, plus a grow case asserting
  `resizer/direction:grow`. The existing `TestCreatePR` label handler asserts
  nothing; this is the assertion it is missing.
- `TestCreatePR_ShrinkClosesItselfWhenLabellingFails` — label endpoint always
  answers 500, close endpoint records the call. Set `labelRetryBackoff = 0`
  for the test (restore it with `t.Cleanup`). Assert: `labelAttempts` label
  requests were made, the PR was commented on and closed, and `CreatePR`
  returned an error with PR id 0.
- `TestCreatePR_GrowSurvivesLabellingFailure` — same 500, direction grow.
  Assert: no close call, `CreatePR` returns the PR number and no error.
- `TestFindOpenPR_UnknownDirectionReadsAsShrink` — label
  `resizer/direction:Shrink` (wrong case) and a second case with
  `resizer/direction:banana`; both must yield `git.DirectionShrink`.

`internal/controller/` — the adoption test added in Step 6 asserts only the PR
id. Extend it to assert the persisted `PRDirection` equals the direction the
fake reported, using `FakeGitProvider.ExistingPRDirection`. Add a case where
the fake reports `git.DirectionShrink` and assert the lease records shrink,
not grow — without it, the hardcoded-grow defect this task removes would still
pass every test.

`FakeGitProvider.ClosedPRID`, `ClosedComment` and `ClosePRCalls` are added in
Step 4; if no test in this task reads them, they are dead fields — the shrink
tests above are their first consumer at the `git` level, and Task 12 is the
first at the controller level.

- [ ] **Step 8: Run the full suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 9: Lint and commit**

```bash
make lint
git add internal/git internal/controller
git commit -m "feat(git): direction-aware pull requests

CreatePR records grow or shrink as a label, FindOpenPR reads it back, and
ClosePR abandons a pull request with an explanatory comment. Orphan recovery
can now tell the two kinds apart; a PR without the label is treated as a grow,
which preserves the previous behaviour for anything already open."
```

---

### Task 11: Direction state and auto-merge restriction

Implements spec 6.1. One rule: auto-merge only ever applies to a grow.

**Files:**
- Modify: `internal/controller/resourcequota_controller.go`
- Modify: `internal/controller/automerge_test.go`

**Interfaces:**
- Consumes: `lock.State.PRDirection` (Task 6), `git.DirectionShrink` (Task 10).
- Produces: no new exported symbols.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/automerge_test.go`, mirroring the setup of the
existing `TestAutoMerge`:

```go
func TestAutoMerge_NeverMergesAShrink(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "compute", Namespace: "team-a", UID: types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("16"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("4"),
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(c)

	g.Expect(locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	provider := &FakeGitProvider{PRStatus: &git.PRStatus{
		IsOpen:         true,
		Mergeable:      true,
		MergeableState: git.MergeableStateClean,
		ChecksState:    git.ChecksStateSuccess,
	}}

	reconciler := &ResourceQuotaReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		GitProvider: provider, Locker: locker,
		Observer:        NewObserver(locker, time.Now),
		BasePolicy:      sizing.DefaultPolicy(),
		EnableAutoMerge: true,
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(provider.MergedPRID).To(Equal(0),
		"a shrink PR must never be auto-merged, however clean it looks")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/... -run TestAutoMerge -v`
Expected: FAIL — `MergedPRID` is 42, because the current code merges any clean
PR.

- [ ] **Step 3: Restrict auto-merge**

In `handleActivePR`, gate the whole auto-merge block on the direction:

```go
	// Shrink pull requests are never auto-merged, regardless of the global
	// flag or the namespace annotation: reclaiming quota is the owning team's
	// decision, not the controller's.
	shouldAutoMerge := r.EnableAutoMerge && state.PRDirection != git.DirectionShrink
	if val, ok := ns.Annotations[resizerConfig.AnnotationAutoMerge]; ok && val == "false" {
		shouldAutoMerge = false
	}
```

In the merge-success branch, record the grow timestamp through `MutateState`
instead of `ReleaseLockWithTimestamp`:

```go
			now := time.Now()
			err := r.Locker.MutateState(ctx, req.Namespace, quota.Name,
				func(s *lock.State) {
					s.PRID = 0
					s.PRDirection = ""
					s.LastModified = now
					s.LastGrow = now
				})
			if err != nil {
				logger.Error(err, "failed to release lock after merge")
				return ctrl.Result{}, err
			}
```

In the closed-or-merged branch at the top of `handleActivePR`, set the
direction-specific timestamp:

```go
		now := time.Now()
		err := r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
			wasShrink := s.PRDirection == git.DirectionShrink
			s.PRID = 0
			s.PRDirection = ""
			if !status.IsMerged {
				return
			}
			s.LastModified = now
			if wasShrink {
				s.LastShrink = now
			} else {
				s.LastGrow = now
			}
		})
		if err != nil {
			logger.Error(err, "failed to release lock")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
```

- [ ] **Step 4: Remove the superseded lock helpers**

`GetLock`, `UpdateLock`, `ReleaseLock`, `ReleaseLockWithTimestamp`,
`SetLastModified`, `GetLastModified` and `CheckCooldown` now have no callers
outside `internal/lock`. Delete the ones that are genuinely unused and keep
`AcquireLock` and `SetLastModified` — `AcquireLock` still carries the
"already held by a different PR" guard, and `SetLastModified` is used by
`internal/lock/state_test.go`.

Run `go build ./...` after each deletion; the compiler is the authority on
what is still referenced. Delete the corresponding cases from
`internal/lock/lease_test.go` and `internal/lock/lease_conflict_test.go` only
for methods you actually removed.

- [ ] **Step 5: Run the full suite**

Run: `make test`
Expected: PASS, including the pre-existing `TestAutoMerge`.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/controller internal/lock
git commit -m "feat(controller): never auto-merge a shrink

Auto-merge now applies to grow pull requests only. Releasing a lock records
the direction-specific timestamp, so the shrink cooldown and the recent-grow
gate observe the event that actually happened."
```

---

### Task 12: Shrink pull requests, supersede and TTL

Implements spec 6.2 and 6.3. Two failure modes are being designed out here: a
shrink PR that blocks an urgent grow, and a shrink PR that nobody reviews and
that therefore holds the lock forever.

**Files:**
- Modify: `internal/git/github.go` — `PRStatus.CreatedAt`
- Modify: `internal/controller/resourcequota_controller.go`
- Create: `internal/controller/shrink_test.go`

**Interfaces:**
- Consumes: `git.ClosePR` (Task 10), `sizing.DirectionShrink` (Task 5),
  `Policy.ShrinkPRTTL` (Task 2).
- Produces:
  - `PRStatus` gains `CreatedAt time.Time`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/shrink_test.go`:

```go
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type shrinkHarness struct {
	reconciler *ResourceQuotaReconciler
	provider   *FakeGitProvider
	locker     *lock.LeaseLocker
}

// shortageObjects returns the event and the owning ReplicaSet that make
// collectDeficits report a pending shortage of the given size. The ReplicaSet
// has to exist because the controller ignores events whose involved object is
// already gone, and it leaves Spec.Replicas nil so the deficit comes straight
// from the event message rather than from a replica calculation.
func shortageObjects(requestedCPU string) []client.Object {
	return []client.Object{
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "team-a"},
		},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "shortage", Namespace: "team-a"},
			Type:       corev1.EventTypeWarning,
			Reason:     "FailedCreate",
			Message: "exceeded quota: compute, requested: requests.cpu=" +
				requestedCPU + ", used: requests.cpu=4, limited: requests.cpu=16",
			InvolvedObject: corev1.ObjectReference{
				Kind:       "ReplicaSet",
				APIVersion: "apps/v1",
				Name:       "web-abc123",
				Namespace:  "team-a",
			},
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	}
}

// newShrinkHarness builds a reconciler whose quota is heavily oversized
// (hard 16, used 4) with shrinking enabled. Extra objects are seeded into the
// fake client, which is how a test stages a pending shortage.
func newShrinkHarness(
	t *testing.T,
	status *git.PRStatus,
	extra ...client.Object,
) *shrinkHarness {
	t.Helper()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "compute", Namespace: "team-a", UID: types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("16"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("4"),
			},
		},
	}

	objects := append([]client.Object{ns, quota}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	locker := lock.NewLeaseLocker(c)
	provider := &FakeGitProvider{PRStatus: status, CreatePRID: 43}

	policy := sizing.DefaultPolicy()
	policy.ShrinkEnabled = true

	return &shrinkHarness{
		reconciler: &ResourceQuotaReconciler{
			Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
			GitProvider: provider, Locker: locker,
			Observer:   NewObserver(locker, time.Now),
			BasePolicy: policy,
		},
		provider: provider,
		locker:   locker,
	}
}

func (h *shrinkHarness) reconcile(ctx context.Context) error {
	_, err := h.reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})
	return err
}

func TestShrink_SupersededByAShortage(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A pending shortage of 20 CPU turns the decision into a grow.
	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Now().Add(-3 * 24 * time.Hour),
	}, shortageObjects("20")...)

	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(1))
	g.Expect(h.provider.ClosedPRID).To(Equal(42))
	g.Expect(h.provider.ClosedComment).To(ContainSubstring("requests.cpu"))

	state, err := h.locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0), "the lock must be free for the grow PR")
	g.Expect(state.LastShrink.IsZero()).To(BeFalse(),
		"closing a shrink starts its cooldown, or it reopens immediately")
}

func TestShrink_ExpiresAfterTTL(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Now().Add(-8 * 24 * time.Hour),
	})
	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(1))
	g.Expect(h.provider.ClosedComment).To(ContainSubstring("without review"))

	state, err := h.locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.LastShrink.IsZero()).To(BeFalse())
}

func TestShrink_YoungPRIsLeftAlone(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Now().Add(-2 * 24 * time.Hour),
	})
	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(0))
	g.Expect(h.provider.MergedPRID).To(Equal(0))
}
```

The shortage is staged with real objects rather than a test hook in the
reconciler: production structs must not carry fields that exist only for
tests. `shortageObjects` supplies the two things `collectDeficits` insists
on — a parseable `FailedCreate` event and a live involved object.

The new imports for this file are `appsv1 "k8s.io/api/apps/v1"` and
`"sigs.k8s.io/controller-runtime/pkg/client"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/... -run TestShrink -v`
Expected: build failure — `unknown field CreatedAt in struct literal`.

- [ ] **Step 3: Expose the PR creation time**

In `internal/git/github.go`, add the field and populate it:

```go
type PRStatus struct {
	IsOpen           bool
	IsMerged         bool
	Mergeable        bool
	MergeableState   string
	ChecksState      string
	ChecksTotalCount int
	// CreatedAt is when the pull request was opened. The shrink TTL is
	// measured against it.
	CreatedAt time.Time
}
```

```go
		CreatedAt:        pr.GetCreatedAt().Time,
```

- [ ] **Step 4: Handle the shrink PR lifecycle**

First, a correction to the closed-PR branch that this task makes necessary.
Task 11 wrote that branch to stamp nothing when a pull request is closed
without being merged, and pinned it with a test asserting exactly that. At the
time it was right: shrink pull requests could not exist, so a closed-unmerged
pull request was always a rejected grow, and a rejected grow needs no cooldown
because the shortage that caused it will re-trigger on its own.

This task makes that same branch the primary way a human interacts with a
shrink. Shrinks are never auto-merged, so closing the pull request *is* the
rejection mechanism — and without a stamp, `Decide`'s shrink cooldown never
engages (it keys off a non-zero `LastShrink`), the immediate requeue recomputes
the same shrink, and the controller reopens an identical pull request seconds
after the reviewer closed it. The TTL path already stamps for precisely this
reason and says so in its comment; the human-rejection path needs the same
treatment, and a rejection deserves it more than a timeout does.

So in the `!status.IsMerged` case, stamp `LastShrink` when the pull request was
a shrink, and continue to stamp nothing for a grow:

```go
		if !status.IsMerged {
			// A closed shrink is a rejection. Without the cooldown stamp the
			// requeue below would recompute the same shrink and reopen it
			// immediately. A closed grow needs no stamp: the shortage that
			// caused it re-triggers on its own.
			if wasShrink {
				s.LastShrink = now
			}
			return
		}
```

This also closes a second path into the same loop: if `ClosePR` succeeds but
the `MutateState` that follows it fails, the next reconcile arrives here and
would otherwise drop the cooldown stamp entirely.

Update Task 11's `TestHandleActivePR_ClosedUnmergedShrink_ClearsLockWithoutStamping`
accordingly — it must now assert that a closed-unmerged *shrink* stamps
`LastShrink` and leaves `LastGrow` zero, and a companion case must pin that a
closed-unmerged *grow* still stamps neither. Rename it to say what it now
checks.

Then, in `handleActivePR`, insert this block immediately after `GetPRStatus`
succeeds and the PR is confirmed open, before the auto-merge logic:

```go
	if state.PRDirection == git.DirectionShrink {
		if reason, expire := shrinkPRShouldClose(policy, status, decision); expire {
			logger.Info("Closing shrink PR", "prID", state.PRID, "reason", reason)
			if err := r.GitProvider.ClosePR(ctx, state.PRID, reason); err != nil {
				logger.Error(err, "failed to close shrink PR", "prID", state.PRID)
				return ctrl.Result{}, err
			}
			// Recording the shrink timestamp is what stops the very next
			// reconcile from opening the same PR again.
			now := time.Now()
			err := r.Locker.MutateState(ctx, req.Namespace, quota.Name,
				func(s *lock.State) {
					s.PRID = 0
					s.PRDirection = ""
					s.LastShrink = now
				})
			if err != nil {
				logger.Error(err, "failed to release lock after closing shrink PR")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}
```

and add the predicate:

```go
// shrinkPRShouldClose reports whether an open shrink pull request has to be
// abandoned, and why. Growth supersedes it because a shortage is a live
// outage; the TTL catches the case where nobody reviewed it and the lock would
// otherwise be held indefinitely.
func shrinkPRShouldClose(
	policy sizing.Policy,
	status *git.PRStatus,
	decision sizing.Decision,
) (string, bool) {
	if decision.Direction == sizing.DirectionGrow {
		return "Superseded: a shortage was detected and the quota has to grow " +
			"instead.\n\n" + decision.Reason, true
	}
	if !status.CreatedAt.IsZero() &&
		time.Since(status.CreatedAt) > policy.ShrinkPRTTL {
		return "Closing automatically: this shrink proposal has been open for " +
			policy.ShrinkPRTTL.String() + " without review. A fresh proposal " +
			"will be opened once the cooldown expires.", true
	}
	return "", false
}
```

`handleActivePR` needs the policy; add `policy sizing.Policy` to its parameter
list and pass it from `Reconcile`.

- [ ] **Step 5: Create shrink PRs**

In `Reconcile`, replace the placeholder shrink branch from Task 8 — but keep
its `deficitScanFailed` guard. Task 8 introduced that guard precisely so it
would already be in place when this branch became actionable, and this is the
task that makes it actionable. Deleting it here would undo that.

The asymmetry it protects against: a deficit can only ever raise the target, so
a failed event scan cannot wrongly cause a grow — but it can understate demand
enough to tip a quota from "no action" into "shrink". A shrink proposed from
data we know to be incomplete is a bad pull request, and the human-review
requirement is not a substitute: it puts the burden of catching our bad data on
the reviewer, who has no way to see that the scan failed.

```go
	if decision.Direction == sizing.DirectionShrink && deficitScanFailed {
		logger.Info("Shrink suppressed: the event scan failed, so the " +
			"target may be understated")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	if decision.Direction != sizing.DirectionNone {
		return r.handleNewProposal(ctx, req, quota, ns, policy, state, decision)
	}
```

Keep the explanatory comment on `deficitScanFailed` at the scan site, updating
its wording now that the shrink branch is real rather than pending.

This needs a test, since the guard now suppresses a pull request that would
otherwise be opened: make `collectDeficits` fail and assert that a quota whose
metrics alone would produce a shrink opens no pull request
(`FakeGitProvider.CreatePRCalls == 0`). The existing fake client supports
this — a `List` on events can be made to fail with an interceptor, the same
mechanism the conflict tests in `internal/lock` use. If no such harness exists
in `internal/controller`, add the smallest one that makes the event list fail;
do not skip the test.

Widening the route makes the rest of `handleNewProposal` reachable for a
shrink, so every part of it that assumed a grow has to be adapted. The
recommendation event is one: it currently reads

```go
	msg := fmt.Sprintf("Recommendation: Increase %s from %s to %s", ...)
	r.Recorder.Event(&quota, corev1.EventTypeWarning, "QuotaResizeRecommended", msg)
```

which on a shrink produces `Warning QuotaResizeRecommended: Recommendation:
Increase requests.cpu from 16 to 12` — the verb inverted, and an optimisation
classified as a shortage. This is visible in `kubectl describe` and to any
event-based alerting. Derive the verb from `decision.Direction`, and use
`EventTypeNormal` for a shrink: a proposal to reclaim unused quota is not a
warning condition.

Read the rest of the function with the same question in mind and report
anything else that reads as grow-only.

In `handleNewProposal`, skip the grow cooldown for a shrink — the shrink
cooldown gate in `Decide` already governs it, and applying both would silently
double the wait:

```go
	if decision.Direction == sizing.DirectionGrow && !state.LastModified.IsZero() {
		elapsed := time.Since(state.LastModified)
		if elapsed < policy.GrowCooldown {
			remaining := policy.GrowCooldown - elapsed
			logger.Info("Skipping resize due to cooldown",
				"cooldown", policy.GrowCooldown, "remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining + 1*time.Second}, nil
		}
	}
```

The direction-specific timestamp after creation is **already in place** — Task
10 moved this block forward to keep the pull request label and the lease from
being written from two different sources:

```go
	err = r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
		s.PRID = newPRID
		s.PRDirection = decision.Direction.String()
		if decision.Direction == sizing.DirectionShrink {
			s.LastShrink = time.Now()
		} else {
			s.LastGrow = time.Now()
		}
	})
```

Verify it reads exactly like this and move on; do not add a second
`MutateState` call.

- [ ] **Step 6: Pin the route this task exists to open**

The tests above all assert that something is *not* done: a pull request closed,
left alone, or suppressed. Nothing asserts that a shrink pull request is ever
opened at all — so reverting the route at the top of Step 5 back to
`decision.Direction == sizing.DirectionGrow`, which would undo this entire
task, leaves the whole suite green. That has to be fixed before the task can be
called done.

Add `TestShrink_OpensAPullRequest`: the fixture from
`TestShrink_SuppressedByFailedScan` without the interceptor — a complete
14-day window and metrics that genuinely produce `DirectionShrink` — asserting
`CreatePRCalls == 1` and `LastDirection == git.DirectionShrink`, and that the
lease afterwards records `PRDirection` as shrink.

Three of the earlier tests seed no observation window, so `GateWindow` blocks
and their decision is `DirectionNone` rather than `DirectionShrink`. They
therefore prove less than their names suggest — `TestShrink_YoungPRIsLeftAlone`
in particular demonstrates "no decision plus young pull request leaves things
alone", not the shrink steady state, and no test ever executes the
`case sizing.DirectionShrink` log branch. Seed the window in the shared
harness so each test exercises the state it claims to. Since both the window
and the interceptor are now wanted by more than one test, give
`newShrinkHarness` optional parameters for them rather than duplicating its
setup a second time.

While seeding, add the case for a zero `CreatedAt`: an unpopulated timestamp
must not read as infinitely old and expire the pull request immediately. The
guard exists; nothing exercises it.

- [ ] **Step 7: Run the full suite**

Run: `make test`
Expected: PASS, including every `TestShrink` case and Task 11's renamed
closed-unmerged cases.

If `TestShrink_YoungPRIsLeftAlone` fails with a merge instead, the auto-merge
restriction from Task 11 is not in effect — check the `state.PRDirection`
comparison there.

- [ ] **Step 8: Lint and commit**

```bash
make lint
git add internal/git internal/controller
git commit -m "feat(controller): shrink pull requests with supersede and TTL

Shrink proposals become real pull requests. A live shortage closes an open
shrink with an explanatory comment and hands the lock to the grow PR, and a
shrink that stays unreviewed past its TTL closes itself. Both paths record the
shrink timestamp so the cooldown prevents an immediate reopen."
```

---

### Task 13: Rollout flag and namespace opt-out

Implements spec 8. Until this task, shrinking is unreachable because
`BasePolicy.ShrinkEnabled` is hard-coded to `false` in `main.go`.

**Files:**
- Modify: `cmd/main.go`
- Modify: `internal/config/constants.go`
- Modify: `config/manager/manager.yaml`
- Test: `internal/sizing/policy_test.go` — opt-out precedence

**Interfaces:**
- Consumes: `Policy.ShrinkEnabled` (Task 2).
- Produces: `--enable-shrink` flag, `ENABLE_SHRINK` environment variable.

- [ ] **Step 1: Write the failing test**

Add to `internal/sizing/policy_test.go`:

```go
func TestParsePolicy_ShrinkOptOutCannotOverrideTheFlag(t *testing.T) {
	// The global flag is expressed as the base policy. A namespace may opt
	// out of shrinking, but it may not opt in when the operator disabled it.
	base := DefaultPolicy()
	base.ShrinkEnabled = false

	p, _ := ParsePolicy(map[string]string{
		"resizer.io/shrink-enabled": "true",
	}, base)

	if p.ShrinkEnabled {
		t.Fatal("namespace annotation enabled shrinking against the global flag")
	}
}

func TestParsePolicy_ShrinkOptOutApplies(t *testing.T) {
	base := DefaultPolicy()
	base.ShrinkEnabled = true

	p, _ := ParsePolicy(map[string]string{
		"resizer.io/shrink-enabled": "false",
	}, base)

	if p.ShrinkEnabled {
		t.Fatal("namespace opt-out was ignored")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sizing/... -run TestParsePolicy_Shrink -v`
Expected: FAIL on the first test — the current parser assigns
`value != "false"`, which lets a namespace switch shrinking back on.

- [ ] **Step 3: Make the opt-out one-directional**

In `internal/sizing/policy.go`, replace the `shrink-enabled` case:

```go
		case name == "shrink-enabled":
			// Opt-out only: a namespace may switch shrinking off, never on.
			// Enabling it is the operator's decision, made with the flag.
			if value == "false" {
				out.ShrinkEnabled = false
			}
```

- [ ] **Step 4: Add the annotation constants**

In `internal/config/constants.go`, mark the superseded constants and add the
new ones:

```go
	// AnnotationCPUThreshold sets the CPU threshold percentage (e.g. "80").
	//
	// Deprecated: use AnnotationCPUHeadroom. The value is still honoured and
	// converted to a headroom fraction.
	AnnotationCPUThreshold = "resizer.io/cpu-threshold"
```

(the same `Deprecated:` note on the memory and storage threshold and on all
three increment constants), plus:

```go
	// AnnotationCPUHeadroom sets the CPU headroom fraction (e.g. "0.25" or "25%").
	AnnotationCPUHeadroom = "resizer.io/cpu-headroom"
	// AnnotationMemoryHeadroom sets the memory headroom fraction.
	AnnotationMemoryHeadroom = "resizer.io/memory-headroom"
	// AnnotationStorageHeadroom sets the storage headroom fraction.
	AnnotationStorageHeadroom = "resizer.io/storage-headroom"

	// AnnotationTolerance sets the dead band around the target (default "0.15").
	AnnotationTolerance = "resizer.io/tolerance"
	// AnnotationWindowDays sets the observation window length (default "14").
	AnnotationWindowDays = "resizer.io/window-days"
	// AnnotationShrinkCooldownDays sets the shrink cooldown (default "7").
	AnnotationShrinkCooldownDays = "resizer.io/shrink-cooldown-days"
	// AnnotationMaxShrinkStep caps a single shrink (default "0.25").
	AnnotationMaxShrinkStep = "resizer.io/max-shrink-step"
	// AnnotationShrinkPRTTLDays expires an unreviewed shrink PR (default "7").
	AnnotationShrinkPRTTLDays = "resizer.io/shrink-pr-ttl-days"
	// AnnotationShrinkEnabled opts a namespace out of shrinking. It cannot
	// opt in when --enable-shrink is off.
	AnnotationShrinkEnabled = "resizer.io/shrink-enabled"
```

- [ ] **Step 5: Add the flag**

In `cmd/main.go`, next to `enableAutoMerge`:

```go
	var enableShrink bool
```

```go
	flag.BoolVar(&enableShrink, "enable-shrink", os.Getenv("ENABLE_SHRINK") == trueStr,
		"If set, the controller opens pull requests that lower quotas. When "+
			"unset it still computes and exports the recommendation, so the "+
			"effect can be reviewed through metrics before enabling it.")
```

and replace the hard-coded assignment from Task 8:

```go
	basePolicy := sizing.DefaultPolicy()
	basePolicy.ShrinkEnabled = enableShrink
```

- [ ] **Step 6: Document the flag in the manifest**

In `config/manager/manager.yaml`, add the commented-out argument next to the
existing ones so operators discover it:

```yaml
        args:
          - --leader-elect
          - --health-probe-bind-address=:8081
          # Uncomment once the dry-run metrics look right. See
          # docs/OPERATIONS.md for what to check first.
          # - --enable-shrink
```

- [ ] **Step 7: Run the full suite**

Run: `make test`
Expected: PASS. Note that `TestShrink_*` from Task 12 sets
`policy.ShrinkEnabled = true` directly on the base policy, so it is unaffected.

- [ ] **Step 8: Lint and commit**

```bash
make lint
git add cmd/main.go internal/config/constants.go internal/sizing config/manager/manager.yaml
git commit -m "feat: gate shrinking behind --enable-shrink

Shrink pull requests require an explicit operator decision. The recommendation
is computed and exported either way, so the effect can be reviewed through
metrics first. The namespace annotation is an opt-out only and cannot enable
shrinking against the flag."
```

---

### Task 14: End-to-end coverage and documentation

Implements spec 10 (E2E) and the documentation obligations from spec 7.3 and 8.

**Files:**
- Modify: `test/e2e/e2e_test.go`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/INSTALLATION.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/TODO.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything from Tasks 1–13.
- Produces: no code symbols.

- [ ] **Step 1: Add the E2E shrink scenario**

In `test/e2e/e2e_test.go`, add a case that pre-seeds a complete observation
window on the Lease and asserts a shrink PR appears. Seeding is what keeps the
test to seconds instead of two weeks:

```go
	It("should propose a shrink for an oversized quota", func() {
		By("seeding a fully covered 14-day observation window")
		window := seedWindow(14, "4")
		cmd := exec.Command("kubectl", "annotate", "lease",
			"state-e2e-compute", "-n", namespace,
			fmt.Sprintf("resizer.io/observation-window=%s", window), "--overwrite")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for a shrink pull request")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "lease",
				"state-e2e-compute", "-n", namespace,
				"-o", "jsonpath={.metadata.annotations['resizer\\.io/pr-direction']}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("shrink"))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})
```

Add the helper next to it. It mirrors `fillWindow` from
`internal/sizing/window_test.go`, but emits the JSON directly because the E2E
suite must not import internal packages:

```go
// seedWindow builds a JSON observation window covering the given number of
// completed days at a constant peak, so the shrink gates pass immediately.
func seedWindow(days int, peak string) string {
	type bucket struct {
		Date   string            `json:"d"`
		N      int               `json:"n"`
		First  string            `json:"first"`
		Last   string            `json:"last"`
		MaxGap string            `json:"maxGap"`
		Peaks  map[string]string `json:"p"`
	}
	payload := struct {
		Version int      `json:"v"`
		UID     string   `json:"uid"`
		Days    []bucket `json:"days"`
	}{Version: 1}

	now := time.Now().UTC()
	for i := 1; i <= days; i++ {
		payload.Days = append(payload.Days, bucket{
			Date:   now.AddDate(0, 0, -i).Format("2006-01-02"),
			N:      288,
			First:  "00:00",
			Last:   "23:55",
			MaxGap: "5m0s",
			Peaks:  map[string]string{"requests.cpu": peak},
		})
	}

	raw, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}
```

The `uid` field is deliberately left empty: `Window.Observe` only resets on a
*mismatch*, and an empty stored UID adopts whatever the live quota reports.

The E2E deployment must run with `--enable-shrink`; add it to the manager args
in the kustomize overlay the suite deploys.

- [ ] **Step 2: Run the E2E suite**

Run: `make test-e2e`
Expected: PASS. This requires a Kind cluster; the target creates one.

- [ ] **Step 3: Rewrite the growth statement in ARCHITECTURE.md**

Replace section 2.2's note (currently at `docs/ARCHITECTURE.md:38`):

```markdown
*Hinweis:* Es gibt kein festes `MaxAllowedLimit` pro Namespace — nicht, um
unbegrenztes Wachstum zu erlauben, sondern weil der beobachtete Bedarf selbst
die Obergrenze bildet. Das Limit folgt dem Bedarf in beide Richtungen.
```

Replace the calculation model in the same section:

```markdown
**Berechnungs-Modell:**

$$ \text{Target} = \max(\text{Peak}_{\text{Fenster}}, \text{Used}) \times (1 + \text{Headroom}) $$

Gehandelt wird nur außerhalb eines Toleranzbands um diesen Zielwert. Details,
Guardrails und Rollout: [Design-Dokument](design/2026-08-08-quota-rightsizing.md).
```

Delete section 2.4's claim that the cooldown is the only guardrail, and list
the shrink gates instead. Add a subsection 3.5 summarising the direction label
and the supersede/TTL rules, linking to spec 6.

- [ ] **Step 4: Document the configuration in INSTALLATION.md**

Add a table of every annotation from spec 7.2 with its default, and a
migration note stating that `*-threshold` and `*-increment` still work and how
they map onto `*-headroom`. State explicitly that `resizer.io/shrink-enabled`
is an opt-out and cannot enable shrinking when `--enable-shrink` is off.

- [ ] **Step 5: Write the rollout runbook in OPERATIONS.md**

Add a section covering the dry-run procedure:

1. Deploy without `--enable-shrink` and set `--metrics-bind-address=:8443`.
2. Watch `resizer_quota_waste_ratio` for at least one full window (14 days).
   A ratio near 1 means the quota already tracks demand; a ratio above 2 marks
   a namespace worth reclaiming.
3. Check `resizer_shrink_blocked_by{gate="window"}`. If it stays at 1, the
   controller is being restarted too often for a window to complete — fix that
   before enabling shrinking.
4. Enable `--enable-shrink`. Expect the first shrink PRs within a day, one per
   quota, each capped at 25 %.
5. To exclude a namespace, annotate it
   `resizer.io/shrink-enabled: "false"`.

Also document that shrink PRs are never auto-merged and expire after 7 days,
and that a shortage closes them early.

- [ ] **Step 6: Update TODO.md and README.md**

In `docs/TODO.md`, tick Phase 6 (auto-merge, which is implemented) and add a
Phase 8 covering this work, with the Stage 1 / Stage 2 split. In `README.md`,
change the "Metric-based Resizing: Increases quota when usage > X%" bullet to
describe the bidirectional behaviour, and add a bullet for the dry-run mode.

- [ ] **Step 7: Verify the docs match the code**

Read the annotation table in `docs/INSTALLATION.md` against the `switch` in
`sizing.ParsePolicy`. Every documented annotation must have a case, and every
case must be documented. This is a manual check — there is no test for it.

- [ ] **Step 8: Commit**

```bash
git add test/e2e docs README.md config
git commit -m "docs: describe bidirectional rightsizing and its rollout

Documents the target formula, the shrink guardrails, the full annotation set
with its migration path, and the metrics-first rollout procedure. Adds an
end-to-end case that seeds a complete observation window so the shrink path is
exercised without waiting two weeks."
```

---

## Verification

The change is complete when all of the following hold:

```bash
make test        # unit + envtest
make test-e2e    # Kind-based end-to-end
make lint        # golangci-lint v2
```

and these behavioural claims are true:

| Claim | Where it is proven |
|---|---|
| Grow behaviour is unchanged for existing installations | `TestReconcile_GrowUsesHeadroomFromLegacyThreshold` |
| A quota inside the band produces no PR | `TestReconcile_QuietInsideToleranceBand` |
| Shrinking never exceeds 25 % per PR | `TestDecide_ShrinkIsStepCapped` |
| Shrinking never goes below current demand | `TestDecide_HardFloorFromCurrentUsage` |
| Controller downtime blocks shrinking | `TestWindow_IsComplete/downtime_invalidates_the_window` |
| A shrink PR is never auto-merged | `TestAutoMerge_NeverMergesAShrink` |
| A shortage frees the lock from a shrink PR | `TestShrink_SupersededByAShortage` |
| An unreviewed shrink PR does not hold the lock forever | `TestShrink_ExpiresAfterTTL` |
| A namespace cannot enable shrinking against the flag | `TestParsePolicy_ShrinkOptOutCannotOverrideTheFlag` |
| Object-count quotas get valid values | `TestQuantize/pods_round_up_to_a_whole_number` |
