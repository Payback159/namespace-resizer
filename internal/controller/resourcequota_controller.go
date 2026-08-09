/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	resizerConfig "github.com/payback159/namespace-resizer/internal/config"
	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
)

// ResourceQuotaReconciler reconciles a ResourceQuota object
type ResourceQuotaReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	GitProvider     git.Provider
	Locker          *lock.LeaseLocker
	Observer        *Observer
	BasePolicy      sizing.Policy
	EnableAutoMerge bool
}

// +kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=resourcequotas/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;delete;get;list;patch;update;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ResourceQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ResourceQuota", "name", req.Name, "namespace", req.Namespace)

	// 1. Fetch ResourceQuota
	var quota corev1.ResourceQuota
	if err := r.Get(ctx, req.NamespacedName, &quota); err != nil {
		if apierrors.IsNotFound(err) {
			// The quota is gone: drop its cached window so the map does not
			// grow for the rest of the process lifetime, and so that
			// recreating the quota (the documented remedy for a stuck
			// observation window) takes effect immediately instead of
			// waiting for a controller restart. A stale cache miss here
			// just costs one extra Lease read on the next reconcile.
			r.Observer.Forget(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Fetch Namespace to check for annotations
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: req.Namespace}, &ns); err != nil {
		logger.Error(err, "unable to fetch Namespace")
		return ctrl.Result{}, err
	}

	policy, warnings := sizing.ParsePolicy(ns.Annotations, r.BasePolicy)
	for _, warning := range warnings {
		if warning.Kind == sizing.WarningRejected {
			// A rejected value is very likely an operator's mistake having
			// no effect at all — a deprecation notice at V(1) is exactly
			// the visibility that made this near-silent before: it needs a
			// normal-level log line and a Warning event on the namespace,
			// not a debug line an operator has to go looking for.
			logger.Info("annotation rejected", "detail", warning.Message)
			r.Recorder.Event(&ns, corev1.EventTypeWarning,
				"PolicyAnnotationRejected", warning.Message)
			continue
		}
		logger.V(1).Info("deprecated annotation in use", "detail", warning.Message)
	}
	if !policy.Enabled {
		logger.V(1).Info("Namespace is opted out", "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	window, err := r.Observer.Observe(ctx, &quota, policy.WindowDays)
	if err != nil {
		logger.Error(err, "failed to record observation")
		return ctrl.Result{}, err
	}

	state, err := r.Locker.GetState(ctx, req.Namespace, quota.Name)
	if err != nil {
		logger.Error(err, "failed to read lease state")
		return ctrl.Result{}, err
	}

	deficits, err := r.collectDeficits(ctx, quota, state.LastModified)
	deficitScanFailed := err != nil
	if err != nil {
		// A failed event scan must not stop the metric-driven path; it only
		// means a pending shortage may be reacted to one reconcile later.
		//
		// It is not symmetric, though: a missing deficit can only lower the
		// target, so a failed scan could tip a quota from "no action" into
		// "shrink". The guard below stops that shrink from being proposed.
		logger.Error(err, "failed to collect event deficits")
	}

	decision := sizing.Decide(sizing.Input{
		Now:        time.Now(),
		Hard:       quota.Status.Hard,
		Used:       quota.Status.Used,
		Deficits:   deficits,
		Window:     window,
		Policy:     policy,
		LastGrow:   state.LastGrow,
		LastShrink: state.LastShrink,
	})

	recordDecision(req.Namespace, quota.Name, quota.Status.Hard, decision)
	logger.V(1).Info("Sizing decision",
		"direction", decision.Direction.String(),
		"targets", decision.Targets,
		"blockedBy", decision.BlockedBy)

	if state.PRID != 0 {
		return r.handleActivePR(ctx, req, quota, ns, policy, state, decision)
	}

	if decision.Direction == sizing.DirectionShrink && deficitScanFailed {
		logger.Info("Shrink suppressed: the event scan failed, so the " +
			"target may be understated")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	if decision.Direction != sizing.DirectionNone {
		return r.handleNewProposal(ctx, req, quota, ns, policy, state, decision)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// handleActivePR manages the lifecycle of an existing Pull Request
func (r *ResourceQuotaReconciler) handleActivePR(ctx context.Context, req ctrl.Request, quota corev1.ResourceQuota, ns corev1.Namespace, policy sizing.Policy, state lock.State, decision sizing.Decision) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	prID := state.PRID
	logger.Info("Lock found, checking PR status", "prID", prID)

	status, err := r.GitProvider.GetPRStatus(ctx, prID)
	if err != nil {
		logger.Error(err, "failed to get PR status")
		return ctrl.Result{}, err
	}

	if !status.IsOpen {
		// PR is merged or closed -> Release Lock
		logger.Info("PR is closed/merged, releasing lock", "prID", prID)

		now := time.Now()
		err := r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
			// Same caveat as the auto-merge gate below: this only knows what
			// the lease recorded, so a merged PR whose direction was never
			// persisted there is stamped as a grow.
			wasShrink := s.PRDirection == git.DirectionShrink
			s.PRID = 0
			s.PRDirection = ""
			if !status.IsMerged {
				// A closed shrink is a rejection. Without the cooldown stamp
				// the requeue below would recompute the same shrink and
				// reopen it immediately. A closed grow needs no stamp: the
				// shortage that caused it re-triggers on its own.
				if wasShrink {
					s.LastShrink = now
				}
				return
			}
			s.LastModified = now
			if wasShrink {
				s.LastShrink = now
			} else {
				s.LastGrow = now
			}
		})
		if err != nil {
			logger.Error(err, "failed to release lock")
			return ctrl.Result{}, err
		}

		// Requeue immediately to start fresh (check cooldown, etc.)
		return ctrl.Result{Requeue: true}, nil
	}

	if state.PRDirection == git.DirectionShrink {
		if reason, expire := shrinkPRShouldClose(policy, status, decision); expire {
			logger.Info("Closing shrink PR", "prID", state.PRID, "reason", reason)
			if err := r.GitProvider.ClosePR(ctx, state.PRID, reason); err != nil {
				logger.Error(err, "failed to close shrink PR", "prID", state.PRID)
				return ctrl.Result{}, err
			}
			// Recording the shrink timestamp is what stops the very next
			// reconcile from opening the same PR again.
			now := time.Now()
			err := r.Locker.MutateState(ctx, req.Namespace, quota.Name,
				func(s *lock.State) {
					s.PRID = 0
					s.PRDirection = ""
					s.LastShrink = now
				})
			if err != nil {
				logger.Error(err, "failed to release lock after closing shrink PR")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// PR is open -> Check Auto-Merge
	//
	// Shrink pull requests are never auto-merged, regardless of the global
	// flag or the namespace annotation: reclaiming quota is the owning team's
	// decision, not the controller's. This gate only knows what the lease
	// recorded, though: it trusts state.PRDirection, so a PR whose direction
	// was never persisted there would pass through as a grow.
	shouldAutoMerge := r.EnableAutoMerge && state.PRDirection != git.DirectionShrink
	if val, ok := ns.Annotations[resizerConfig.AnnotationAutoMerge]; ok && val == "false" {
		shouldAutoMerge = false
	}

	if shouldAutoMerge {
		if strings.ToLower(status.MergeableState) == "unknown" {
			logger.Info("Mergeable state unknown from GitHub; requeueing to allow computation", "prID", prID)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		canAttemptMerge := status.Mergeable &&
			(status.MergeableState == git.MergeableStateClean ||
				(status.MergeableState == git.MergeableStateBlocked && (status.ChecksState == git.ChecksStateSuccess || status.ChecksTotalCount == 0)))

		if canAttemptMerge {
			logger.Info("Auto-merging PR", "prID", prID, "state", status.MergeableState, "checks", status.ChecksState, "checksCount", status.ChecksTotalCount)
			if err := r.GitProvider.MergePR(ctx, prID, "squash"); err != nil {
				logger.Error(err, "failed to auto-merge PR")
			} else {
				// The merge succeeded, so we release the lock and record the
				// last-modified timestamp right away. Relying on a follow-up
				// GetPRStatus call would be racy: GitHub may still report the PR as
				// open for a short window, causing the controller to attempt a
				// second merge on the next reconcile.
				now := time.Now()
				err := r.Locker.MutateState(ctx, req.Namespace, quota.Name,
					func(s *lock.State) {
						s.PRID = 0
						s.PRDirection = ""
						s.LastModified = now
						s.LastGrow = now
					})
				if err != nil {
					logger.Error(err, "failed to release lock after merge")
					return ctrl.Result{}, err
				}
				logger.Info("PR auto-merged and lock released", "prID", prID)
				return ctrl.Result{Requeue: true}, nil
			}
		} else {
			logger.Info("Auto-merge enabled but PR is not ready",
				"mergeable", status.Mergeable,
				"state", status.MergeableState,
				"checks", status.ChecksState,
				"checksCount", status.ChecksTotalCount)
		}
	}

	// Update PR if recommendations changed. Only a grow decision may rewrite
	// the PR: a shrink decision reaching this point would otherwise lower the
	// limits on an open grow PR, which the auto-merge block above could then
	// merge — breaking the rule that a shrink is never auto-merged. A shrink
	// decision on an open shrink PR that shrinkPRShouldClose left alone (not
	// superseded, not expired) falls through to the DirectionShrink case
	// below, which does not call UpdatePR.
	switch decision.Direction {
	case sizing.DirectionGrow:
		logger.Info("PR is open, updating if needed", "prID", prID)
		if err := r.GitProvider.UpdatePR(ctx, prID, quota.Name, req.Namespace, ns.Annotations, decision.Targets); err != nil {
			if errors.Is(err, git.ErrFileNotFound) {
				logger.Info("Quota file not found in Git repository during update. Retrying later.", "error", err.Error())
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			logger.Error(err, "failed to update PR")
			return ctrl.Result{}, err
		}
	case sizing.DirectionShrink:
		logger.Info("Shrink PR is still open and awaiting review", "prID", prID)
	default:
		logger.Info("PR is open but no resize needed currently", "prID", prID)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// shrinkPRShouldClose reports whether an open shrink pull request has to be
// abandoned, and why. Growth supersedes it because a shortage is a live
// outage; the TTL catches the case where nobody reviewed it and the lock would
// otherwise be held indefinitely.
func shrinkPRShouldClose(
	policy sizing.Policy,
	status *git.PRStatus,
	decision sizing.Decision,
) (string, bool) {
	if decision.Direction == sizing.DirectionGrow {
		return "Superseded: a shortage was detected and the quota has to grow " +
			"instead.\n\n" + decision.Reason, true
	}
	if !status.CreatedAt.IsZero() &&
		time.Since(status.CreatedAt) > policy.ShrinkPRTTL {
		return "Closing automatically: this shrink proposal has been open for " +
			formatDays(policy.ShrinkPRTTL) + " without review. A fresh " +
			"proposal will be opened once the cooldown expires.", true
	}
	return "", false
}

// formatDays renders a duration as whole days for a human-facing comment.
// time.Duration.String() would render policy.ShrinkPRTTL's default as
// "168h0m0s", which nobody reviewing a pull request wants to do arithmetic
// on. ShrinkPRTTL is always configured in whole days (see policy.go), so
// integer division loses nothing.
func formatDays(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// handleNewProposal manages the creation of new Pull Requests
func (r *ResourceQuotaReconciler) handleNewProposal(
	ctx context.Context,
	req ctrl.Request,
	quota corev1.ResourceQuota,
	ns corev1.Namespace,
	policy sizing.Policy,
	state lock.State,
	decision sizing.Decision,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	recommendations := decision.Targets

	// 0. Recover orphaned PRs before creating a new one.
	// A PR may have been created in a previous reconcile where the subsequent
	// AcquireLock failed (transient API error, optimistic-concurrency conflict,
	// or a controller restart between CreatePR and AcquireLock). That leaves an
	// open PR with no lock recorded. Without this check the controller would open
	// a brand-new duplicate PR on every reconcile.
	existingPRID, existingDirection, err := r.GitProvider.FindOpenPR(ctx, req.Namespace, quota.Name)
	if err != nil {
		logger.Error(err, "failed to check for existing open PR")
		return ctrl.Result{}, err
	}
	if existingPRID != 0 {
		logger.Info("Found existing open PR without lock; adopting it instead of creating a duplicate", "prID", existingPRID, "direction", existingDirection)
		err = r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
			s.PRID = existingPRID
			s.PRDirection = existingDirection
		})
		if err != nil {
			logger.Error(err, "failed to acquire lock for existing PR")
			return ctrl.Result{}, err
		}
		// Requeue so the next pass manages the now-locked PR (status/update/merge).
		return ctrl.Result{Requeue: true}, nil
	}

	// 1. Detect No-Op
	isNoop := true
	for res, rec := range recommendations {
		cur, ok := quota.Spec.Hard[res]
		if !ok {
			isNoop = false
			break
		}
		if cur.Cmp(rec) != 0 {
			isNoop = false
			break
		}
	}
	if isNoop {
		logger.Info("Detected no-op recommendation; skipping PR creation", "namespace", req.Namespace, "quota", quota.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
	}

	// 2. Smart Cooldown Check
	//
	// A shrink skips this: the shrink cooldown gate in Decide already governs
	// it, and applying both here would silently double the wait.
	if decision.Direction == sizing.DirectionGrow && !state.LastModified.IsZero() {
		elapsed := time.Since(state.LastModified)
		if elapsed < policy.GrowCooldown {
			remaining := policy.GrowCooldown - elapsed
			logger.Info("Skipping resize due to cooldown",
				"cooldown", policy.GrowCooldown, "remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining + 1*time.Second}, nil
		}
	}

	// 3. Create PR
	// Log recommendation. A shrink is an optimisation, not a shortage: it
	// gets the opposite verb and a Normal event instead of a Warning.
	verb, eventType := "Increase", corev1.EventTypeWarning
	if decision.Direction == sizing.DirectionShrink {
		verb, eventType = "Decrease", corev1.EventTypeNormal
	}
	for res, newLimit := range recommendations {
		currentLimit := quota.Status.Hard[res]
		msg := fmt.Sprintf("Recommendation: %s %s from %s to %s",
			verb, res, currentLimit.String(), newLimit.String())
		logger.Info(msg)
		r.Recorder.Event(&quota, eventType, "QuotaResizeRecommended", msg)
	}

	logger.Info("No lock found, creating PR")
	newPRID, err := r.GitProvider.CreatePR(
		ctx, quota.Name, req.Namespace, decision.Direction.String(),
		ns.Annotations, recommendations)
	if err != nil {
		if errors.Is(err, git.ErrFileNotFound) {
			logger.Info("Quota file not found in Git repository. Retrying later.", "error", err.Error())
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		logger.Error(err, "failed to create PR")
		return ctrl.Result{}, err
	}

	logger.Info("PR created, acquiring lock", "prID", newPRID)
	err = r.Locker.MutateState(ctx, req.Namespace, quota.Name, func(s *lock.State) {
		s.PRID = newPRID
		s.PRDirection = decision.Direction.String()
		if decision.Direction == sizing.DirectionShrink {
			s.LastShrink = time.Now()
		} else {
			s.LastGrow = time.Now()
		}
	})
	if err != nil {
		logger.Error(err, "failed to record the new pull request")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// collectDeficits scans recent FailedCreate events and returns, per quota key,
// the additional milli-value that the blocked workloads asked for. Events older
// than the last successful change are skipped so a single shortage cannot be
// counted twice.
func (r *ResourceQuotaReconciler) collectDeficits(
	ctx context.Context,
	quota corev1.ResourceQuota,
	since time.Time,
) (map[corev1.ResourceName]int64, error) {
	logger := log.FromContext(ctx)

	var eventList corev1.EventList
	if err := r.List(ctx, &eventList, client.InNamespace(quota.Namespace)); err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-1 * time.Hour)

	// Keyed by resource, then by workload: the maximum per workload, summed
	// across workloads. Retries of one workload must not accumulate.
	perWorkload := make(map[corev1.ResourceName]map[string]int64)

	for _, evt := range eventList.Items {
		if evt.LastTimestamp.Time.Before(cutoff) {
			continue
		}
		if !since.IsZero() && evt.LastTimestamp.Time.Before(since) {
			continue
		}
		if evt.Type != corev1.EventTypeWarning || evt.Reason != reasonFailedCreate {
			continue
		}
		if !strings.Contains(evt.Message, "exceeded quota") ||
			!strings.Contains(evt.Message, quota.Name) {
			continue
		}

		resName, reqQty, err := sizing.ParseEventMessage(evt.Message)
		if err != nil {
			logger.Error(err, "Failed to parse event message", "message", evt.Message)
			continue
		}
		if !r.isObjectAlive(ctx, evt.InvolvedObject, quota.Namespace) {
			continue
		}

		key, workloadDeficits := r.calculateWorkloadDeficit(ctx, evt, resName, reqQty)
		for rName, value := range workloadDeficits {
			if _, ok := perWorkload[rName]; !ok {
				perWorkload[rName] = make(map[string]int64)
			}
			if value > perWorkload[rName][key] {
				perWorkload[rName][key] = value
			}
		}
	}

	deficits := make(map[corev1.ResourceName]int64, len(perWorkload))
	for resName, workloads := range perWorkload {
		var total int64
		for _, value := range workloads {
			total += value
		}
		quotaKey, ok := resolveQuotaKey(quota, resName)
		if !ok {
			continue
		}
		deficits[quotaKey] += total
	}

	return deficits, nil
}

// resolveQuotaKey maps a resource name derived from an event onto the key the
// quota actually uses, trying the short and the long form in turn.
func resolveQuotaKey(
	quota corev1.ResourceQuota,
	res corev1.ResourceName,
) (corev1.ResourceName, bool) {
	if _, ok := quota.Status.Hard[res]; ok {
		return res, true
	}

	candidates := map[corev1.ResourceName]corev1.ResourceName{
		corev1.ResourceCPU:             corev1.ResourceRequestsCPU,
		corev1.ResourceMemory:          corev1.ResourceRequestsMemory,
		corev1.ResourceStorage:         corev1.ResourceRequestsStorage,
		corev1.ResourceRequestsCPU:     corev1.ResourceCPU,
		corev1.ResourceRequestsMemory:  corev1.ResourceMemory,
		corev1.ResourceRequestsStorage: corev1.ResourceStorage,
	}
	if alt, ok := candidates[res]; ok {
		if _, ok := quota.Status.Hard[alt]; ok {
			return alt, true
		}
	}
	return "", false
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ResourceQuota{}).
		Named("resourcequota").
		Watches(&corev1.Event{}, handler.EnqueueRequestsFromMapFunc(r.mapEventToQuota)).
		Complete(r)
}
