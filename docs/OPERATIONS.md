# Namespace Resizer - Betriebshandbuch

Dieses Dokument beschreibt das Verhalten und die Funktionsweise des **Namespace Resizer Controllers** aus Sicht des IT-Betriebs. Es dient dazu, Entscheidungen des Controllers nachzuvollziehen und bei Problemen (z.B. "Warum wurde mein Quota nicht erhöht?") schnell die Ursache zu finden.

## 1. Grundprinzip

Der Controller überwacht Kubernetes Namespaces und passt `ResourceQuota` Objekte automatisch an den beobachteten Bedarf an — **in beide Richtungen**. Er vergrößert Quotas bei echtem Engpass zügig und baut Überdimensionierung langsam und reviewbar zurück.
Er arbeitet nach dem **GitOps-Prinzip**: Änderungen werden als Pull Requests (PRs) im Git-Repository vorgeschlagen, nie direkt am Cluster.

## 2. Wann reagiert der Controller?

Der Controller reagiert auf drei Auslöser (Trigger):

### A. Bedarf über dem Zielwert (Metrik-basiert, Grow)
Der Controller berechnet je Resource einen Zielwert aus dem beobachteten Bedarf (siehe Abschnitt 3) und vergleicht ihn mit dem aktuellen Limit (`hard`). Liegt der Zielwert oberhalb eines Toleranzbands um `hard`, wird ein Grow-PR vorgeschlagen.

*   **Verhaltensänderung gegenüber früheren Versionen:** Früher löste eine flache Schwelle von **80 % Auslastung** (`used / hard`) sofort eine Erhöhung aus. Mit den Default-Werten (Headroom 25 %, Toleranz 15 %) verschiebt sich dieser Auslösepunkt bei konstanter Last auf **rund 92 % Auslastung** (`hard < 1,087 × used`, siehe [ARCHITECTURE.md](ARCHITECTURE.md) Abschnitt 2.2 und das [Design-Dokument](design/2026-08-08-quota-rightsizing.md) Abschnitt 3.2). Ein Quota, das stabil bei 85 % Auslastung liegt, erhält also **keinen PR mehr** — das ist beabsichtigt, kein Regressions-Symptom. Wer den alten Auslösepunkt möchte, kann das über eine niedrigere `-headroom`- oder `-tolerance`-Annotation annähern.
*   **Beispiel:** Limit 10 CPU, Bedarf steigt auf 9.3 CPU (93 %) -> Trigger.

### B. Fehlgeschlagene Deployments (Event-basiert, Grow)
Wenn ein Pod nicht starten kann, weil das Quota voll ist (`FailedCreate` Event).
*   **Erkennung:** Der Controller liest die Fehlermeldung ("exceeded quota... requested: 2 CPU").
*   **Reaktion:** Das Defizit hebt den Zielwert sofort an, ohne auf den nächsten Beobachtungszyklus zu warten.
*   **Multi-Burst:** Wenn mehrere Deployments gleichzeitig failen, summiert der Controller den Bedarf auf.
*   **Liveness Check:** Der Controller ignoriert Events von Objekten, die bereits gelöscht wurden (z.B. bei einem Rollback), um keine unnötigen Erhöhungen vorzuschlagen.
*   **Sicherheitsgarantie:** Ein Shrink wird nie aus einem Event-Scan vorgeschlagen, der fehlgeschlagen ist — schlägt das Auslesen der Events fehl, unterdrückt der Controller einen sonst fälligen Shrink lieber für einen Zyklus, statt mit möglicherweise unvollständigen Daten zu schrumpfen.

### C. Überdimensionierung (Metrik-basiert, Shrink)
Liegt der Zielwert unterhalb des Toleranzbands um `hard`, ist das Quota überdimensioniert. Ein Shrink wird aber nur vorgeschlagen, wenn zusätzlich **alle** Gates aus Abschnitt 4 halten — siehe Abschnitt 7 für den empfohlenen Rollout.

## 3. Wie berechnet er das neue Limit?

Eine einzige Formel regelt beide Richtungen:

```
target = max(Peak über das Beobachtungsfenster, aktueller Bedarf) × (1 + Headroom)
```

