# Namespace Resizer - Betriebshandbuch

Dieses Dokument beschreibt das Verhalten und die Funktionsweise des **Namespace Resizer Controllers** aus Sicht des IT-Betriebs. Es dient dazu, Entscheidungen des Controllers nachzuvollziehen und bei Problemen (z.B. "Warum wurde mein Quota nicht erhöht?") schnell die Ursache zu finden.

## 1. Grundprinzip

Der Controller überwacht Kubernetes Namespaces und passt `ResourceQuota` Objekte automatisch an, wenn der Bedarf steigt.
Er arbeitet nach dem **GitOps-Prinzip**: Änderungen werden als Pull Requests (PRs) im Git-Repository vorgeschlagen.

## 2. Wann reagiert der Controller?

Der Controller reagiert auf zwei Auslöser (Trigger):

### A. Hohe Auslastung (Metrik-basiert)
Wenn der *aktuelle Verbrauch* (`used`) im Verhältnis zum *Limit* (`hard`) einen Schwellenwert überschreitet.
*   **Standard:** Ab **80%** Auslastung.
*   **Formel:** `(Used / Hard) * 100 >= Threshold`
*   **Beispiel:** Limit 10 CPU, Verbrauch steigt auf 8.5 CPU -> Trigger.

### B. Fehlgeschlagene Deployments (Event-basiert)
Wenn ein Pod nicht starten kann, weil das Quota voll ist (`FailedCreate` Event).
*   **Erkennung:** Der Controller liest die Fehlermeldung ("exceeded quota... requested: 2 CPU").
*   **Reaktion:** Er berechnet sofort, wie viel *zusätzlich* benötigt wird, damit der Pod starten kann.
*   **Multi-Burst:** Wenn mehrere Deployments gleichzeitig failen, summiert der Controller den Bedarf auf.
*   **Liveness Check:** Der Controller ignoriert Events von Objekten, die bereits gelöscht wurden (z.B. bei einem Rollback), um keine unnötigen Erhöhungen vorzuschlagen.

## 3. Wie berechnet er das neue Limit?

Das neue Limit ist immer: **Aktueller Bedarf + Puffer**.

*   **Puffer (Increment):** Standardmäßig **20%** (Faktor 0.2).
*   **Rundung:** Werte werden auf lesbare Einheiten gerundet (z.B. auf volle MiB oder 100m CPU), um "krumme" Zahlen wie `1288490188800m` zu vermeiden.

## 4. Sicherheitsmechanismen (Warum passiert nichts?)

Wenn der Controller *nicht* reagiert, liegt es meist an einem dieser Schutzmechanismen:

### A. Cooldown (Abkühlphase)
Nach jeder Aktion (PR erstellt oder Empfehlung geloggt) macht der Controller eine Pause für diesen Namespace.
*   **Dauer:** Standardmäßig **60 Minuten**.
*   **Grund:** Verhindert "Flapping" (ständiges Ändern) und Spamming von PRs.
*   **Log-Meldung:** `Skipping resize due to cooldown`

### B. Opt-Out (Deaktivierung)
Ein Namespace kann explizit ignoriert werden.
*   **Check:** Prüfe Annotation `resizer.io/enabled: "false"` am Namespace.

### C. Offener Pull Request (Locking)
Solange ein PR für diesen Namespace offen ist, erstellt der Controller keinen neuen.
*   **Verhalten:** Er aktualisiert den bestehenden PR mit dem **aktuell berechneten Bedarf**. Das bedeutet, der Wert im PR kann steigen (neuer Burst) oder auch sinken (Burst vorbei/gelöscht), solange er noch nicht gemerged ist.
*   **Grund:** Vermeidung von Konflikten und Race Conditions.

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

## 6. Konfiguration (Annotations)

Werte können pro Namespace angepasst werden:

```yaml
metadata:
  annotations:
    resizer.io/cpu-threshold: "90"      # Erst ab 90% reagieren
    resizer.io/cpu-increment: "0.1"     # Nur 10% Puffer
    resizer.io/cooldown-minutes: "30"   # Nur 30min Pause
    resizer.io/auto-merge: "true"       # (Optional) Automatisch mergen
```
