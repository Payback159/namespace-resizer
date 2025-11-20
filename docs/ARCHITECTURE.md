# Architecture: Namespace Resizer Controller

## 1. Übersicht
Der Controller hat die Aufgabe, Kubernetes `ResourceQuota` Objekte zu überwachen und proaktiv neue Limits vorzuschlagen oder zu setzen, wenn die aktuelle Auslastung einen definierten Schwellenwert überschreitet.

## 2. Phase 1: Erkennung & Berechnung (Detection & Calculation)

In dieser Phase konzentrieren wir uns rein auf die Logik: "Wann muss gehandelt werden?" und "Was ist der neue Zielwert?".

### 2.1. Erkennung (Detection)

Der Controller implementiert einen **Reconciliation Loop**, der `ResourceQuota` Ressourcen beobachtet.

**Datenquellen:**
- `ResourceQuota.status.hard`: Das konfigurierte Limit.
- `ResourceQuota.status.used`: Der aktuelle Verbrauch.

**Trigger-Logik:**
Ein Resize-Event wird ausgelöst, wenn für eine Ressource (z.B. `requests.cpu`, `limits.memory`) gilt:

$$ \frac{\text{used}}{\text{hard}} \times 100 \ge \text{Threshold}_{\%} $$

*Beispiel:*
- Limit: 10 CPU
- Used: 8.5 CPU
- Threshold: 80%
- Berechnung: $8.5 / 10 = 85\%$.
- **Ergebnis:** Trigger ausgelöst (da $85\% \ge 80\%$).

### 2.2. Berechnung (Calculation)

Sobald der Trigger ausgelöst wurde, muss der neue Wert berechnet werden. Hierbei müssen verschiedene Strategien unterstützt werden, um "Flapping" (ständiges Ändern) und unkontrolliertes Wachstum zu verhindern.

**Berechnungs-Modell:**

$$ \text{NewLimit} = \text{CurrentLimit} \times (1 + \text{IncrementFactor}) $$

*Hinweis:* Es gibt kein absolutes `MaxAllowedLimit` für den Namespace, um das Wachstum über den gesamten Lebenszyklus zu ermöglichen. Stattdessen setzen wir auf Geschwindigkeitsbegrenzungen (siehe 2.4).

**Parameter:**
1.  **Threshold**: Prozentwert (0-100), ab wann reagiert wird (Default: 80%).
2.  **IncrementFactor**: Wie stark soll erhöht werden? (z.B. 0.2 für 20%).

### 2.4. Sicherheits-Mechanismen (Guardrails)

Um "unkontrolliertes Wachstum in kurzer Zeit" zu verhindern, wird folgende Bremse eingebaut:

1.  **Cooldown Period (Zeit-Sperre):**
    Nach einer Anpassung (oder Empfehlung) darf für einen definierten Zeitraum (z.B. 1h oder 24h) keine weitere Erhöhung stattfinden.
    *Parameter:* `cooldown_minutes`

### 2.5. Konfiguration (Policy & Scope)

Der Controller arbeitet nach dem Prinzip **"Opt-Out"**. Das bedeutet, er überwacht standardmäßig **alle Namespaces** im Cluster.

**1. Global Defaults:**
Der Controller startet mit globalen Standardwerten (konfigurierbar via CLI-Flags oder ConfigMap), z.B.:
*   Threshold: 80%
*   Increment: 20%
*   Cooldown: 60m

**2. Namespace Overrides (Annotations):**
Einzelne Namespaces können diese Werte überschreiben oder sich komplett vom Resizing ausschließen.

*   **Deaktivieren (Opt-Out):**
    ```yaml
    metadata:
      annotations:
        resizer.io/enabled: "false"
    ```

*   **Parameter anpassen:**
    ```yaml
    metadata:
      annotations:
        resizer.io/cpu-threshold: "90"      # Erst ab 90% reagieren
        resizer.io/cpu-increment: "10%"     # Vorsichtiger erhöhen
    ```

*Empfehlung für Phase 1:* Wir implementieren die Annotation-Logik. CRDs werden vorerst nicht benötigt.

### 2.6. Handling Large Deployments (Burst-Szenarien)

Das rein metrik-basierte Resizing (`used / hard`) hat eine Schwäche: Wenn ein großes Deployment ausgerollt wird, das das Quota sofort sprengt, scheitert das Deployment ("Pending" oder "FailedCreate"), und der `used`-Wert bleibt am Limit kleben (100%). Eine pauschale Erhöhung um 20% reicht dann eventuell nicht aus, um das Deployment zu ermöglichen.

**Lösung: Event-Driven Resizing**

