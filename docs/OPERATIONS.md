# Namespace Resizer — Operations Handbook

This document describes how the **Namespace Resizer controller** behaves, from an operations point of view. Use it to retrace the controller's decisions and to find the cause quickly when something looks wrong (for example: "why was my quota not raised?").

## 1. Principle

The controller watches Kubernetes namespaces and adjusts `ResourceQuota` objects to observed demand — **in both directions**. It grows a quota promptly on a genuine shortage, and reduces over-provisioning slowly and reviewably.
It follows the **GitOps principle**: changes are proposed as pull requests in the Git repository, never applied directly to the cluster.

## 2. When Does the Controller Act?

There are three triggers:

### A. Demand Above the Target (metric-based, grow)
The controller computes a target per resource from observed demand (see section 3) and compares it with the current limit (`hard`). If the target sits above a tolerance band around `hard`, a grow PR is proposed.

*   **Behaviour change versus earlier versions:** a flat threshold of **80 % utilisation** (`used / hard`) used to trigger an increase immediately. With the default values (headroom 25 %, tolerance 15 %), that trigger point moves to **roughly 92 % utilisation** under constant load (`hard < 1.087 × used`; see [ARCHITECTURE.md](ARCHITECTURE.md) section 2.2 and the [design document](design/2026-08-08-quota-rightsizing.md) section 3.2). A quota sitting steadily at 85 % utilisation therefore **no longer receives a PR** — that is intended, not a regression. If you want the old trigger point, you can approximate it with a lower `-headroom` or `-tolerance` annotation.
*   **Example:** limit 10 CPU, demand rises to 9.3 CPU (93 %) -> trigger.

### B. Failed Deployments (event-based, grow)
When a pod cannot start because the quota is exhausted (`FailedCreate` event).
*   **Detection:** the controller reads the error message ("exceeded quota... requested: 2 CPU").
*   **Reaction:** the deficit raises the target immediately, without waiting for the next observation cycle.
*   **Multi-burst:** when several deployments fail at once, the controller sums their demand.
*   **Liveness check:** the controller ignores events from objects that are already gone (after a rollback, say), so it does not propose increases nobody needs.
*   **Safety guarantee:** a shrink is never proposed from an event scan that failed — if reading the events fails, the controller would rather suppress an otherwise due shrink for one cycle than shrink on possibly incomplete data.

### C. Over-provisioning (metric-based, shrink)
If the target sits below the tolerance band around `hard`, the quota is over-provisioned. A shrink is only proposed when **all** the gates in section 4 hold as well — see section 7 for the recommended rollout.

## 3. How Is the New Limit Calculated?

A single formula governs both directions:

```
target = max(peak over the observation window, current demand) × (1 + headroom)
```

*   **Headroom:** buffer above observed demand, **25 %** by default (annotation `resizer.io/<resource>-headroom`).
*   **Tolerance band:** nothing happens within ±15 % (annotation `resizer.io/tolerance`) around the target — this rules out flapping between grow and shrink structurally.
*   **Observation window:** 14 days of daily peaks (annotation `resizer.io/window-days`); only fully covered days count (see section 4).
*   **Shrink step cap:** a single shrink PR lowers the limit by at most 25 % (annotation `resizer.io/max-shrink-step`), even when the target sits further down. Large over-provisioning is reduced step by step across several PRs.
*   **Hard floor:** the target never falls below current demand (plus headroom) or a configured lower bound (`resizer.io/<resource>-min`).
*   **Rounding:** values are rounded to readable units (full MiB or 100m CPU, for instance, and rounded up to whole numbers for countable resources such as `pods`) to avoid awkward figures like `1288490188800m` or `11250m` pods.

Details and derivation: [design document](design/2026-08-08-quota-rightsizing.md) section 3.

## 4. Safety Mechanisms (Why Is Nothing Happening?)

When the controller does *not* act, it is usually one of these:

### A. Grow Cooldown
After every grow, the controller pauses for that namespace.
*   **Duration:** **60 minutes** by default.
*   **Reason:** prevents flapping and PR spam.
*   **Log message:** `Skipping resize due to cooldown`

### B. Opt-Out
A namespace can be ignored explicitly.
*   **Check:** look for the annotation `resizer.io/enabled: "false"` on the namespace.

