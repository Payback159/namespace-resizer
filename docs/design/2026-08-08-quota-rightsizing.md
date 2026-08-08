# Design: Bidirektionales Quota-Rightsizing

**Datum**: 2026-08-08
**Status**: Entwurf, freigegeben
**Betrifft**: `internal/controller`, `internal/lock`, `internal/git`, `internal/config`

## 1. Problem

Die heutige Implementierung kann Quotas ausschließlich vergrößern. Beide Pfade,
die `recommendations` befüllen — die Metrik-Analyse in `calculateRecommendations`
und die Event-Analyse in `analyzeEvents` — erzeugen nur höhere Werte;
`analyzeEvents` filtert kleinere Werte sogar explizit heraus
(`resourcequota_controller.go:482`). `ARCHITECTURE.md:38` hält das als
Entwurfsentscheidung fest: kein `MaxAllowedLimit`, damit Wachstum über den
gesamten Lebenszyklus möglich bleibt.

Daraus folgen vier Effekte, die Ressourcen verschwenden:

1. **Peaks werden zementiert.** Ein einmaliges Ereignis — Rolling Update mit
   `maxSurge`, ein Batch-Job, ein fehlgeschlagener Rollout — hebt das Quota
   dauerhaft an. Es gibt keinen Mechanismus, der es je wieder senkt.
2. **Der Metrik-Pfad triggert ohne echten Bedarf.** Bei 80 % Auslastung wurde
   noch kein Pod abgelehnt, trotzdem wird `hard` um 20 % erhöht. Die
   Entscheidung fällt auf Basis eines einzigen Sample-Zeitpunkts, ohne
   Betrachtung, ob die Auslastung anhält.
3. **`used` misst Requests, nicht Verbrauch.** Über-requestende Workloads
   führen dazu, dass der Controller Quota für Luft vergrößert, die nie genutzt
   wird. Schlechtes Request-Sizing wird belohnt statt sichtbar gemacht.
4. **Puffer auf Puffer.** Im Event-Pfad gilt
   `total = (used + deficit) × (1 + increment)`
   (`resourcequota_controller.go:470-476`) — der Aufschlag wird auf die neue
   Gesamtsumme gerechnet, nicht auf das Defizit. Wiederholte Bursts
   kompoundieren.

## 2. Ziel

Das Quota folgt dem tatsächlich beobachteten Bedarf mit definiertem Headroom —
in **beide** Richtungen. Der Controller erhöht bei echtem Engpass schnell und
baut Überdimensionierung langsam und reviewbar zurück.

Bedarf wird dabei als `quota.status.used` definiert, also als Summe der
**Requests**. Das ist die Größe, an der Admission scheitert: Ein Quota unterhalb
der Requests-Summe lehnt Pods ab, unabhängig davon, wie wenig CPU real
verbraucht wird. Echter Verbrauch (metrics-server, Prometheus) ist ein
wertvolles Reporting-Signal, aber keine sichere Basis fürs Verkleinern, und
bleibt außerhalb des Scopes dieses Designs.

## 3. Regelmodell

Eine einzige Zielformel regelt beide Richtungen. Der bisherige
Threshold-Pfad (`used/hard ≥ 80 % → hard × 1.2`) entfällt ersatzlos.

```
für jede Resource in quota.status.hard:
    peak   = max( Tages-Peaks über alle abgedeckten Tage , used_now )
    peak   = max( peak , used_now + deficit )                 # Event-Beschleuniger
    target = peak × (1 + headroom)
    target = max( target , used_now × (1 + headroom) , <res>-min )   # harter Boden

    target > hard × (1 + tolerance)  →  Grow-Kandidat: target
    target < hard × (1 - tolerance)  →  Shrink-Kandidat: max(target, hard × (1 - maxShrinkStep))
    sonst                            →  keine Aktion
```

`deficit` ist das aus `FailedCreate`-Events berechnete Defizit für diese
Resource (heutige `calculateWorkloadDeficit`-Logik), oder `0`, wenn kein
aktuelles Event vorliegt. `<res>-min` ist die optionale Untergrenzen-Annotation
aus 7.2, oder `0`, wenn nicht gesetzt. Der laufende, noch unvollständige Tag
zählt nicht zu den Tages-Peaks — er ist über `used_now` bereits abgedeckt.

