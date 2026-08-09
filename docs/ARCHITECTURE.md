# Architecture: Namespace Resizer Controller

## 1. Overview
The controller's job is to watch Kubernetes `ResourceQuota` objects and to propose — or set — new limits proactively when observed demand moves away from the current limit.

## 2. Phase 1: Detection & Calculation

This phase is purely about the logic: "when do we need to act?" and "what is the new target?".

### 2.1. Detection

The controller runs a **reconciliation loop** that watches `ResourceQuota` resources.

**Data sources:**
- `ResourceQuota.status.hard`: the configured limit.
- `ResourceQuota.status.used`: current consumption.
- The observation window (daily peaks over the last `window-days` days, see
  section 4 of the [design document](design/2026-08-08-quota-rightsizing.md)).

**Trigger logic:**
There is no isolated utilisation threshold any more. For every resource the
controller computes a target (formula in 2.2) and compares it against a
tolerance band around the current limit:

$$ \text{Target} > \text{hard} \times (1 + \text{Tolerance}) \implies \text{Grow} $$

$$ \text{Target} < \text{hard} \times (1 - \text{Tolerance}) \implies \text{Shrink candidate} $$

$$ \text{otherwise} \implies \text{no action} $$

*Example (default values: headroom 25 %, tolerance 15 %):*
- Limit: 10 CPU
- Consumption: 8.5 CPU (85 % utilisation)
- Target: $8.5 \times 1.25 = 10.625$
- Grow threshold: $10 \times 1.15 = 11.5$ — the target is below it.
- Shrink threshold: $10 \times 0.85 = 8.5$ — the target is above it.
- **Result:** no trigger. Under the earlier, purely threshold-based logic the
  same utilisation (85 % ≥ 80 %) would still have triggered an increase — this
  is the most visible behaviour change in this model, see
  [OPERATIONS.md](OPERATIONS.md) section 2.

  With these defaults the target only crosses the grow threshold at roughly
  92 % utilisation (9.3 CPU, for instance: target $9.3 \times 1.25 = 11.625 >
  11.5$ → trigger).

### 2.2. Calculation

Once the trigger fires, the new value has to be computed. Several strategies are needed here to prevent flapping and uncontrolled growth.

**Calculation model:**

$$ \text{Target} = \max(\text{Peak}_{\text{window}}, \text{Used}) \times (1 + \text{Headroom}) $$

Action is taken only outside a tolerance band around this target. For details,
guardrails and rollout, see the
[design document](design/2026-08-08-quota-rightsizing.md).

*Note:* there is no fixed `MaxAllowedLimit` per namespace — not in order to
permit unbounded growth, but because observed demand is itself the upper
bound. The limit follows demand in both directions.

**Parameters:**
1.  **Headroom**: buffer above observed demand (default: 0.25, i.e. 25 %).
2.  **Tolerance**: tolerance band around the target, which rules out flapping structurally (default: 0.15, i.e. 15 %).

The earlier parameters `Threshold` and `IncrementFactor` still work as
annotations and are mapped internally onto `Headroom` — details and the
migration table are in [INSTALLATION.md](INSTALLATION.md).

### 2.4. Guardrails

Growth is still subject to a cooldown:

1.  **Cooldown period:**
    After an adjustment (or a recommendation), no further increase may happen for a defined period (default 60 minutes).
    *Parameter:* `resizer.io/cooldown-minutes`

For a reduction (shrink) a cooldown alone is not enough — there it is only one
of several gates, **all** of which have to hold before a shrink PR is created:

| Gate | Condition |
|---|---|
| `enabled` | Shrinking is enabled globally (`--enable-shrink`) and the namespace has not opted out with `resizer.io/shrink-enabled: "false"` |
| `window` | The observation window is gap-free across the configured window length (default 14 days) for this resource |
| `recent-grow` | No grow happened within the window |
| `cooldown` | The last shrink is further back than the shrink cooldown (default 7 days) |
| `lock` (implicit) | No open PR for this quota — follows from the existing Lease lock (see 3.3) |