### C. Open Pull Request (locking)
While a PR is open for this namespace, the controller creates no new one.
*   **Behaviour (grow):** it updates the existing PR with the **currently calculated demand**. The value in the PR may therefore rise (a new burst) or fall (the burst is over, or was deleted) for as long as it has not been merged.
*   **Behaviour (shrink):** an open shrink PR is not updated with new values. If a genuine shortage appears while it is open, the controller closes it (**supersede**) and opens a grow PR instead — an emergency always outranks a proposal to reduce.
*   **Reason:** avoids conflicts and race conditions.

### D. Shrink Gates
A reduction is guarded considerably more carefully than a grow. All four gates have to hold simultaneously, otherwise nothing happens — the controller still computes the shrink candidate and exports it as a metric (section 7):

| Gate (metric label) | Condition | Typical cause when blocked |
|---|---|---|
| `enabled` | `--enable-shrink` is set and the namespace has not opted out with `resizer.io/shrink-enabled: "false"` | Rollout not yet enabled, or a deliberate opt-out |
| `window` | The observation window is gap-free across `window-days` days (default 14) for this resource | The controller has not been running for 14 days yet, or had a downtime > 1h on one day |
| `recent-grow` | No grow happened within the window | The quota grew recently — the reduction waits out one window |
| `cooldown` | The last shrink is further back than `shrink-cooldown-days` (default 7 days) | A previous shrink PR was recently merged, closed, or rejected by a human |

Two effects that look surprising at first but are correct:

*   When a human rejects a shrink PR (closes it without merging), the controller sets `resizer.io/last-shrink` to now. `resizer_shrink_blocked_by{gate="cooldown"}` then reads `1` for the full shrink cooldown — that is not a stuck gauge, it is the rejection being respected.
*   While a gate blocks a shrink, `resizer_quota_target` and `resizer_quota_waste_ratio` keep reporting the uncapped target anyway — the same number every evaluated resource gets, whether it turns into a capped shrink, a grow, or no action at all. That is not a bug; it is exactly what makes the dry-run rollout in section 7 observable in the first place.

### E. Further Safety Guarantees

*   **Shrink PRs are never merged automatically** — regardless of `--enable-auto-merge` and the `resizer.io/auto-merge` annotation. A reduction is always a deliberate human decision.
*   **A PR with no recognisable direction label counts as a grow; a PR whose label is not unambiguously `grow` counts as a shrink** (see [ARCHITECTURE.md](ARCHITECTURE.md) section 3.5). In case of doubt that costs one extra review round instead of an unreviewed merge.
*   **A shrink is never proposed from a failed event scan** (see 2.B).
*   **A genuine emergency closes an open shrink PR and takes over** (see 4.C, supersede).

## 5. Troubleshooting Guide

### Scenario: "My deployment is stuck, but no PR arrives."

1.  **Check the logs:**
    ```bash
    kubectl logs -n namespace-resizer-system -l control-plane=controller-manager
    ```
2.  **Look for keywords:**
    *   `"Skipping resize due to cooldown"` -> wait, or shorten the cooldown via annotation.
    *   `"Quota file not found"` -> the controller cannot find the file in Git (check the path configuration).
    *   `"PR is open"` -> a PR already exists, check GitHub.

### Scenario: "The PR is far too high!"

*   Check whether there were massive bursts in the last hour (many pods failing at once).
*   The controller sums the demand of all workloads failing *simultaneously*.

### Scenario: "A quota is obviously over-provisioned, but no shrink PR arrives."

1.  First check whether `--enable-shrink` is set at all (see section 7) — without the flag no shrink PRs are created, only metrics.
2.  Check `resizer_shrink_blocked_by{namespace="...",quota="..."}` for all four gate values (section 4.D). A value of `1` marks the blocking gate.
3.  The most common one is `window`: a controller restart or a downtime longer than an hour on a given day invalidates that day for the observation window.
4.  **A blocked `window` gate cannot be fixed by editing the Lease annotation on a running controller.** The controller keeps each quota's observation window in process memory and reads the Lease for it only once; a manual change to `resizer.io/observation-window` on a Lease that has already been observed goes unnoticed by the running controller and has no effect until the controller pod restarts. Two things do work instead: restarting the controller pod, which reads the Lease afresh, or deleting and recreating the ResourceQuota — the new UID discards the previous window on the next reconcile, because stored history must not be applied to a different object. Recreating costs the entire observation so far, though: the window starts from zero.