Zusätzlich zum Monitoring der `ResourceQuota`-Objekte überwacht der Controller Kubernetes **Events** im Namespace.

1.  **Trigger:** Suche nach Events vom Typ `Warning` mit Reason `FailedCreate`.
    *   **Quellen:** Dies betrifft Pods, ReplicaSets (Deployments), **StatefulSets**, **DaemonSets** sowie **Jobs** (die von **CronJobs** erstellt werden).
    *   **Filter:** Die Message muss Textbausteine wie "exceeded quota" oder "forbidden" enthalten.
2.  **Analyse:** Diese Fehlermeldungen enthalten oft präzise Informationen über den Fehlbetrag.
    *   *Beispiel:* "exceeded quota: my-quota, requested: cpu=5, used: cpu=8, limited: cpu=10".
    *   *Erkenntnis:* Wir brauchen 5 CPUs, haben aber nur noch 2 frei (10-8). Defizit = 3 CPUs.
3.  **Reaktion (Deficit-Filling):**
    Statt der Standard-Erhöhung (z.B. 20%) wird berechnet, was *mindestens* nötig ist.
### 2.7. Event Deduplication & Stale Events

Ein kritisches Problem bei Event-Driven Resizing ist das "Double Counting" von alten Events.
*Szenario:* Ein Deployment failt (Event A). Der Controller erhöht das Quota. Das Deployment startet. Der Cooldown läuft ab. Das Event A existiert aber immer noch (K8s Events leben standardmäßig 1h). Der Controller würde beim nächsten Lauf Event A erneut sehen und fälschlicherweise denken, es gäbe immer noch ein Problem.

**Lösung: Last-Modified Timestamp in Persistent Lease**
Der Controller speichert den Zeitpunkt der letzten erfolgreichen Änderung (PR Erstellung oder Merge) im **State-Objekt (Lease)** des Controllers (siehe 3.3).

*   **Speicherort:** Annotation `resizer.io/last-modified` am `Lease` Objekt im `namespace-resizer-system` Namespace.
*   **Logik:** Bei der Analyse von Events werden alle Events ignoriert, deren `LastTimestamp` **älter** ist als `resizer.io/last-modified`.

Damit wird sichergestellt, dass jedes Event nur genau einmal zu einer Aktion führt.

## 3. GitOps Kompatibilität & Ausführungs-Strategie

Wir verfolgen einen strikten **GitOps-First** Ansatz. Das bedeutet, dass der Cluster-State (ResourceQuotas) idealerweise immer synchron mit dem Git-Repository ist.

### 3.1. Phase 1: "Observer Mode" (Log & Recommend)
In der ersten Implementierungsphase wird der Controller **keine Änderungen** am Cluster oder an Git vornehmen. Er agiert rein passiv-beobachtend.

**Verhalten:**
1.  Erkennung von Engpässen (Metrik oder Event).
2.  Berechnung des neuen notwendigen Limits (inkl. Guardrails).
3.  **Aktion:**
    *   Strukturierter **Log-Output** (JSON), der von externen Tools geparst werden könnte.
    *   **Kubernetes Event** am ResourceQuota-Objekt (z.B. `Type: Warning, Reason: QuotaResizeRecommended, Message: "CPU limit should be increased to 12"`).

Dies ermöglicht es uns, die Berechnungslogik in echten Umgebungen gefahrlos zu testen und Vertrauen aufzubauen.

### 3.2. Phase 2: Git Integration (GitHub Provider)
Anstatt das Quota im Cluster "schmutzig" zu patchen (was zu Konflikten mit ArgoCD/Flux führen würde), wird der Controller in einer späteren Phase direkt mit dem Source-Code-Management interagieren.

**Geplanter Workflow:**
1.  Controller erkennt Bedarf.
2.  Controller authentifiziert sich gegenüber GitHub (Token/App).
3.  Controller sucht die Datei, die das `ResourceQuota` definiert.
4.  Controller erstellt einen **Pull Request** mit der Anpassung.

*Hinweis:* Die Strategie "Direct Patch" (ehemals Strategie A) wird übersprungen, um die GitOps-Prinzipien nicht zu verletzen.

### 3.3. State Management & Locking (Persistent Leases)

Wir benötigen einen Mechanismus für zwei Dinge:
1.  **Locking:** Verhindern, dass wir mehrere PRs gleichzeitig für denselben Namespace öffnen.
2.  **State:** Speichern, wann wir zuletzt agiert haben (für Event Deduplication), da wir das Quota-Objekt selbst nicht verändern können (GitOps Sync würde es überschreiben).

Wir nutzen dazu native Kubernetes `Lease` Objekte (`coordination.k8s.io/v1`).