*   **Headroom:** Puffer über dem beobachteten Bedarf, standardmäßig **25 %** (Annotation `resizer.io/<resource>-headroom`).
*   **Toleranzband:** Innerhalb von ±15 % (Annotation `resizer.io/tolerance`) um den Zielwert passiert nichts — das schließt Flapping zwischen Grow und Shrink strukturell aus.
*   **Beobachtungsfenster:** 14 Tage Tages-Peaks (Annotation `resizer.io/window-days`); nur vollständig abgedeckte Tage zählen (siehe Abschnitt 4).
*   **Shrink-Schritt-Deckel:** Ein einzelner Shrink-PR senkt das Limit um höchstens 25 % (Annotation `resizer.io/max-shrink-step`), auch wenn der Zielwert weiter unten liegt. Große Überdimensionierung wird über mehrere PRs schrittweise abgebaut.
*   **Harter Boden:** Der Zielwert fällt nie unter den aktuellen Bedarf (plus Headroom) oder eine konfigurierte Untergrenze (`resizer.io/<resource>-min`).
*   **Rundung:** Werte werden auf lesbare Einheiten gerundet (z.B. auf volle MiB oder 100m CPU, bei zählbaren Ressourcen wie `pods` aufgerundet auf ganze Zahlen), um "krumme" Zahlen wie `1288490188800m` oder `11250m` Pods zu vermeiden.

Details und Herleitung: [Design-Dokument](design/2026-08-08-quota-rightsizing.md) Abschnitt 3.

## 4. Sicherheitsmechanismen (Warum passiert nichts?)

Wenn der Controller *nicht* reagiert, liegt es meist an einem dieser Schutzmechanismen:

### A. Grow-Cooldown (Abkühlphase)
Nach jeder Grow-Aktion macht der Controller eine Pause für diesen Namespace.
*   **Dauer:** Standardmäßig **60 Minuten**.
*   **Grund:** Verhindert "Flapping" (ständiges Ändern) und Spamming von PRs.
*   **Log-Meldung:** `Skipping resize due to cooldown`

### B. Opt-Out (Deaktivierung)
Ein Namespace kann explizit ignoriert werden.
*   **Check:** Prüfe Annotation `resizer.io/enabled: "false"` am Namespace.

### C. Offener Pull Request (Locking)
Solange ein PR für diesen Namespace offen ist, erstellt der Controller keinen neuen.
*   **Verhalten (Grow):** Er aktualisiert den bestehenden PR mit dem **aktuell berechneten Bedarf**. Das bedeutet, der Wert im PR kann steigen (neuer Burst) oder auch sinken (Burst vorbei/gelöscht), solange er noch nicht gemerged ist.
*   **Verhalten (Shrink):** Ein offener Shrink-PR wird nicht mit neuen Werten aktualisiert. Entsteht während er offen ist ein echter Engpass, schließt der Controller ihn (**Supersede**) und öffnet stattdessen einen Grow-PR — eine Notlage hat immer Vorrang vor einem Rückbau-Vorschlag.
*   **Grund:** Vermeidung von Konflikten und Race Conditions.

### D. Shrink-Gates
Ein Rückbau ist deutlich vorsichtiger abgesichert als ein Grow. Alle vier Gates müssen gleichzeitig halten, sonst passiert nichts — der Controller berechnet den Shrink-Kandidaten trotzdem und exportiert ihn als Metrik (Abschnitt 7):

| Gate (Metrik-Label) | Bedingung | Typische Ursache, wenn blockiert |
|---|---|---|
| `enabled` | `--enable-shrink` ist gesetzt und der Namespace hat sich nicht per `resizer.io/shrink-enabled: "false"` abgemeldet | Rollout noch nicht aktiviert, oder bewusster Opt-out |
| `window` | Das Beobachtungsfenster ist über `window-days` Tage (Default 14) pro Resource lückenlos abgedeckt | Controller lief noch keine 14 Tage durch, oder hatte eine Downtime > 1h an einem Tag |
| `recent-grow` | Innerhalb des Fensters fand kein Grow statt | Das Quota ist erst kürzlich gewachsen — der Rückbau wartet ein Fenster lang ab |
| `cooldown` | Der letzte Shrink liegt länger zurück als `shrink-cooldown-days` (Default 7 Tage) | Ein vorheriger Shrink-PR wurde erst kürzlich gemerged, geschlossen oder von einem Menschen abgelehnt |

Zwei Effekte, die dabei auf den ersten Blick überraschen, aber korrekt sind:

