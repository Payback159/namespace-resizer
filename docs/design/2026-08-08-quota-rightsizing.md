# Design: Bidirectional Quota Rightsizing

**Date**: 2026-08-08
**Status**: Draft, approved
**Affects**: `internal/controller`, `internal/lock`, `internal/git`, `internal/config`

## 1. Problem

The current implementation can only ever enlarge quotas. Both paths that fill
`recommendations` — the metric analysis in `calculateRecommendations` and the
event analysis in `analyzeEvents` — produce only higher values; `analyzeEvents`
even filters smaller values out explicitly
(`resourcequota_controller.go:482`). `ARCHITECTURE.md:38` records this as a
design decision: no `MaxAllowedLimit`, so that growth stays possible across the
whole lifecycle.

Four effects follow from that, and all four waste resources:

1. **Peaks get cemented.** A one-off event — a rolling update with `maxSurge`,
   a batch job, a failed rollout — raises the quota permanently. No mechanism
   ever lowers it again.
2. **The metric path triggers without real demand.** At 80 % utilisation no pod
   has been rejected yet, and `hard` is raised by 20 % anyway. The decision
   rests on a single sample point, with no consideration of whether the
   utilisation persists.
3. **`used` measures requests, not consumption.** Over-requesting workloads make
   the controller enlarge quota for air that is never used. Poor request sizing
   is rewarded instead of made visible.
4. **Buffer on buffer.** The event path computes
   `total = (used + deficit) × (1 + increment)`
   (`resourcequota_controller.go:470-476`) — the markup is applied to the new
   total rather than to the deficit. Repeated bursts compound.

## 2. Goal

The quota follows actually observed demand with a defined headroom — in
**both** directions. The controller raises quickly on a genuine shortage and
reduces over-provisioning slowly and reviewably.

Demand is defined here as `quota.status.used`, i.e. the sum of the
**requests**. That is the quantity admission fails on: a quota below the
requests sum rejects pods, however little CPU is actually consumed. Real
consumption (metrics-server, Prometheus) is a valuable reporting signal but
not a safe basis for shrinking, and stays out of scope for this design.

## 3. Rule Model

A single target formula governs both directions. The previous threshold path
(`used/hard ≥ 80 % → hard × 1.2`) is dropped outright.

```
for each resource in quota.status.hard:
    peak   = max( daily peaks across all covered days , used_now )
    peak   = max( peak , used_now + deficit )                 # event accelerator
    target = peak × (1 + headroom)
    target = max( target , used_now × (1 + headroom) , <res>-min )   # hard floor

    target > hard × (1 + tolerance)  →  grow candidate: target
    target < hard × (1 - tolerance)  →  shrink candidate: max(target, hard × (1 - maxShrinkStep))
    otherwise                        →  no action
```

