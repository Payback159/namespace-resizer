# Installation & Konfiguration

## Installation

Der Namespace Resizer kann einfach über `kubectl` installiert werden. Wir stellen sowohl Kustomize-Manifeste als auch ein statisches Installations-Manifest bereit.

### Option 1: Statisches Manifest (Empfohlen)

Für eine schnelle Installation der neuesten Version:

```bash
kubectl apply -f dist/install.yaml
```

Dies installiert den Controller im Namespace `namespace-resizer-system`.

### Option 2: Kustomize

Wenn du Anpassungen vornehmen möchtest (z.B. Image-Tag, Ressourcen-Limits), kannst du Kustomize verwenden:

```bash
# Klone das Repository
git clone https://github.com/Payback159/namespace-resizer.git
cd namespace-resizer

# Bearbeite config/default/kustomization.yaml nach Bedarf

# Installiere
kubectl apply -k config/default
```

## Konfiguration

Der Controller wird primär über **Annotations** an den Namespaces konfiguriert.

### Namespace Annotations

Das Quota-Sizing folgt einer einzigen Zielformel, die in beide Richtungen
handelt (siehe [ARCHITECTURE.md](ARCHITECTURE.md) Abschnitt 2.2). Die
folgenden Annotationen steuern sie. `<resource>` steht für einen
Quota-Key wie `cpu`, `memory`, `storage` oder `pods`; bei `headroom` ist der
Resource-Prefix optional (ohne Prefix gilt der Wert als Namespace-Default für
alle Resources), bei `min` ist er zwingend, weil eine Untergrenze ohne
Resource-Bezug keinen Sinn ergibt.

| Annotation                            | Beschreibung                                                                                   | Default              | Beispiel        |
| -------------------------------------- | ------------------------------------------------------------------------------------------------ | --------------------- | ---------------- |
| `resizer.io/enabled`                  | Aktiviert/deaktiviert den Controller für diesen Namespace                                       | `true`                | `"false"`        |
| `resizer.io/<resource>-headroom` bzw. `resizer.io/headroom` | Puffer über dem beobachteten Bedarf, als Fraktion oder Prozent                  | `0.25`                | `"0.4"`, `"40%"` |
| `resizer.io/tolerance`                | Toleranzband um den Zielwert; innerhalb wird nicht gehandelt                                    | `0.15`                | `"0.1"`          |
| `resizer.io/<resource>-min`           | Harte Untergrenze für eine Resource (Quantity); ein Shrink geht nie darunter                    | – (kein Minimum)      | `"2"`            |
| `resizer.io/window-days`              | Länge des Beobachtungsfensters in Tagen                                                          | `14`                  | `"21"`           |
| `resizer.io/shrink-cooldown-days`     | Mindestabstand zwischen zwei Shrink-PRs für dasselbe Quota                                       | `7`                   | `"14"`           |
| `resizer.io/max-shrink-step`          | Maximale Reduktion pro Shrink-PR, als Anteil des aktuellen Limits                                 | `0.25`                | `"0.1"`          |
| `resizer.io/shrink-pr-ttl-days`       | Ein unreviewter Shrink-PR wird nach dieser Zeit automatisch geschlossen                          | `7`                   | `"3"`            |
| `resizer.io/cooldown-minutes`         | Wartezeit nach einer Grow-Aktion, bevor erneut erhöht wird                                       | `60`                  | `"120"`          |
| `resizer.io/shrink-enabled`           | Opt-out: schaltet Shrink für diesen Namespace ab (siehe Hinweis unten)                            | `true`                | `"false"`        |
| `resizer.io/auto-merge`               | Überschreibt das globale Auto-Merge-Verhalten für diesen Namespace (wirkt nur bei Grow-PRs)      | globale Einstellung   | `"false"`        |

**`resizer.io/shrink-enabled` ist ein Opt-out, kein Opt-in.** Shrinking
findet nur statt, wenn *beides* zutrifft: das globale Flag `--enable-shrink`
ist gesetzt (Default `false`) **und** der Namespace hat sich nicht per
`resizer.io/shrink-enabled: "false"` abgemeldet. Die Annotation kann Shrink
für einen einzelnen Namespace abschalten, aber niemals gegen das globale Flag
anschalten — ein Wert wie `"true"` bewirkt nichts, wenn `--enable-shrink`
global aus ist. Jeder Annotation-Wert außer einem exakten `"true"` gilt als
Opt-out; ein nicht erkannter Wert (z.B. Tippfehler) schaltet Shrink für den
Namespace ebenfalls ab und wird im Controller-Log als Warnung protokolliert.
Details zum Rollout: [OPERATIONS.md](OPERATIONS.md).

#### Migration von Threshold/Increment auf Headroom

Die früheren Annotationen `resizer.io/<resource>-threshold` und
`resizer.io/<resource>-increment` (sowie ihre resource-losen Varianten
`resizer.io/threshold` und `resizer.io/increment`) funktionieren unverändert
weiter — bestehende Installationen müssen nichts anpassen, um ihr
Grow-Verhalten zu behalten. Intern werden sie beim Reconcile in einen
Headroom-Wert übersetzt, mit folgender Rangfolge pro Resource:

1. Ist `<resource>-headroom` gesetzt, wird dieser Wert verwendet.
2. Sonst, ist `<resource>-increment` gesetzt, wird er unverändert als Headroom
   übernommen (`0.2` bleibt `0.2`).
3. Sonst, ist `<resource>-threshold` gesetzt, wird daraus abgeleitet:
   `headroom = 100 / threshold − 1` (z.B. `80` → `0.25`).
4. Sonst gilt der Default `0.25`.

Eine gesetzte, aber veraltete Annotation erzeugt beim ersten Reconcile eine
Deprecation-Warnung im Log, ändert das Ergebnis aber nicht — außer wenn für
dieselbe Resource zusätzlich eine `headroom`-Annotation (oder, für
`threshold`, zusätzlich eine `increment`-Annotation) gesetzt ist: Dann
gewinnt diese nach der Rangfolge oben ohnehin, und die veraltete Annotation
erzeugt keine Warnung, weil sie das Ergebnis gar nicht mehr beeinflusst.

### Authentifizierung (GitHub)

Damit der Controller Pull Requests erstellen kann, muss er authentifiziert werden. Siehe [AUTHENTICATION.md](AUTHENTICATION.md) für Details zur Einrichtung von GitHub Apps oder Personal Access Tokens.

## GitHub Branch Protection & Auto-Merge

Wenn du das **Auto-Merge** Feature nutzen möchtest und in deinem Repository "Branch Protection Rules" (z.B. "Require pull request reviews before merging") aktiviert hast, musst du dem Controller erlauben, diese Regeln zu umgehen.

### Einrichtung der "Bypass List"

1.  Gehe in deinem GitHub Repository zu **Settings** > **Branches**.
2.  Klicke auf **Edit** neben der Branch Protection Rule für deinen Haupt-Branch (z.B. `main`).
3.  Suche den Abschnitt **"Require a pull request before merging"**.
4.  Suche die Option **"Allow specified actors to bypass required pull request reviews"**.
    *   *Hinweis:* Diese Option ist nur verfügbar, wenn "Require pull request reviews before merging" aktiviert ist.
5.  Suche nach dem User oder der GitHub App, die der Controller verwendet (siehe `AUTHENTICATION.md`), und füge sie hinzu.
6.  Speichere die Änderungen ("Save changes").

Damit darf der Controller seine eigenen Pull Requests mergen, auch wenn keine manuellen Reviews vorliegen.