*   Wird ein Shrink-PR von einem Menschen abgelehnt (PR ohne Merge geschlossen), setzt der Controller `resizer.io/last-shrink` auf jetzt. `resizer_shrink_blocked_by{gate="cooldown"}` steht danach für die volle Shrink-Cooldown-Dauer auf `1` — das ist kein hängender Gauge, sondern die Ablehnung, die respektiert wird.
*   Solange ein Gate einen Shrink blockiert, melden `resizer_quota_target` und `resizer_quota_waste_ratio` trotzdem den blockierten Shrink-Zielwert weiter (die Dry-Run-Vorschau wird auch bei blockierendem Gate befüllt). Das ist kein Fehler, sondern genau das, was den Dry-Run-Rollout in Abschnitt 7 überhaupt beobachtbar macht.

### E. Weitere Sicherheitsgarantien

*   **Shrink-PRs werden nie automatisch gemerged** — unabhängig von `--enable-auto-merge` und der `resizer.io/auto-merge`-Annotation. Rückbau ist immer eine bewusste menschliche Entscheidung.
*   **Ein PR ohne erkennbares Richtungs-Label gilt als Grow, ein PR mit einem nicht eindeutig als `grow` erkennbaren Label gilt als Shrink** (siehe [ARCHITECTURE.md](ARCHITECTURE.md) Abschnitt 3.5). Im Zweifel kostet das eine zusätzliche Review-Runde statt eines ungeprüften Merges.
*   **Ein Shrink wird nie aus einem fehlgeschlagenen Event-Scan vorgeschlagen** (siehe 2.B).
*   **Eine echte Notlage schließt einen offenen Shrink-PR und übernimmt** (siehe 4.C, Supersede).

## 5. Troubleshooting Guide

### Szenario: "Mein Deployment hängt, aber kein PR kommt."

1.  **Logs prüfen:**
    ```bash
    kubectl logs -n namespace-resizer-system -l control-plane=controller-manager
    ```
2.  **Nach Schlüsselwörtern suchen:**
    *   `"Skipping resize due to cooldown"` -> Warten oder Cooldown via Annotation verkürzen.
    *   `"Quota file not found"` -> Der Controller findet die Datei im Git nicht (Pfad-Konfiguration prüfen).
    *   `"PR is open"` -> Es gibt schon einen PR, checke GitHub.

### Szenario: "Der PR ist viel zu hoch!"

*   Prüfe, ob es in der letzten Stunde massive "Bursts" gab (viele fehlschlagende Pods gleichzeitig).
*   Der Controller summiert den Bedarf aller *gleichzeitig* fehlschlagenden Workloads.

### Szenario: "Ein Quota ist offensichtlich überdimensioniert, aber es kommt kein Shrink-PR."

1.  Prüfe zuerst, ob `--enable-shrink` überhaupt gesetzt ist (siehe Abschnitt 7) — ohne das Flag entstehen grundsätzlich keine Shrink-PRs, nur Metriken.
2.  Prüfe `resizer_shrink_blocked_by{namespace="...",quota="..."}` für alle vier Gate-Werte (Abschnitt 4.D). Der Wert `1` zeigt das blockierende Gate.
3.  Am häufigsten ist `window`: Ein Controller-Neustart oder eine Downtime von mehr als einer Stunde an einem Tag entwertet diesen Tag für das Beobachtungsfenster.
4.  **Ein blockiertes `window`-Gate lässt sich nicht durch Bearbeiten der Lease-Annotation am laufenden Controller beheben.** Der Controller hält das Beobachtungsfenster pro Quota im Prozessspeicher und liest die Lease dafür nur einmalig ein; eine manuelle Änderung an `resizer.io/observation-window` auf einer bereits beobachteten Lease wird vom laufenden Controller nicht bemerkt und hat keine Wirkung, bis der Controller-Pod neu startet. Zwei Wege wirken stattdessen: ein Neustart des Controller-Pods, der die Lease wieder frisch einliest, oder das Löschen und Neuanlegen des ResourceQuota — die neue UID verwirft das bisherige Fenster beim nächsten Reconcile, weil eine gespeicherte Historie nicht für ein anderes Objekt gelten darf. Das Neuanlegen kostet allerdings die gesamte bisherige Beobachtung, das Fenster beginnt bei null.

## 6. Konfiguration (Annotations)

Werte können pro Namespace angepasst werden — die vollständige Tabelle mit allen Annotationen, Defaults und der Migration von `threshold`/`increment` auf `headroom` steht in [INSTALLATION.md](INSTALLATION.md).