## 6. Configuration (annotations)

Values can be adjusted per namespace — the full table with every annotation, its default, and the migration from `threshold`/`increment` to `headroom` is in [INSTALLATION.md](INSTALLATION.md).

```yaml
metadata:
  annotations:
    resizer.io/cpu-headroom: "0.4"        # 40% buffer instead of the 25% default
    resizer.io/tolerance: "0.1"           # tighter tolerance band
    resizer.io/cooldown-minutes: "30"     # only a 30min grow pause
    resizer.io/shrink-enabled: "false"    # this namespace stays exempt from reductions
    resizer.io/auto-merge: "true"         # (optional) merge grow PRs automatically; never applies to shrink
```

## 7. Monitoring, Metrics and Rollout

### Prometheus Metrics

The controller exposes Prometheus metrics when the `--metrics-bind-address` flag is set (`:8443`, for instance). These metrics are **essential** for observing the shrink path, particularly during the flag-off rollout:

*   **`resizer_quota_target`**: the uncapped target computed in section 3 for each quota resource (in milli units) — not the single-step-capped value that is actually proposed by PR (`max-shrink-step`). Reported for every resource the controller evaluates, including one that currently sits inside the tolerance band or whose shrink a gate is blocking (see 4.D).
*   **`resizer_quota_waste_ratio`**: ratio of the current hard limit to that uncapped target — not to the capped shrink candidate. This lets the value distinguish a 4× from a 40× over-provisioned quota reliably; against the capped candidate both would saturate at the same number (`hard / (hard × 0.75) ≈ 1.33`). A value near `1` means the quota already tracks demand closely; a value well above `1` marks over-provisioning.
*   **`resizer_shrink_blocked_by{gate}`**: which gate (`enabled`, `window`, `recent-grow`, `cooldown`) is currently blocking a shrink (1 = blocked, 0 = not blocked). After a rejected PR it stays at `cooldown=1` for the full cooldown, as expected (see 4.D).
*   **`resizer_decision_total`**: counter of sizing decisions per direction (`grow`/`shrink`/`none`).

### Rollout: From Dry Run to Active Shrinking

The feature ships with shrinking **off**: `--enable-shrink` defaults to `false`, and `config/manager/manager.yaml` ships it commented out accordingly. Until the flag is set, the controller still computes shrink decisions in full and exports them through the metrics above and nowhere else — no PR is created. This mirrors the "observer mode" from [ARCHITECTURE.md](ARCHITECTURE.md) section 3.1, with which the project already built confidence in the grow path.

Recommended sequence for enabling the flag with a clear conscience:

1.  Deploy without `--enable-shrink` and with `--metrics-bind-address=:8443` set, so the metrics are available.
2.  Watch `resizer_quota_waste_ratio` for at least one full observation window (14 days by default). The value is computed against the uncapped target rather than against the 25 % step cap of a single shrink PR, and therefore stays meaningful across the whole range of over-provisioning. A value near `1` means the quota already tracks demand; a value above `2` marks a namespace worth reducing — including one that will only be reduced over several successive PRs (see section 3.4 of the design document).
3.  Check `resizer_shrink_blocked_by{gate="window"}`. If it stays at `1` permanently, the controller is being restarted too often for a window to complete — fix that before enabling shrinking.
4.  Enable `--enable-shrink`. Expect the first shrink PRs within a day, at most one per quota, each capped at a 25 % reduction.
5.  To exempt individual namespaces, annotate them with `resizer.io/shrink-enabled: "false"` — this works regardless of the global flag's state and stays in effect after it is enabled (see [INSTALLATION.md](INSTALLATION.md)).

Shrink PRs are never merged automatically and expire after 7 days without review (`resizer.io/shrink-pr-ttl-days`); a genuine emergency arising in the meantime closes an open shrink PR early and replaces it with a grow PR (supersede, see section 4.C).