`deficit` is the deficit computed for this resource from `FailedCreate` events
(today's `calculateWorkloadDeficit` logic), or `0` when there is no current
event. `<res>-min` is the optional lower-bound annotation from 7.2, or `0` when
unset. The current, still incomplete day does not count towards the daily
peaks — it is already covered through `used_now`.

### 3.1 Direction Decision

**Grow always wins.** As soon as a single resource wants to grow, the whole
decision is a grow; every shrink candidate is discarded. A PR that raised CPU
while lowering memory would be hard to review and would undermine the rule
"never auto-merge a shrink".

A shrink only arises when no resource wants to grow **and** all the gates in
section 3.3 hold.

### 3.2 Tolerance Band

With headroom 0.25 and tolerance 0.15, constant load (`peak = used_now = U`,
hence `target = 1.25 × U`) yields a stable corridor:

```
Grow   when  1.25 × U > 1.15 × hard   ⟹   hard < 1.087 × U
Shrink when  1.25 × U < 0.85 × hard   ⟹   hard > 1.47  × U

stable: hard ∈ [1.087 × U … 1.47 × U]
```

Nothing happens inside this band. Flapping between grow and shrink is therefore
ruled out structurally and needs no additional locks.

One consequence matters when judging shrink results: the band ends **above**
the target. A reduction stops as soon as `hard ≤ target / 0.85 = 1.176 ×
target` is reached, not exactly at `target`. A residual buffer of up to 17.6 %
above the computed target is intended behaviour, not a bug.

### 3.3 Shrink Gates

A shrink only fires when **all** gates hold:

| Gate | Condition |
|---|---|
| `window` | The observation window is fully covered across `windowDays` days (see 4.2), checked per resource |
| `recent-grow` | No grow happened within the window (`last-grow` older than `windowDays`) |
| `cooldown` | `last-shrink` is older than `shrinkCooldownDays` |
| `lock` | No open PR for this quota (follows from the existing Lease lock) |
| `enabled` | Shrinking is enabled globally and the namespace does not carry `resizer.io/shrink-enabled: "false"` |

Which gate blocked is recorded in `Decision.BlockedBy` and emitted as a
Prometheus metric and a `V(1)` log line — not as a PR.

Two protections are already anchored in the formula from section 3 and need no
gate of their own: the **hard floor**
(`max(target, used_now × (1 + headroom), min annotation)`) and the
**step cap** (`max(target, hard × (1 - maxShrinkStep))`).

### 3.4 Reduction Example

A quota over-provisioned fourfold (`hard = 16` CPU, `peak₁₄ = 4`, `used = 3.5`,
hence `target = 5`). A shrink fires while `hard > target / 0.85 = 5.88`; each
step is capped at `hard × 0.75`:

```
Round 1 (day  0):  16    → 12       cap binds (12 > target 5)
Round 2 (day  7):  12    →  9       cap binds
Round 3 (day 14):   9    →  6.75    cap binds
Round 4 (day 21):   6.75 →  5.06    cap binds
Day 28:             5.06 <  5.88  →  no shrink, band closed
```

Four reviewed PRs over three weeks; final value 5.06 rather than exactly 5 —
the residual buffer from the tolerance band (see 3.2). Fourfold
over-provisioning becomes 1.45-fold. The sequence can be interrupted at any
point by a genuine shortage (see 6.2).

## 4. Observation & Data Model

### 4.1 Sampling

On every reconcile the controller reads `quota.status.used` and records it in
the current day's bucket (maximum per resource). The window is a ring over
`windowDays` days, JSON-encoded in the annotation
`resizer.io/observation-window` on the existing state Lease
(`state-<namespace>-<quota>` in the controller namespace, see
`ARCHITECTURE.md` 3.3).

```json
{
  "v": 1,
  "uid": "3f2a1c8e-...",
  "days": [
    {
      "d": "2026-08-08",
      "n": 271,
      "first": "00:02",
      "last": "23:58",
      "maxGap": "7m",
      "p": { "requests.cpu": "11500m", "requests.memory": "48Gi" }
    }
  ]
}
```

Values are stored as `resource.Quantity` strings so that format and precision
survive. With 14 days and a handful of resources the annotation is about
1.7 KB — far below the Kubernetes limit of 256 KB for all annotations of an
object combined.

**Write load**: the Lease is not written on every reconcile, only when a peak
rises or more than an hour has passed since the last write. At 200 quotas that
comes to roughly 5000 writes a day (≈ 0.06/s) — negligible for the API.

### 4.2 Window Completeness

It is **not** enough to check that 14 day entries exist. A controller that ran
for only ten minutes each day would have 14 entries and still no dependable
history — and therefore a dangerously low peak.

The controller therefore carries forward, per sample:

```
maxGap = max(maxGap, now − lastSampleAt)
```

`lastSampleAt` is carried across the day boundary: a gap spanning midnight
raises `maxGap` in the *new* day's bucket, while the old one stands out through
its `last`. Both days are correctly discarded that way.

A day counts as **covered** when `maxGap ≤ 1h`, `first ≤ 00:30` and
`last ≥ 23:30`. Controller downtime therefore invalidates the affected days
automatically instead of feigning a low peak.

The current day is by definition never covered. The window counts as complete
when all `windowDays` **completed** days before today are covered.

### 4.3 Resource Changes

When a resource appears in the quota for the first time (`requests.storage`,
say), it is missing from the older buckets. The `window` gate therefore applies
**per resource**: only what has been observed across all covered days is
shrunk. For newly added resources only the grow path applies until a full
window has elapsed.

## 5. Structure

The logic moves into a new package `internal/sizing` with no Kubernetes client.
The controller becomes an orchestrator: observe → decide → PR.

```
internal/sizing/
  decide.go     Decide(Input) Decision      — pure function, clock injected
  window.go     rolling-window encode/decode, coverage check
  config.go     annotations → policy (including the migration from 7.1)
  deficit.go    event deficit calculation (taken from resourcequota_utils.go)

internal/controller/
  resourcequota_controller.go   orchestration only
  observation.go                sampling → Lease
```

```go
type Direction int   // None | Grow | Shrink

type Decision struct {
    Direction Direction
    Targets   map[corev1.ResourceName]resource.Quantity
    Reason    string   // structured rationale, ends up in the PR body
    BlockedBy []Gate   // which gates prevented a shrink
}
```

The reason for this cut is concrete: the shrink gates are time-dependent (is
the window complete? has the cooldown expired?). With an injected clock they
can be checked as table tests in milliseconds — cases such as "14 days of
history, day 9 of which has a 6-hour gap" would not be practically testable
against a real API server. On top of that the rationale no longer disappears
into the log but lands structured in the PR body.

`resourcequota_controller.go` currently has 498 lines and five
responsibilities (reconcile orchestration, metric analysis, event analysis, PR
lifecycle, auto-merge). Metric and event analysis move out entirely.

## 6. PR Lifecycle

### 6.1 Direction State

Alongside the PR ID, the Lease stores the direction
(`resizer.io/pr-direction: grow|shrink`). The PR itself carries a GitHub label
`resizer.io/direction` with the same value.

The label is not cosmetic: `FindOpenPR` — the orphan recovery from commit
`ebf581e` — returns only a PR ID today. Without a direction the controller
would adopt an orphaned shrink PR as a grow and potentially auto-merge it. The
signature is extended to `(int, Direction, error)` and reads the direction from
the label.

**Auto-merge applies exclusively to `grow`**, regardless of
`--enable-auto-merge` and the `resizer.io/auto-merge` annotation.

> **Implementation note (added after review).** The label turned out to be an
> insufficient carrier on its own: it is attached in a second API call after
> the PR is created, so a failure there leaves an unlabelled shrink PR that
> orphan recovery reads as a grow. The direction is therefore encoded in the
> branch name as well — `resize/<direction>/<namespace>/<quota>/<timestamp>` —
> which is created atomically with the PR and cannot fail separately. The
> label remains as a fallback for PRs predating the change, and for humans
> filtering in the GitHub UI. See `ARCHITECTURE.md` section 3.5.

### 6.2 Supersede

If the open PR is a shrink and the current decision is a grow:

1. `ClosePR(ctx, prID, comment)` with an explanatory comment (which resource,
   what demand, which triggering event).
2. Release the lock, set `last-shrink = now`.
3. Requeue — the next pass opens the grow PR.

`ClosePR` is a new method on the `git.Provider` interface.

### 6.3 TTL

A shrink PR left open without review for `shrinkPrTTL` (default 7 days) is
closed through the same sequence. The `last-shrink = now` is essential here:
without it the controller would immediately reopen the same PR on the next
reconcile.

### 6.4 Lease Annotations

| Annotation | Role |
|---|---|
| `resizer.io/last-modified` | unchanged — event deduplication |
| `resizer.io/last-grow` | gate `recent-grow` |
| `resizer.io/last-shrink` | gate `cooldown` |
| `resizer.io/pr-direction` | direction of the active PR |
| `resizer.io/observation-window` | rolling window (see 4.1) |

## 7. Configuration

### 7.1 Migration

`resizer.io/<res>-headroom` replaces threshold and increment. Fallback chain,
so that existing installations do **not** change their grow behaviour:

```
*-headroom set?       → use it
else *-increment?     → take over directly (same semantics: 0.2 → 0.2)
else *-threshold?     → derive: 100/threshold − 1              (80 → 0.25)
else                  → default 0.25
```

The old annotations keep working and produce a deprecation log line. The
constants in `internal/config/constants.go` are retained with a
`// Deprecated:` comment.

### 7.2 Parameters

All values are set globally by flag/ConfigMap and can be overridden per
namespace via annotation.

| Parameter | Annotation | Default |
|---|---|---|
| Headroom | `resizer.io/<res>-headroom` | `0.25` |
| Tolerance | `resizer.io/tolerance` | `0.15` |
| Lower bound | `resizer.io/<res>-min` | — (Quantity, optional) |
| Window length | `resizer.io/window-days` | `14` |
| Shrink cooldown | `resizer.io/shrink-cooldown-days` | `7` |
| Max. shrink step | `resizer.io/max-shrink-step` | `0.25` |
| Shrink PR TTL | `resizer.io/shrink-pr-ttl-days` | `7` |
| Grow cooldown | `resizer.io/cooldown-minutes` | `60` (unchanged) |
| Shrink opt-out | `resizer.io/shrink-enabled` | `true` |

Shrinking only happens when **both** hold: the global `--enable-shrink` flag is
set (default `false`, see 8) **and** the namespace has not opted out with
`resizer.io/shrink-enabled: "false"`. The annotation is an opt-out within
globally enabled shrinking, not a way to override the flag.

### 7.3 Deliberately Not Introduced

A `*-max` ceiling per namespace. The target formula already bounds growth at
observed demand; a second, competing limiter would be configuration without
additional benefit.

`ARCHITECTURE.md:38` is rewritten accordingly: no longer "no maximum, so that
unbounded growth is possible", but "no fixed maximum needed, because observed
demand forms the upper bound".

## 8. Rollout

Flag `--enable-shrink`, default `false`. The controller computes shrink
decisions in full regardless and exports them as metrics. What it would do is
therefore visible for weeks before the first PR appears — this mirrors the
"observer mode" from `ARCHITECTURE.md` 3.1, with which the project has already
built confidence once.

```
resizer_quota_target{namespace,quota,resource}          # computed target
resizer_quota_waste_ratio{namespace,quota,resource}     # hard / target
resizer_shrink_blocked_by{namespace,quota,gate}         # blocking gate
resizer_decision_total{namespace,quota,direction}       # counter
```

The namespace opt-out via `resizer.io/shrink-enabled: "false"` remains in
effect after the flag is enabled — consistent with the controller's opt-out
principle.

## 9. Error Handling

The guiding rule throughout: **when in doubt, do not shrink.**

| Fault | Behaviour |
|---|---|
| Window JSON corrupt or unknown `v` | Treat the window as empty and restart sampling; the `window` gate blocks shrinking automatically |
| Controller downtime | `maxGap` invalidates the affected days → window incomplete → no shrink |
| Lease write conflict (optimistic concurrency) | Re-read + retry; the pattern already exists from commit `5d88f39` |
| `ClosePR` fails | The lock stays, requeue — a delay, not an inconsistent state |
| GitHub unreachable | Return the error, controller-runtime backoff (unchanged) |
| Clock jumps backwards | Buckets dated in the future are discarded |
| Quota deleted and recreated under the same name | `quota.metadata.uid` is carried in the window; on a change the window is reset |

### 9.1 Latent Bug: Object Counters

`convertToReadableFormat` (`resourcequota_utils.go:217`) routes everything
except memory and storage through `resource.NewMilliQuantity`. For countable
resources such as `pods` or `count/deployments.apps` that produces invalid
quantities at non-round values: a target of 11.25 pods becomes `"11250m"`,
which Kubernetes rejects as a pod quota.

Today this rarely shows, because `hard × 1.2` on integer starting values
usually comes out round again. The new target formula produces fractional
values far more often, so the fix belongs in this design: a third branch that
rounds up to whole numbers for countable resources.

## 10. Tests

**`internal/sizing` — table tests with an injected clock:**

- Target formula per resource type: CPU (milli), memory (BinarySI), object
  counters (integral, see 9.1)
- Tolerance band: values just inside and just outside both bounds
- Hard floor: `used_now` and `*-min` override a lower peak
- Step cap: target far below `hard × 0.75`
- Each gate from 3.3 blocking individually
- Grow beats shrink on mixed candidates
- Event accelerator: a deficit raises the peak immediately, without waiting for
  sampling

**`internal/sizing/window`:**

- Encode/decode round trip, ring rotation across the day boundary
- `maxGap` carry-forward; downtime invalidates a day
- A newly appearing resource blocks only that resource
- Corrupt JSON and unknown version
- A UID change resets the window

**Controller (envtest):**

- Supersede sequence: open shrink PR + FailedCreate → close, lock free, grow PR
- TTL closure including the `last-shrink` update
- Auto-merge applies to `grow`, not to `shrink`
- Orphan recovery adopts with the correct direction

**Existing tests:**

- `smart_calculation_test.go` and `event_analysis_*_test.go` move into the new
  package; the cases stay valid in substance, since the deficit calculation
  does not change
- `limits_test.go` goes away with the threshold path
- `fake_git_provider.go` is extended with `ClosePR` and the direction

**E2E (`test/e2e`):** a shrink scenario with a pre-seeded Lease window.

## 11. Implementation Order

The design falls into two stages that follow the rollout from section 8. After
stage 1 the system is fully operational and already delivers value (visibility
of waste), without a single shrink PR being possible.

**Stage 1 — observation and decision, without shrink PRs**

1. `internal/sizing`: window encoding, coverage check, target formula, gates,
   config migration — including table tests
2. The fix for countable resources from 9.1
3. `observation.go`: sampling into the Lease, write-sparing
4. Move the controller onto `sizing.Decide`; the threshold path and
   `limits_test.go` go away. Grow behaviour stays unchanged through the
   migration fallbacks
5. The metrics from section 8

**Stage 2 — shrink PRs**

6. `ClosePR` on `git.Provider`, `FindOpenPR` extended with the direction,
   direction label on creation
7. Direction state in the Lease, auto-merge only for `grow`
8. Supersede and TTL
9. The `--enable-shrink` flag, `resizer.io/shrink-enabled`
10. envtest and E2E coverage, documentation (`ARCHITECTURE.md`,
    `INSTALLATION.md`, `OPERATIONS.md`)

## 12. Out of Scope

- Reconciliation against real consumption (metrics-server/Prometheus) as a
  reporting signal for over-requesting workloads
- Cluster capacity or aggregate budget awareness across all namespaces
- Git providers other than GitHub
