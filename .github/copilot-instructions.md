# Namespace Resizer - Copilot Instructions

You are an expert AI assistant working on the "Namespace Resizer" Kubernetes controller.
This project is a Kubernetes controller written in **Go** using **Kubebuilder**.

## Project Context & Architecture

Always refer to `docs/ARCHITECTURE.md` for the definitive source of truth regarding logic and behavior.

**Key Architectural Decisions:**

1.  **Phase 1 (Observer Mode):** The controller currently ONLY logs recommendations and emits Kubernetes Events. It does NOT modify the cluster state or Git repositories yet.
2.  **Detection Logic:**
    - **Metric-based:** `(used / hard) * 100 >= Threshold`.
    - **Event-based:** Listens for `FailedCreate` events (Pods, Deployments, StatefulSets, DaemonSets, Jobs) to detect burst scenarios.
3.  **Calculation Logic:** `NewLimit = CurrentLimit * (1 + IncrementFactor)` or `NewLimit = CurrentLimit + max(Increment, Deficit + Buffer)` for events.
4.  **Configuration:**
    - **Opt-Out:** All namespaces are watched by default.
    - **Annotations:** Configuration is done via `resizer.io/*` annotations on the Namespace object.
    - `resizer.io/enabled: "false"` disables the controller for a namespace.
5.  **GitOps Strategy (Future):**
    - We will use a "Stateful Locking" mechanism via Kubernetes `Lease` objects in the controller's namespace.
    - We do NOT use CRDs for policy in Phase 1.

## Tech Stack & Standards

- **Language:** Go (Golang) 1.25+
- **Framework:** Kubebuilder / Controller-Runtime
- **Testing:** Ginkgo / Gomega

## Coding Guidelines

1.  **Kubebuilder Patterns:** Follow standard Kubebuilder patterns for Reconcilers.
    - Use `r.Client.Get` and `r.Client.List`.
    - Return `ctrl.Result{}, nil` on success.
    - Return `ctrl.Result{}, err` on retryable errors.
2.  **Error Handling:** Wrap errors with context (e.g., `fmt.Errorf("failed to list pods: %w", err)`).
3.  **Logging:** Use `logr.Logger` (structured logging).
4.  **Idempotency:** The Reconcile loop must be idempotent.

## Workflow

- When asked to implement a feature, first check `docs/TODO.md` and `docs/ARCHITECTURE.md`.
- If the user asks for code, ensure it aligns with the "Observer Mode" restriction of Phase 1 (no writes to Quotas).