### 3.1 Richtungsentscheidung

**Grow gewinnt immer.** Sobald eine einzige Resource wachsen will, ist die
gesamte Decision ein Grow; alle Shrink-Kandidaten werden verworfen. Ein PR, der
CPU anhebt und gleichzeitig Memory senkt, wäre schwer zu reviewen und würde die
Regel „Shrink nie auto-mergen" unterlaufen.

Ein Shrink entsteht nur, wenn keine Resource wachsen will **und** alle Gates aus
Abschnitt 3.3 halten.

### 3.2 Toleranzband

Bei Headroom 0.25 und Toleranz 0.15 ergibt sich unter konstanter Last
(`peak = used_now = U`, also `target = 1.25 × U`) ein stabiler Korridor:

```
Grow   wenn  1.25 × U > 1.15 × hard   ⟹   hard < 1.087 × U
Shrink wenn  1.25 × U < 0.85 × hard   ⟹   hard > 1.47  × U

stabil: hard ∈ [1.087 × U … 1.47 × U]
```

Innerhalb dieses Bandes passiert nichts. Flapping zwischen Grow und Shrink ist
damit strukturell ausgeschlossen und braucht keine zusätzlichen Sperren.

Eine Konsequenz, die bei der Bewertung von Shrink-Ergebnissen wichtig ist: Das
Band endet **oberhalb** des Zielwerts. Der Rückbau stoppt, sobald
`hard ≤ target / 0.85 = 1.176 × target` erreicht ist, nicht exakt bei `target`.
Ein Restpuffer von bis zu 17,6 % über dem rechnerischen Ziel ist gewolltes
Verhalten, kein Fehler.

### 3.3 Shrink-Gates

Ein Shrink wird nur ausgelöst, wenn **alle** Gates halten:

| Gate | Bedingung |
|---|---|
| `window` | Das Beobachtungsfenster ist über `windowDays` Tage vollständig abgedeckt (siehe 4.2), pro Resource geprüft |
| `recent-grow` | Innerhalb des Fensters fand kein Grow statt (`last-grow` älter als `windowDays`) |
| `cooldown` | `last-shrink` ist älter als `shrinkCooldownDays` |
| `lock` | Kein offener PR für dieses Quota (ergibt sich aus dem bestehenden Lease-Lock) |
| `enabled` | Shrink ist global aktiviert und der Namespace hat nicht `resizer.io/shrink-enabled: "false"` |

Welches Gate blockiert hat, wird in `Decision.BlockedBy` festgehalten und als
Prometheus-Metrik sowie `V(1)`-Log ausgegeben — nicht als PR.

Zwei Schutzmechanismen sind bereits in der Formel aus Abschnitt 3 verankert und
brauchen kein eigenes Gate: der **harte Boden**
(`max(target, used_now × (1 + headroom), min-Annotation)`) und der
**Schritt-Deckel** (`max(target, hard × (1 - maxShrinkStep))`).

### 3.4 Rückbau-Beispiel

Ein vierfach überdimensioniertes Quota (`hard = 16` CPU, `peak₁₄ = 4`,
`used = 3.5`, also `target = 5`). Ein Shrink wird ausgelöst, solange
`hard > target / 0.85 = 5.88`; jeder Schritt ist auf `hard × 0.75` gedeckelt:

```
Runde 1 (Tag  0):  16    → 12       Deckel greift (12 > target 5)
Runde 2 (Tag  7):  12    →  9       Deckel greift
Runde 3 (Tag 14):   9    →  6.75    Deckel greift
Runde 4 (Tag 21):   6.75 →  5.06    Deckel greift
Tag 28:             5.06 <  5.88  →  kein Shrink, Band geschlossen
```

