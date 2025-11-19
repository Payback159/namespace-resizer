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
    $$ \text{NewLimit} = \text{CurrentLimit} + \max(\text{StandardIncrement}, \text{Deficit} + \text{Buffer}) $$
    *   Damit wird sichergestellt, dass das Limit sofort so weit angehoben wird, dass das Deployment durchgeht.

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

### 3.3. Handling Async Latency (Stateful Locking via Kubernetes Leases)

Um zu verhindern, dass der Controller während der Bearbeitungszeit eines Pull Requests (PR) ständig neue PRs erstellt, benötigen wir einen Locking-Mechanismus.
Wir nutzen dazu native Kubernetes `Lease` Objekte (`coordination.k8s.io/v1`).

**Speicherort:**
Die Leases werden im **Namespace des Controllers** (z.B. `namespace-resizer-system`) gespeichert, nicht im Ziel-Namespace.
*Grund:* GitOps-Tools (ArgoCD/Flux) würden Leases im Ziel-Namespace löschen ("Pruning"), da sie nicht im Git definiert sind.

**Namenskonvention:**
`lock-<target-namespace>-<quota-name>`

**Workflow:**

1.  **Check (Local):**
    Der Controller prüft bei Handlungsbedarf zuerst, ob eine passende `Lease` im Controller-Namespace existiert.

2.  **Fall A: Lease existiert (Lock aktiv)**
    *   Der Controller liest die PR-ID aus der Lease (z.B. via Annotation oder `holderIdentity`).
    *   **GitHub Check:** Gezielter API-Call zum Status dieses *einen* PRs.
        *   *PR ist offen:* Prüfe, ob der neue Bedarf höher ist als im PR. Falls ja -> Update PR. Falls nein -> Warten.
        *   *PR ist gemerged/closed:* Der Lock ist veraltet. Lösche die Lease. -> Neustart des Zyklus.

3.  **Fall B: Keine Lease (Lock frei)**
    *   Der Controller erstellt einen neuen Pull Request in GitHub.
    *   Der Controller erstellt eine `Lease` im Controller-Namespace und speichert die PR-URL/ID darin.

**Vorteil:**
*   **Performance:** Spart teure "List PRs" Calls gegen die GitHub API.
*   **Stabilität:** Der Status "Wir arbeiten gerade an Namespace X" liegt im Cluster.
*   **GitOps-Safe:** Da die Leases im System-Namespace liegen, werden sie nicht vom GitOps-Sync des App-Teams gelöscht.
