# Installation & Configuration

## Installation

The Namespace Resizer installs with a plain `kubectl`. Both Kustomize manifests and a static install manifest are provided.

### Option 1: Static Manifest (Recommended)

For a quick install of the latest version:

```bash
kubectl apply -f dist/install.yaml
```

This installs the controller into the `namespace-resizer-system` namespace.

### Option 2: Kustomize

If you want to customise the deployment (image tag, resource limits, and so on), use Kustomize:

```bash
# Clone the repository
git clone https://github.com/Payback159/namespace-resizer.git
cd namespace-resizer

# Edit config/default/kustomization.yaml as needed

# Install
kubectl apply -k config/default
```

## Configuration

The controller is configured primarily through **annotations** on the namespaces.

### Namespace Annotations

Quota sizing follows a single target formula that acts in both directions (see
[ARCHITECTURE.md](ARCHITECTURE.md) section 2.2). The annotations below steer
it. `<resource>` stands for a quota key such as `cpu`, `memory`, `storage` or
`pods`; for `headroom` the resource prefix is optional (without a prefix the
value becomes the namespace-wide default for every resource), for `min` it is
mandatory, because a lower bound without a resource to bound makes no sense.

| Annotation                            | Description                                                                                    | Default              | Example         |
| -------------------------------------- | ------------------------------------------------------------------------------------------------ | --------------------- | ---------------- |
| `resizer.io/enabled`                  | Enables/disables the controller for this namespace                                              | `true`                | `"false"`        |
| `resizer.io/<resource>-headroom` or `resizer.io/headroom` | Buffer above observed demand, as a fraction or a percentage                  | `0.25`                | `"0.4"`, `"40%"` |
| `resizer.io/tolerance`                | Tolerance band around the target; nothing happens inside it                                     | `0.15`                | `"0.1"`          |
| `resizer.io/<resource>-min`           | Hard lower bound for a resource (Quantity); a shrink never goes below it                        | – (no minimum)        | `"2"`            |
| `resizer.io/window-days`              | Length of the observation window in days                                                        | `14`                  | `"21"`           |
| `resizer.io/shrink-cooldown-days`     | Minimum gap between two shrink PRs for the same quota                                           | `7`                   | `"14"`           |
| `resizer.io/max-shrink-step`          | Maximum reduction per shrink PR, as a share of the current limit                                | `0.25`                | `"0.1"`          |
| `resizer.io/shrink-pr-ttl-days`       | An unreviewed shrink PR is closed automatically after this long                                 | `7`                   | `"3"`            |
| `resizer.io/cooldown-minutes`         | Wait after a grow before raising again                                                          | `60`                  | `"120"`          |
| `resizer.io/shrink-enabled`           | Opt-out: switches shrinking off for this namespace (see the note below)                         | `true`                | `"false"`        |
| `resizer.io/auto-merge`               | Overrides the global auto-merge behaviour for this namespace (applies to grow PRs only)         | global setting        | `"false"`        |

**`resizer.io/shrink-enabled` is an opt-out, not an opt-in.** Shrinking only
happens when *both* hold: the global `--enable-shrink` flag is set (default
`false`) **and** the namespace has not opted out with
`resizer.io/shrink-enabled: "false"`. The annotation can switch shrinking off
for an individual namespace, but never on against the global flag — a value
of `"true"` does nothing while `--enable-shrink` is globally off. Any
annotation value other than an exact `"true"` counts as an opt-out; an
unrecognised value (a typo, say) also switches shrinking off for the namespace
and is recorded as a warning in the controller log and as a Warning event on
the quota. For the rollout, see [OPERATIONS.md](OPERATIONS.md).

#### Migrating from Threshold/Increment to Headroom

The earlier annotations `resizer.io/<resource>-threshold` and
`resizer.io/<resource>-increment` (along with their resource-less variants
`resizer.io/threshold` and `resizer.io/increment`) keep working unchanged —
existing installations need no edits to keep their grow behaviour. They are
translated into a headroom value on each reconcile, with this precedence per
resource:

1. If `<resource>-headroom` is set, that value is used.
2. Otherwise, if `<resource>-increment` is set, it is taken over as the
   headroom unchanged (`0.2` stays `0.2`).
3. Otherwise, if `<resource>-threshold` is set, the headroom is derived from
   it: `headroom = 100 / threshold − 1` (e.g. `80` → `0.25`).
4. Otherwise the default `0.25` applies.

A deprecated annotation that is set produces a deprecation warning in the log
on the first reconcile without changing the result — except when a `headroom`
annotation (or, for `threshold`, an `increment` annotation) is set for the same
resource: that one wins by the precedence above anyway, and the deprecated
annotation then produces no warning, because it no longer influences the
result at all.

### Authentication (GitHub)

The controller has to authenticate before it can open pull requests. See
[AUTHENTICATION.md](AUTHENTICATION.md) for how to set up a GitHub App or a
personal access token.

## GitHub Branch Protection & Auto-Merge

If you want to use the **auto-merge** feature and your repository has branch
protection rules enabled (for example "Require pull request reviews before
merging"), the controller has to be allowed to bypass them.

### Setting Up the Bypass List

1.  In your GitHub repository, go to **Settings** > **Branches**.
2.  Click **Edit** next to the branch protection rule for your main branch (e.g. `main`).
3.  Find the **"Require a pull request before merging"** section.
4.  Find the **"Allow specified actors to bypass required pull request reviews"** option.
    *   *Note:* this option only appears when "Require pull request reviews before merging" is enabled.
5.  Search for the user or GitHub App the controller uses (see `AUTHENTICATION.md`) and add it.
6.  Save the changes.

The controller may then merge its own pull requests even without manual reviews.