Vier reviewte PRs über drei Wochen; Endwert 5.06 statt exakt 5 — der Restpuffer
aus dem Toleranzband (siehe 3.2). Aus vierfacher Überdimensionierung wird
1,45-fache. Der Ablauf ist jederzeit durch einen echten Engpass unterbrechbar
(siehe 6.2).

## 4. Beobachtung & Datenmodell

### 4.1 Sampling

Bei jedem Reconcile liest der Controller `quota.status.used` und trägt es in den
Tages-Bucket des laufenden Tages ein (Maximum je Resource). Das Fenster ist ein
Ring über `windowDays` Tage, JSON-kodiert in der Annotation
`resizer.io/observation-window` am bestehenden State-Lease
(`state-<namespace>-<quota>` im Controller-Namespace, siehe `ARCHITECTURE.md`
3.3).

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

Werte werden als `resource.Quantity`-Strings gespeichert, damit Format und
Präzision erhalten bleiben. Bei 14 Tagen und einer Handvoll Resources liegt die
Annotation bei etwa 1,7 KB — weit unter dem Kubernetes-Limit von 256 KB für
alle Annotationen eines Objekts zusammen.

**Schreiblast**: Das Lease wird nicht bei jedem Reconcile beschrieben, sondern
nur, wenn ein Peak steigt oder seit dem letzten Write mehr als eine Stunde
vergangen ist. Bei 200 Quotas ergibt das rund 5000 Writes pro Tag
(≈ 0,06/s) — für die API vernachlässigbar.

### 4.2 Vollständigkeit des Fensters

Es genügt **nicht**, zu prüfen, ob 14 Tages-Einträge existieren. Ein Controller,
der jeden Tag nur zehn Minuten lief, hätte 14 Einträge und trotzdem keine
belastbare Historie — und damit einen gefährlich niedrigen Peak.

Deshalb führt der Controller pro Sample fort:

```
maxGap = max(maxGap, now − lastSampleAt)
```

`lastSampleAt` wird dabei über die Tagesgrenze hinweg fortgeschrieben: Eine
Lücke, die über Mitternacht reicht, erhöht `maxGap` im *neuen* Tages-Bucket,
während der alte durch sein `last` auffällt. Beide Tage werden damit korrekt
verworfen.

Ein Tag gilt als **abgedeckt**, wenn `maxGap ≤ 1h`, `first ≤ 00:30` und
`last ≥ 23:30`. Controller-Downtime macht die betroffenen Tage damit
automatisch ungültig, statt einen zu niedrigen Peak vorzutäuschen.

Der laufende Tag ist per Definition nie abgedeckt. Das Fenster gilt als
vollständig, wenn die `windowDays` **abgeschlossenen** Tage vor heute alle
abgedeckt sind.

### 4.3 Resource-Wechsel

Taucht eine Resource neu im Quota auf (z. B. `requests.storage`), fehlt sie in
den älteren Buckets. Das `window`-Gate greift deshalb **pro Resource**:
Geschrumpft wird nur, was über alle abgedeckten Tage hinweg beobachtet wurde.
Für neu hinzugekommene Resources gilt bis zum Ablauf eines vollen Fensters nur
der Grow-Pfad.

## 5. Struktur

Die Logik zieht in ein neues Paket `internal/sizing` ohne Kubernetes-Client.
Der Controller wird zum Orchestrator: beobachten → entscheiden → PR.

```
internal/sizing/
  decide.go     Decide(Input) Decision      — reine Funktion, Clock injiziert
  window.go     Rolling-Window Encode/Decode, Abdeckungsprüfung
  config.go     Annotations → Policy (inkl. Migration aus 7.1)
  deficit.go    Event-Defizit-Berechnung (aus resourcequota_utils.go übernommen)

internal/controller/
  resourcequota_controller.go   nur noch Orchestrierung
  observation.go                Sampling → Lease
```

```go
type Direction int   // None | Grow | Shrink

type Decision struct {
    Direction Direction
    Targets   map[corev1.ResourceName]resource.Quantity
    Reason    string   // strukturierte Begründung, landet im PR-Body
    BlockedBy []Gate   // welche Gates einen Shrink verhindert haben
}
```

