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

| Annotation                    | Beschreibung                                              | Default | Beispiel  |
| ----------------------------- | --------------------------------------------------------- | ------- | --------- |
| `resizer.io/enabled`          | Aktiviert/Deaktiviert den Controller für diesen Namespace | `true`  | `"false"` |
| `resizer.io/threshold`        | Globaler Schwellenwert in % (0-100)                       | `80`    | `"90"`    |
| `resizer.io/cpu-threshold`    | Spezifischer Schwellenwert für CPU                        | `80`    | `"85"`    |
| `resizer.io/memory-threshold` | Spezifischer Schwellenwert für Memory                     | `80`    | `"90"`    |
| `resizer.io/increment`        | Globaler Erhöhungsfaktor (0.2 = 20%)                      | `0.2`   | `"0.5"`   |
| `resizer.io/cpu-increment`    | Spezifischer Erhöhungsfaktor für CPU                      | `0.2`   | `"0.1"`   |
| `resizer.io/cooldown-minutes` | Wartezeit nach einer Änderung in Minuten                  | `60`    | `"120"`   |

### Authentifizierung (GitHub)

Damit der Controller Pull Requests erstellen kann, muss er authentifiziert werden. Siehe [AUTHENTICATION.md](AUTHENTICATION.md) für Details zur Einrichtung von GitHub Apps oder Personal Access Tokens.

Die Credentials werden als Secret im Controller-Namespace erwartet:

```bash
kubectl create secret generic github-auth \
  --from-literal=app-id=12345 \
  --from-literal=installation-id=67890 \
  --from-file=private-key=private-key.pem \
  -n namespace-resizer-system
```