Which gate blocks is exported as a Prometheus metric, not as a PR (see
[OPERATIONS.md](OPERATIONS.md)). Two further protections are built directly
into the target formula and need no gate of their own: a hard floor (the target
never falls below current demand) and a step cap (at most a 25 % reduction per
PR). Details:
[design document, section 3.3](design/2026-08-08-quota-rightsizing.md#33-shrink-gates).

### 2.5. Configuration (Policy & Scope)

The controller works on an **opt-out** basis. By default it watches **every namespace** in the cluster.

**1. Global defaults:**
The controller starts with global default values (configurable via CLI flags or a ConfigMap), for example:
*   Headroom: 0.25 (25 %)
*   Tolerance: 0.15 (15 %)
*   Cooldown: 60m

The older parameters `Threshold` and `Increment` are no longer defaults in
their own right but migration inputs: setting them still works, and they are
converted internally into a headroom value rather than driving any logic of
their own (see 2.2 and the migration table in
[INSTALLATION.md](INSTALLATION.md)).

**2. Namespace overrides (annotations):**
Individual namespaces can override these values or exclude themselves from resizing entirely.

*   **Disable (opt-out):**
    ```yaml
    metadata:
      annotations:
        resizer.io/enabled: "false"
    ```

*   **Adjust parameters:**
    ```yaml
    metadata:
      annotations:
        resizer.io/cpu-headroom: "0.4"      # more buffer for CPU
        resizer.io/tolerance: "0.1"         # tighter tolerance band
    ```

*Recommendation for phase 1:* implement the annotation logic. CRDs are not needed for now.

### 2.6. Handling Large Deployments (burst scenarios)

Purely metric-based resizing (`used / hard`) has a weakness: when a large deployment is rolled out that blows past the quota immediately, the deployment fails ("Pending" or "FailedCreate") and the `used` value stays pinned at the limit (100 %). A blanket increase of 20 % may then not be enough to let the deployment through.

**Solution: event-driven resizing**

Alongside monitoring the `ResourceQuota` objects, the controller watches Kubernetes **events** in the namespace.

1.  **Trigger:** look for events of type `Warning` with reason `FailedCreate`.
    *   **Sources:** this covers Pods, ReplicaSets (Deployments), **StatefulSets**, **DaemonSets** and **Jobs** (created by **CronJobs**).
    *   **Filter:** the message has to contain fragments such as "exceeded quota" or "forbidden".
2.  **Analysis:** these error messages often carry precise information about the shortfall.
    *   *Example:* "exceeded quota: my-quota, requested: cpu=5, used: cpu=8, limited: cpu=10".
    *   *Conclusion:* we need 5 CPUs but only have 2 left (10-8). Deficit = 3 CPUs.
3.  **Reaction (deficit filling):**
    Instead of the standard increase (20 %, say), the controller computes what is *minimally* required.

### 2.7. Event Deduplication & Stale Events

A critical problem with event-driven resizing is double counting of old events.
*Scenario:* a deployment fails (event A). The controller raises the quota. The deployment starts. The cooldown expires. But event A still exists (K8s events live for 1h by default). On the next pass the controller would see event A again and wrongly conclude the problem persists.

**Solution: last-modified timestamp in a persistent Lease**
The controller stores the time of the last successful change (PR creation or merge) in its **state object (Lease)** (see 3.3).

*   **Location:** annotation `resizer.io/last-modified` on the `Lease` object in the `namespace-resizer-system` namespace.
*   **Logic:** when analysing events, every event whose `LastTimestamp` is **older** than `resizer.io/last-modified` is ignored.

This guarantees that each event leads to exactly one action.

## 3. GitOps Compatibility & Execution Strategy

The approach is strictly **GitOps first**: cluster state (ResourceQuotas) should ideally always be in sync with the Git repository.

### 3.1. Phase 1: "Observer Mode" (log & recommend)
In the first implementation phase the controller makes **no changes** to the cluster or to Git. It acts purely as an observer.

**Behaviour:**
1.  Detect shortages (metric or event).
2.  Compute the required new limit (including guardrails).
3.  **Action:**
    *   Structured **log output** (JSON) that external tools could parse.
    *   A **Kubernetes event** on the ResourceQuota object (e.g. `Type: Warning, Reason: QuotaResizeRecommended, Message: "CPU limit should be increased to 12"`).

This lets the calculation logic be tested safely in real environments while confidence is built.

### 3.2. Phase 2: Git Integration (GitHub provider)
Rather than patching the quota in the cluster — which would conflict with ArgoCD/Flux — the controller interacts with source control directly in a later phase.

**Planned workflow:**
1.  The controller detects demand.
2.  The controller authenticates against GitHub (token/app).
3.  The controller locates the file defining the `ResourceQuota`.
4.  The controller opens a **pull request** with the change.

*Note:* the "direct patch" strategy (formerly strategy A) is skipped so as not to violate GitOps principles.

### 3.3. State Management & Locking (persistent Leases)

Two things need a mechanism:
1.  **Locking:** prevent opening several PRs at once for the same namespace.
2.  **State:** record when we last acted (for event deduplication), since the quota object itself cannot be modified (a GitOps sync would overwrite it).

Native Kubernetes `Lease` objects (`coordination.k8s.io/v1`) are used for this.

**Location:**
The Leases live in the **controller's namespace** (e.g. `namespace-resizer-system`).
*Reason:* GitOps tools (ArgoCD/Flux) would prune Leases in the target namespace, since they are not defined in Git.

**Naming convention:**
`state-<target-namespace>-<quota-name>`

**Workflow:**

1.  **Check (local):**
    The controller loads the Lease for the target quota.

2.  **Case A: the Lease has a holder (lock active)**
    *   Wait (or update the PR, see 3.4).

3.  **Case B: the Lease has no holder (lock free)**
    *   Check `Annotations["resizer.io/last-modified"]`.
    *   Are the events newer than that timestamp?
        *   **Yes:** take the lock (set `HolderIdentity`), create the PR, update the timestamp.
        *   **No:** ignore the events (an old problem).

**Important:** the Lease is **not deleted** when the PR is merged. Only the `HolderIdentity` is removed (unlock), so the state (timestamp) survives.

### 3.3.1. Garbage Collection (Lease cleanup)

Since a persistent Lease object is created in the controller namespace for every namespace, orphaned Leases could accumulate over time (when a namespace is deleted, for instance). To keep the Kubernetes API tidy, the controller runs a garbage collection routine.

**Mechanism:**
*   A background goroutine runs periodically (every 12 hours, say).
*   It lists all Leases in the controller namespace carrying the label `app.kubernetes.io/managed-by=namespace-resizer`.
*   It extracts the target namespace from the Lease name.
*   It checks whether that target namespace still exists in the cluster.
*   If the namespace is gone, the Lease object is deleted.

### 3.4. Auto-Merge Strategy

To close the loop and allow fully automatic operation, the controller can merge pull requests itself, provided certain criteria are met.

**Configuration:**
1.  **Global:** the controller has an `--enable-auto-merge` flag (default: `false`).
2.  **Namespace (opt-out):** when enabled globally, the behaviour can be disabled per namespace:
    `resizer.io/auto-merge: "false"`

*Logic:*
*   Global `false`: auto-merge is always off (safety net).
*   Global `true` + annotation `false`: auto-merge is off for this namespace.
*   Global `true` + no annotation: auto-merge is **on**.

**Preconditions for auto-merge:**
On each reconcile loop (when a lock/PR exists) the controller checks the PR's status in GitHub:
1.  **Mergeable:** GitHub reports no conflict (`mergeable: true`).
2.  **CI checks:** `MergeableState` has to be `clean` (all required status checks passed).
3.  **State:** the PR has to be open.

**Sequence:**
1.  The controller finds an active lock & PR.
2.  The controller queries the PR status via the GitHub API.
3.  If `auto-merge: "true"` AND the preconditions hold:
    *   The controller merges (squash merge preferred).
    *   The controller releases the lock (Lease).
4.  If the preconditions do not hold (CI still running, say):
    *   The controller waits (requeue).

**Safety:**
*   Race conditions (merge vs ArgoCD sync) are absorbed by the controller's idempotence. After the merge the quota in the cluster stays briefly "too low" until ArgoCD syncs. The controller still sees the demand but no longer finds an open PR (it was merged). In theory it would want to create a new PR, but we can check whether the last merge was recent (cooldown) or whether the repository's head commit already contains the change.
*   Alternatively, the controller simply waits. Once ArgoCD syncs, the "threshold exceeded" condition disappears.

### 3.5. Direction, Supersede and TTL

Every PR the controller creates carries its direction in three places: the
branch name, a GitHub label (`resizer/direction:grow` or
`resizer/direction:shrink`), and the direction field on the state Lease
(`resizer.io/pr-direction`). Direction has to survive so that orphan recovery
(`FindOpenPR`) can classify a pull request it did not open itself — without it,
an orphaned shrink PR could be adopted as a grow and potentially auto-merged.

**The branch name is the authoritative source.** New branches are named
`resize/<direction>/<namespace>/<quota>/<timestamp>`. The branch is created in
the same call as the pull request and cannot fail separately, so the direction
cannot be lost the way a label can when attaching it fails. Because a
Kubernetes namespace or quota name can never contain `/`, this shape is also
unambiguous: it can never collide with a branch belonging to a different
namespace/quota pair.

**The label is the fallback**, for pull requests created before the branch
encoding existed, and it is deliberately asymmetric in how it is read. A PR
carrying no direction label at all counts as `grow` — this preserves the
behaviour of PRs opened before the label existed. A PR carrying a label that is
not exactly `grow` (an unknown value, a typo, several direction labels at once)
counts as `shrink`. Labels are writable by anyone with repository access; a PR
that already carries a label but is not unambiguously `grow` costs at worst one
extra review round, whereas the alternative would be an unreviewed merge of a
lowered limit.

**Auto-merge applies exclusively to `grow`**, regardless of
`--enable-auto-merge` and the `resizer.io/auto-merge` annotation.

**Supersede:** if the controller detects a genuine emergency while a shrink PR
is open (the decision becomes a grow), it closes the shrink PR with an
explanatory comment, sets `resizer.io/last-shrink`, and opens the grow PR on
the next reconcile.

**TTL:** a shrink PR left open without review for `resizer.io/shrink-pr-ttl-days`
(default 7 days) is closed automatically with a comment, likewise updating
`resizer.io/last-shrink` — without that stamp the next reconcile would
immediately reopen the same PR.

Details: [design document, section 6](design/2026-08-08-quota-rightsizing.md#6-pr-lifecycle).