Der Grund für diesen Schnitt ist konkret: Die Shrink-Gates sind zeitabhängig
(Fenster vollständig? Cooldown abgelaufen?). Mit injizierter Clock lassen sie
sich als Tabellen-Tests in Millisekunden prüfen — Fälle wie „14 Tage Historie,
davon Tag 9 mit einer 6-Stunden-Lücke" wären gegen einen echten API-Server nicht
praktikabel testbar. Zusätzlich verschwindet die Begründung nicht im Log,
sondern landet strukturiert im PR-Body.

`resourcequota_controller.go` hat heute 498 Zeilen und fünf Verantwortlichkeiten
(Reconcile-Orchestrierung, Metrik-Analyse, Event-Analyse, PR-Lifecycle,
Auto-Merge). Metrik- und Event-Analyse wandern vollständig heraus.

## 6. PR-Lifecycle

### 6.1 Richtungs-Zustand

Das Lease speichert neben der PR-ID die Richtung
(`resizer.io/pr-direction: grow|shrink`). Der PR selbst trägt ein
GitHub-Label `resizer.io/direction` mit demselben Wert.

Das Label ist nicht kosmetisch: `FindOpenPR` — die Orphan-Recovery aus Commit
`ebf581e` — gibt heute nur eine PR-ID zurück. Ohne Richtung würde der Controller
einen verwaisten Shrink-PR als Grow adoptieren und ihn potenziell auto-mergen.
Die Signatur wird auf `(int, Direction, error)` erweitert und liest die Richtung
aus dem Label.

**Auto-Merge greift ausschließlich bei `grow`**, unabhängig von
`--enable-auto-merge` und der `resizer.io/auto-merge`-Annotation.

### 6.2 Supersede

Ist der offene PR ein Shrink und die aktuelle Decision ein Grow:

1. `ClosePR(ctx, prID, comment)` mit erklärendem Kommentar (welche Resource,
   welcher Bedarf, welches auslösende Event).
2. Lock freigeben, `last-shrink = now` setzen.
3. Requeue — der nächste Pass öffnet den Grow-PR.

`ClosePR` ist eine neue Methode am `git.Provider`-Interface.

### 6.3 TTL

Ein Shrink-PR, der `shrinkPrTTL` (Default 7 Tage) ohne Review offen liegt, wird
mit demselben Ablauf geschlossen. Das `last-shrink = now` ist dabei essenziell:
ohne es würde der Controller im nächsten Reconcile sofort denselben PR erneut
öffnen.

### 6.4 Lease-Annotationen

| Annotation | Rolle |
|---|---|
| `resizer.io/last-modified` | unverändert — Event-Deduplizierung |
| `resizer.io/last-grow` | Gate `recent-grow` |
| `resizer.io/last-shrink` | Gate `cooldown` |
| `resizer.io/pr-direction` | Richtung des aktiven PRs |
| `resizer.io/observation-window` | Rolling Window (siehe 4.1) |

## 7. Konfiguration

### 7.1 Migration

`resizer.io/<res>-headroom` ersetzt Threshold und Increment. Fallback-Kette,
damit Bestandsinstallationen ihr Grow-Verhalten **nicht** ändern:

```
*-headroom gesetzt?   → verwenden
sonst *-increment?    → direkt übernehmen (gleiche Semantik: 0.2 → 0.2)
sonst *-threshold?    → ableiten: 100/threshold − 1        (80 → 0.25)
sonst                 → Default 0.25
```

Die alten Annotationen funktionieren weiter und erzeugen ein Deprecation-Log.
Die Konstanten in `internal/config/constants.go` bleiben mit
`// Deprecated:`-Kommentar erhalten.

### 7.2 Parameter

Alle Werte sind global per Flag/ConfigMap gesetzt und pro Namespace via
Annotation überschreibbar.

