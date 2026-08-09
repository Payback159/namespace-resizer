# Project Plan: Namespace Resizer

## Phase 1: Planning & Architecture (Completed)
- [x] Draft the architecture for detection & calculation (`ARCHITECTURE.md`)
- [x] Decision: configuration via annotations (to start) vs CRD
- [x] Concept: event-driven resizing for burst scenarios (Deployments, StatefulSets, Jobs)
- [x] Concept: GitOps strategy (phase 1: observer mode, phase 2: GitHub PRs)

## Phase 2: Implementation (Observer Mode) (Completed)
- [x] Set up the controller skeleton (Go, Kubebuilder)
- [x] **Module 1: metric observer**
    - [x] Watcher for ResourceQuotas
    - [x] Calculation: `used / hard` vs threshold
- [x] **Module 2: event observer**
    - [x] Watcher for events (`FailedCreate`)
    - [x] Parser for error messages ("requested: x, used: y")
- [x] **Module 3: policy & calculation**
    - [x] Increment logic
- [x] **Module 4: reporter**
    - [x] Structured logging of the recommendation
    - [x] Kubernetes events

## Phase 3: GitOps & Locking (Completed)
- [x] GitHub integration (PR creation)
- [x] Locking mechanism (K8s Leases)
- [x] Stale event prevention
- [x] Zombie lock prevention

## Phase 4: Stability & Cooldown (In Progress)
- [x] Cooldown mechanism (K8s Leases)
- [x] Configuration via annotation (`resizer.io/cooldown-minutes`)

## Phase 5: Deployment
- [x] Kustomize manifests (`config/`)
- [x] Static install manifest (`dist/install.yaml`)
- [x] CI/CD pipeline for releases (`.github/workflows/release.yml`)
- [x] Update documentation (installation, configuration)

## Phase 6: Advanced GitOps (Auto-Merge) (Completed)
- [x] Configuration: define the `resizer.io/auto-merge` annotation
- [x] Extend the GitHub provider:
    - [x] `GetPRStatus` (mergeable, checks status)
    - [x] `MergePR` (squash merge)
- [x] Controller logic:
    - [x] Check PR status in the reconcile loop
    - [x] Merge once the conditions are met
- [x] Tests for the auto-merge logic

## Phase 7: Future Work
- [x] Metrics export (Prometheus)
- [ ] Validating webhook

## Phase 8: Bidirectional Quota Rightsizing (Completed)

Details and derivation: [design document](design/2026-08-08-quota-rightsizing.md).

**Stage 1 — observation and decision, without shrink PRs**
- [x] `internal/sizing`: rolling-window encoding, coverage check, target formula, shrink gates, config migration (`threshold`/`increment` → `headroom`)
- [x] Fix for countable resources (e.g. `pods`) landing on fractional targets
- [x] Sampling into the state Lease, write-sparing
- [x] Controller moved onto `sizing.Decide`; the old threshold path removed, grow behaviour preserved through migration fallbacks
- [x] Prometheus metrics (`resizer_quota_target`, `resizer_quota_waste_ratio`, `resizer_shrink_blocked_by`, `resizer_decision_total`)

**Stage 2 — shrink PRs**
- [x] `ClosePR` on `git.Provider`, `FindOpenPR` extended with the direction, direction label on creation
- [x] Direction state in the Lease, auto-merge only for `grow`
- [x] Supersede (a shortage closes an open shrink PR) and TTL (an unreviewed shrink PR expires)
- [x] `--enable-shrink` flag (off by default), `resizer.io/shrink-enabled` as a pure opt-out
- [x] envtest and E2E coverage, documentation (`ARCHITECTURE.md`, `INSTALLATION.md`, `OPERATIONS.md`, `README.md`)