**Speicherort:**
Die Leases werden im **Namespace des Controllers** (z.B. `namespace-resizer-system`) gespeichert.
*Grund:* GitOps-Tools (ArgoCD/Flux) würden Leases im Ziel-Namespace löschen ("Pruning"), da sie nicht im Git definiert sind.

**Namenskonvention:**
`state-<target-namespace>-<quota-name>`

**Workflow:**

1.  **Check (Local):**
    Der Controller lädt die Lease für das Ziel-Quota.

2.  **Fall A: Lease hat Holder (Lock aktiv)**
    *   Wir warten (oder aktualisieren den PR, siehe 3.4).

3.  **Fall B: Lease hat keinen Holder (Lock frei)**
    *   Wir prüfen `Annotations["resizer.io/last-modified"]`.
    *   Sind die Events neuer als dieser Timestamp?
        *   **Ja:** Wir übernehmen den Lock (setzen `HolderIdentity`), erstellen PR, aktualisieren Timestamp.
        *   **Nein:** Wir ignorieren die Events (altes Problem).

**Wichtig:** Die Lease wird **nicht gelöscht**, wenn der PR gemerged ist. Wir entfernen nur die `HolderIdentity` (Unlock), damit der State (Timestamp) erhalten bleibt.

### 3.3.1. Garbage Collection (Lease Cleanup)

Da wir für jeden Namespace ein persistentes Lease-Objekt im Controller-Namespace anlegen, könnten sich über die Zeit "verwaiste" Leases ansammeln (z.B. wenn ein Namespace gelöscht wird). Um die Kubernetes API sauber zu halten, implementiert der Controller eine Garbage Collection Routine.

**Mechanismus:**
*   Ein Hintergrund-Prozess (Goroutine) läuft periodisch (z.B. alle 12 Stunden).
*   Er listet alle Leases im Controller-Namespace, die das Label `app.kubernetes.io/managed-by=namespace-resizer` tragen.
*   Er extrahiert den Ziel-Namespace aus dem Lease-Namen.
*   Er prüft, ob der Ziel-Namespace im Cluster noch existiert.
*   Falls der Namespace nicht mehr existiert, wird das Lease-Objekt gelöscht.

### 3.4. Auto-Merge Strategie

Um den Kreis zu schließen und einen vollautomatischen Betrieb zu ermöglichen, kann der Controller Pull Requests selbstständig mergen, sofern bestimmte Kriterien erfüllt sind.

**Konfiguration:**
1.  **Global:** Der Controller verfügt über ein Flag `--enable-auto-merge` (Default: `false`).
2.  **Namespace (Opt-Out):** Wenn global aktiviert, kann das Verhalten pro Namespace deaktiviert werden:
    `resizer.io/auto-merge: "false"`

*Logik:*
*   Global `false`: Auto-Merge ist immer aus (Sicherheitsnetz).
*   Global `true` + Annotation `false`: Auto-Merge ist aus für diesen Namespace.
*   Global `true` + Keine Annotation: Auto-Merge ist **an**.

**Voraussetzungen für Auto-Merge:**
Der Controller prüft bei jedem Reconcile-Loop (wenn ein Lock/PR existiert) den Status des PRs in GitHub:
1.  **Mergeable:** GitHub meldet keinen Konflikt (`mergeable: true`).
2.  **CI Checks:** Der `MergeableState` muss `clean` sein (alle erforderlichen Status-Checks sind erfolgreich durchgelaufen).
3.  **State:** Der PR muss offen sein.

**Ablauf:**
1.  Controller findet aktives Lock & PR.
2.  Controller fragt PR-Status via GitHub API ab.
3.  Wenn `auto-merge: "true"` UND Voraussetzungen erfüllt:
    *   Controller führt Merge durch (Squash-Merge bevorzugt).
    *   Controller löscht das Lock (Lease).
4.  Wenn Voraussetzungen nicht erfüllt (z.B. CI läuft noch):
    *   Controller wartet (Requeue).

**Sicherheit:**
*   Race Conditions (Merge vs. ArgoCD Sync) werden durch die Idempotenz des Controllers abgefangen. Nach dem Merge bleibt die Quota im Cluster kurzzeitig "zu niedrig", bis ArgoCD synct. Der Controller sieht zwar weiterhin den Bedarf, findet aber keinen offenen PR mehr (da gemerged). Er würde theoretisch einen neuen PR erstellen wollen, aber wir können prüfen, ob der letzte Merge erst vor kurzem war (Cooldown) oder ob der Head-Commit des Repos bereits die Änderung enthält.
*   Alternativ: Der Controller wartet einfach. Wenn ArgoCD synct, verschwindet der "Threshold exceeded" Zustand.