| Parameter | Annotation | Default |
|---|---|---|
| Headroom | `resizer.io/<res>-headroom` | `0.25` |
| Toleranz | `resizer.io/tolerance` | `0.15` |
| Untergrenze | `resizer.io/<res>-min` | — (Quantity, optional) |
| Fensterlänge | `resizer.io/window-days` | `14` |
| Shrink-Cooldown | `resizer.io/shrink-cooldown-days` | `7` |
| Max. Shrink-Schritt | `resizer.io/max-shrink-step` | `0.25` |
| Shrink-PR-TTL | `resizer.io/shrink-pr-ttl-days` | `7` |
| Grow-Cooldown | `resizer.io/cooldown-minutes` | `60` (unverändert) |
| Shrink-Opt-out | `resizer.io/shrink-enabled` | `true` |

Shrink findet nur statt, wenn **beides** zutrifft: das globale Flag
`--enable-shrink` ist gesetzt (Default `false`, siehe 8) **und** der Namespace
hat sich nicht per `resizer.io/shrink-enabled: "false"` abgemeldet. Die
Annotation ist ein Opt-out innerhalb eines global aktivierten Shrink-Betriebs,
kein Weg, das Flag zu übersteuern.

### 7.3 Bewusst nicht eingeführt

Ein `*-max`-Dach pro Namespace. Die Zielformel begrenzt Wachstum bereits am
beobachteten Bedarf; ein zweiter, konkurrierender Begrenzer wäre Konfiguration
ohne zusätzlichen Nutzen.

`ARCHITECTURE.md:38` wird entsprechend umgeschrieben: nicht mehr „kein Maximum,
damit unbegrenztes Wachstum möglich ist", sondern „kein festes Maximum nötig,
weil der beobachtete Bedarf die Obergrenze bildet".

## 8. Rollout

Flag `--enable-shrink`, Default `false`. Der Controller berechnet
Shrink-Entscheidungen trotzdem vollständig und exportiert sie als Metriken.
Damit ist wochenlang sichtbar, was er täte, bevor der erste PR entsteht — das
Vorgehen entspricht dem „Observer Mode" aus `ARCHITECTURE.md` 3.1, mit dem das
Projekt schon einmal Vertrauen aufgebaut hat.

```
resizer_quota_target{namespace,quota,resource}          # berechneter Zielwert
resizer_quota_waste_ratio{namespace,quota,resource}     # hard / target
resizer_shrink_blocked_by{namespace,quota,gate}         # blockierendes Gate
resizer_decision_total{namespace,quota,direction}       # Counter
```

Namespace-Opt-out über `resizer.io/shrink-enabled: "false"` bleibt auch nach
Aktivierung des Flags bestehen — konsistent mit dem Opt-out-Prinzip des
Controllers.

## 9. Fehlerbehandlung

Leitlinie durchgehend: **im Zweifel nicht schrumpfen.**

| Störung | Verhalten |
|---|---|
| Window-JSON korrupt oder unbekanntes `v` | Fenster als leer behandeln, Sampling neu starten; Gate `window` sperrt Shrink automatisch |
| Controller-Downtime | `maxGap` macht betroffene Tage ungültig → Fenster unvollständig → kein Shrink |
| Lease-Write-Konflikt (optimistic concurrency) | Re-Read + Retry; Muster existiert bereits aus Commit `5d88f39` |
| `ClosePR` schlägt fehl | Lock bleibt bestehen, Requeue — Verzögerung, kein inkonsistenter Zustand |
| GitHub nicht erreichbar | Fehler zurückgeben, controller-runtime Backoff (unverändert) |
| Uhr springt rückwärts | Buckets mit Datum in der Zukunft werden verworfen |
| Quota gelöscht und gleichnamig neu angelegt | `quota.metadata.uid` wird im Fenster mitgeführt; bei Änderung wird das Fenster zurückgesetzt |

### 9.1 Latenter Bug: Objektzähler

`convertToReadableFormat` (`resourcequota_utils.go:217`) führt alles außer
Memory und Storage über `resource.NewMilliQuantity`. Für zählbare Ressourcen wie
`pods` oder `count/deployments.apps` erzeugt das bei nicht-glatten Werten
ungültige Quantities: Aus einem Ziel von 11,25 Pods wird `"11250m"`, was
Kubernetes als Pod-Quota ablehnt.

