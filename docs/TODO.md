# Projekt Plan: Namespace Resizer

## Phase 1: Planung & Architektur (Abgeschlossen)
- [x] Architektur-Entwurf für Erkennung & Berechnung erstellen (`ARCHITECTURE.md`)
- [x] Entscheidung: Konfiguration via Annotations (Start) vs CRD
- [x] Konzept: Event-Driven Resizing für Burst-Szenarien (Deployments, StatefulSets, Jobs)
- [x] Konzept: GitOps-Strategie (Phase 1: Observer Mode, Phase 2: GitHub PRs)

## Phase 2: Implementierung (Observer Mode)
- [ ] Skeleton des Controllers aufsetzen (Sprache: Go? Framework: Kubebuilder?)
- [ ] **Modul 1: Metrik-Beobachter**
    - [ ] Watcher für ResourceQuotas
    - [ ] Berechnung: `used / hard` vs Threshold
- [ ] **Modul 2: Event-Beobachter**
    - [ ] Watcher für Events (`FailedCreate`)
    - [ ] Parser für Fehlermeldungen ("requested: x, used: y")
- [ ] **Modul 3: Policy & Berechnung**
    - [ ] Logik für Increment, Cooldown, Max Step
- [ ] **Modul 4: Reporter (statt Aktor)**
    - [ ] Strukturiertes Logging der Empfehlung
    - [ ] Emittieren von Kubernetes Events (`QuotaResizeRecommended`)

## Phase 3: Git Integration (GitHub)
- [ ] GitHub Client Integration
- [ ] Logik zum Finden der Quota-Datei im Repo
- [ ] Erstellen von Pull Requests mit neuen Werten
