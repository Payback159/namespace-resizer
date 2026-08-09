# Projekt Plan: Namespace Resizer

## Phase 1: Planung & Architektur (Abgeschlossen)
- [x] Architektur-Entwurf für Erkennung & Berechnung erstellen (`ARCHITECTURE.md`)
- [x] Entscheidung: Konfiguration via Annotations (Start) vs CRD
- [x] Konzept: Event-Driven Resizing für Burst-Szenarien (Deployments, StatefulSets, Jobs)
- [x] Konzept: GitOps-Strategie (Phase 1: Observer Mode, Phase 2: GitHub PRs)

## Phase 2: Implementierung (Observer Mode) (Abgeschlossen)
- [x] Skeleton des Controllers aufsetzen (Go, Kubebuilder)
- [x] **Modul 1: Metrik-Beobachter**
    - [x] Watcher für ResourceQuotas
    - [x] Berechnung: `used / hard` vs Threshold
- [x] **Modul 2: Event-Beobachter**
    - [x] Watcher für Events (`FailedCreate`)
    - [x] Parser für Fehlermeldungen ("requested: x, used: y")
- [x] **Modul 3: Policy & Berechnung**
    - [x] Logik für Increment
- [x] **Modul 4: Reporter**
    - [x] Strukturiertes Logging der Empfehlung
    - [x] Kubernetes Events

## Phase 3: GitOps & Locking (Abgeschlossen)
- [x] GitHub Integration (PR Erstellung)
- [x] Locking Mechanismus (K8s Leases)
- [x] Stale Event Prevention
- [x] Zombie Lock Prevention

## Phase 4: Stabilität & Cooldown (In Arbeit)
- [x] Cooldown Mechanismus (K8s Leases)
- [x] Konfiguration via Annotation (`resizer.io/cooldown-minutes`)

## Phase 5: Deployment
- [x] Kustomize Manifests (`config/`)
- [x] Static Install Manifest (`dist/install.yaml`)
- [x] CI/CD Pipeline für Releases (`.github/workflows/release.yml`)
- [x] Dokumentation aktualisieren (Installation, Konfiguration)

## Phase 6: Advanced GitOps (Auto-Merge) (Abgeschlossen)
- [x] Konfiguration: Annotation `resizer.io/auto-merge` definieren
- [x] GitHub Provider erweitern:
    - [x] `GetPRStatus` (Mergeable, Checks Status)
    - [x] `MergePR` (Squash Merge)
- [x] Controller Logik:
    - [x] Im Reconcile-Loop PR-Status prüfen
    - [x] Merge ausführen wenn Conditions met
- [x] Tests für Auto-Merge Logik

## Phase 7: Future Work
- [ ] Metrics Export (Prometheus)
- [ ] Webhook für Validierung

## Phase 8: Bidirektionales Quota-Rightsizing (Abgeschlossen)

Details und Herleitung: [Design-Dokument](design/2026-08-08-quota-rightsizing.md).

**Stufe 1 — Beobachtung und Entscheidung, ohne Shrink-PRs**
- [x] `internal/sizing`: Rolling-Window-Kodierung, Abdeckungsprüfung, Zielformel, Shrink-Gates, Config-Migration (`threshold`/`increment` → `headroom`)
- [x] Fix für zählbare Ressourcen (z.B. `pods`) bei krummen Zielwerten
- [x] Sampling ins State-Lease, schreibsparsam
- [x] Controller auf `sizing.Decide` umgestellt; alter Threshold-Pfad entfernt, Grow-Verhalten durch Migrations-Fallbacks unverändert
- [x] Prometheus-Metriken (`resizer_quota_target`, `resizer_quota_waste_ratio`, `resizer_shrink_blocked_by`, `resizer_decision_total`)

**Stufe 2 — Shrink-PRs**
- [x] `ClosePR` am `git.Provider`, `FindOpenPR` um Richtung erweitert, Richtungs-Label beim Erstellen
- [x] Richtungs-Zustand im Lease, Auto-Merge nur bei `grow`
- [x] Supersede (Notlage schließt offenen Shrink-PR) und TTL (unreviewter Shrink-PR läuft ab)
- [x] `--enable-shrink`-Flag (Default aus), `resizer.io/shrink-enabled` als reiner Opt-out
- [x] envtest- und E2E-Abdeckung, Dokumentation (`ARCHITECTURE.md`, `INSTALLATION.md`, `OPERATIONS.md`, `README.md`)