Heute fällt das kaum auf, weil `hard × 1.2` bei ganzzahligen Ausgangswerten
meist wieder glatt aufgeht. Die neue Zielformel erzeugt deutlich häufiger krumme
Werte, deshalb gehört der Fix in dieses Design: ein dritter Zweig, der für
zählbare Ressourcen auf ganze Zahlen aufrundet.

## 10. Tests

**`internal/sizing` — Tabellen-Tests mit injizierter Clock:**

- Zielformel je Resource-Typ: CPU (milli), Memory (BinarySI), Objektzähler
  (ganzzahlig, siehe 9.1)
- Toleranzband: Werte knapp innerhalb und außerhalb beider Grenzen
- Harter Boden: `used_now` und `*-min` überstimmen einen niedrigeren Peak
- Schritt-Deckel: Ziel weit unter `hard × 0.75`
- Jedes Gate aus 3.3 einzeln blockierend
- Grow schlägt Shrink bei gemischten Kandidaten
- Event-Beschleuniger: Defizit hebt den Peak sofort, ohne aufs Sampling zu warten

**`internal/sizing/window`:**

- Encode/Decode-Roundtrip, Ring-Rotation über die Tagesgrenze
- `maxGap`-Fortschreibung; Downtime macht einen Tag ungültig
- Neu auftauchende Resource blockiert nur diese Resource
- Korruptes JSON und unbekannte Version
- UID-Wechsel setzt das Fenster zurück

**Controller (envtest):**

- Supersede-Ablauf: offener Shrink-PR + FailedCreate → Close, Lock frei, Grow-PR
- TTL-Schließung inklusive `last-shrink`-Update
- Auto-Merge greift bei `grow`, nicht bei `shrink`
- Orphan-Recovery adoptiert mit korrekter Richtung

**Bestehende Tests:**

- `smart_calculation_test.go` und `event_analysis_*_test.go` ziehen ins neue
  Paket um; die Fälle bleiben inhaltlich gültig, da sich die Defizit-Berechnung
  nicht ändert
- `limits_test.go` entfällt mit dem Threshold-Pfad
- `fake_git_provider.go` wird um `ClosePR` und die Richtung erweitert

**E2E (`test/e2e`):** ein Shrink-Szenario mit vorbefülltem Lease-Fenster.

## 11. Umsetzungsreihenfolge

Das Design zerfällt in zwei Stufen, die dem Rollout aus Abschnitt 8 folgen. Nach
Stufe 1 ist das System vollständig lauffähig und liefert bereits Nutzen
(Sichtbarkeit der Verschwendung), ohne dass ein einziger Shrink-PR entstehen
kann.

**Stufe 1 — Beobachtung und Entscheidung, ohne Shrink-PRs**

1. `internal/sizing`: Window-Kodierung, Abdeckungsprüfung, Zielformel, Gates,
   Config-Migration — inklusive Tabellen-Tests
2. Fix für zählbare Ressourcen aus 9.1
3. `observation.go`: Sampling ins Lease, schreibsparsam
4. Controller auf `sizing.Decide` umstellen; Threshold-Pfad und
   `limits_test.go` entfallen. Grow-Verhalten bleibt durch die
   Migrations-Fallbacks unverändert
5. Metriken aus Abschnitt 8

**Stufe 2 — Shrink-PRs**

6. `ClosePR` am `git.Provider`, `FindOpenPR` um die Richtung erweitert,
   Direction-Label beim Erstellen
7. Richtungs-Zustand im Lease, Auto-Merge nur bei `grow`
8. Supersede und TTL
9. `--enable-shrink`-Flag, `resizer.io/shrink-enabled`
10. envtest- und E2E-Abdeckung, Doku (`ARCHITECTURE.md`, `INSTALLATION.md`,
    `OPERATIONS.md`)

## 12. Nicht im Scope

- Abgleich mit echtem Verbrauch (metrics-server/Prometheus) als Reporting-Signal
  für über-requestende Workloads
- Cluster-Kapazitäts- oder Aggregat-Budget-Awareness über alle Namespaces
- Andere Git-Provider als GitHub
