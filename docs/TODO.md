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
- [ ] Helm Chart erstellen
- [ ] CI/CD Pipeline für Releases
- [ ] Dokumentation aktualisieren (Installation, Konfiguration)

## Phase 6: Future Work
- [ ] Metrics Export (Prometheus)
- [ ] Webhook für Validierung