```yaml
metadata:
  annotations:
    resizer.io/cpu-headroom: "0.4"        # 40% Puffer statt Default 25%
    resizer.io/tolerance: "0.1"           # engeres Toleranzband
    resizer.io/cooldown-minutes: "30"     # nur 30min Grow-Pause
    resizer.io/shrink-enabled: "false"    # dieser Namespace bleibt vom Rückbau ausgenommen
    resizer.io/auto-merge: "true"         # (optional) Grow-PRs automatisch mergen; wirkt nie auf Shrink
```

## 7. Monitoring, Metriken und Rollout

### Prometheus-Metriken

Der Controller exposiert Prometheus-Metriken, wenn das `--metrics-bind-address` Flag gesetzt ist (z.B. `:8443`). Diese Metriken sind für die Beobachtung des Shrink-Pfads (insbesondere im Flag-Off-Rollout) **essentiell**:

*   **`resizer_quota_target`**: Das berechnete Ziel für jede Quota-Ressource (in Milli-Einheiten). Wird auch für einen von einem Gate blockierten Shrink weiter gemeldet (siehe 4.D).
*   **`resizer_quota_waste_ratio`**: Verhältnis von aktuellem Hard-Limit zu berechnetem Ziel. Ein Wert nahe `1` heißt, das Quota folgt dem Bedarf bereits eng; ein Wert deutlich über `1` markiert Überdimensionierung.
*   **`resizer_shrink_blocked_by{gate}`**: Welches Gate (`enabled`, `window`, `recent-grow`, `cooldown`) eine Shrink-Operation derzeit blockiert (1 = blockiert, 0 = nicht blockiert). Bleibt nach einer abgelehnten PR erwartungsgemäß für die volle Cooldown-Dauer bei `cooldown=1` stehen (siehe 4.D).
*   **`resizer_decision_total`**: Zähler der Sizing-Decisions pro Richtung (`grow`/`shrink`/`none`).

### Rollout: von Dry-Run zu aktivem Shrink

Das Feature schaltet Shrink standardmäßig **aus**: `--enable-shrink` ist per Default `false`, und `config/manager/manager.yaml` liefert es entsprechend auskommentiert aus. Bis das Flag gesetzt wird, berechnet der Controller Shrink-Entscheidungen trotzdem vollständig und exportiert sie ausschließlich über die Metriken oben — es entsteht kein PR. Das entspricht dem „Observer Mode" aus [ARCHITECTURE.md](ARCHITECTURE.md) Abschnitt 3.1, mit dem das Projekt schon beim Grow-Pfad Vertrauen aufgebaut hat.

Empfohlener Ablauf, um das Flag guten Gewissens zu aktivieren:

1.  Deploye ohne `--enable-shrink` und mit gesetztem `--metrics-bind-address=:8443`, damit die Metriken verfügbar sind.
2.  Beobachte `resizer_quota_waste_ratio` über mindestens ein volles Beobachtungsfenster (14 Tage, Default). Ein Wert nahe `1` heißt, das Quota trackt den Bedarf bereits; ein Wert über `2` markiert einen Namespace, der sich für einen Rückbau lohnt.
3.  Prüfe `resizer_shrink_blocked_by{gate="window"}`. Bleibt der Wert dauerhaft bei `1`, wird der Controller zu häufig neu gestartet, als dass ein Fenster vollständig würde — das zuerst beheben, bevor Shrink aktiviert wird.
4.  Aktiviere `--enable-shrink`. Die ersten Shrink-PRs sind innerhalb eines Tages zu erwarten, höchstens einer pro Quota, jeweils auf maximal 25 % Reduktion gedeckelt.
5.  Um einzelne Namespaces auszunehmen, annotiere sie mit `resizer.io/shrink-enabled: "false"` — das funktioniert unabhängig vom globalen Flag-Zustand und bleibt auch nach dessen Aktivierung bestehen (siehe [INSTALLATION.md](INSTALLATION.md)).

Shrink-PRs werden dabei nie automatisch gemergt und laufen nach 7 Tagen ohne Review automatisch ab (`resizer.io/shrink-pr-ttl-days`); eine währenddessen auftretende echte Notlage schließt einen offenen Shrink-PR vorzeitig und ersetzt ihn durch einen Grow-PR (Supersede, siehe Abschnitt 4.C).
