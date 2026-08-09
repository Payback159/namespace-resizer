package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	prTestNS    = "default"
	prTestQuota = "test-quota"
)

// newResizeNeededQuota builds a ResourceQuota that is fully used (used == hard
// CPU) so that, under sizing.DefaultPolicy, the target (used * 1.25 headroom)
// clears the tolerance band and the reconciler proposes a grow.
func newResizeNeededQuota() *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prTestQuota,
			Namespace: prTestNS,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
		},
	}
}

func newPRTestReconciler(t *testing.T) (*ResourceQuotaReconciler, *fakeClientBundle) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prTestNS}}
	quota := newResizeNeededQuota()

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ns, quota).
		Build()

	locker := lock.NewLeaseLocker(fakeClient)
	r := &ResourceQuotaReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		Recorder:   record.NewFakeRecorder(100),
		Locker:     locker,
		Observer:   NewObserver(locker, time.Now),
		BasePolicy: sizing.DefaultPolicy(),
	}
	return r, &fakeClientBundle{locker: locker}
}

type fakeClientBundle struct {
	locker *lock.LeaseLocker
}

// TestHandleNewProposal_AdoptsOrphanedPR verifies that when a previous reconcile
// created a PR but failed to record the lock, the next reconcile adopts the
// existing PR instead of opening a duplicate, and persists the direction the
// provider reported for it.
func TestHandleNewProposal_AdoptsOrphanedPR(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	r, b := newPRTestReconciler(t)
	fakeGit := &FakeGitProvider{ExistingPR: 555}
	r.GitProvider = fakeGit

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: prTestQuota, Namespace: prTestNS}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).ToNot(HaveOccurred())

	// No duplicate PR must be created.
	g.Expect(fakeGit.CreatePRCalls).To(Equal(0), "must not create a duplicate PR")
	g.Expect(fakeGit.FindOpenPRCalls).To(Equal(1))

	// The orphaned PR (555) must now be locked.
	id, err := b.locker.GetLock(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(555))

	state, err := b.locker.GetState(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(state.PRDirection).To(Equal(git.DirectionGrow), "no direction on the fake defaults to grow")
}

// TestHandleNewProposal_AdoptsOrphanedShrinkPR verifies that adopting an
// orphaned PR persists a reported shrink direction as shrink, not grow.
// Without this, the hardcoded-grow defect this task removes would still pass
// every test.
func TestHandleNewProposal_AdoptsOrphanedShrinkPR(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	r, b := newPRTestReconciler(t)
	fakeGit := &FakeGitProvider{ExistingPR: 555, ExistingPRDirection: git.DirectionShrink}
	r.GitProvider = fakeGit

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: prTestQuota, Namespace: prTestNS}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).ToNot(HaveOccurred())

	state, err := b.locker.GetState(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(state.PRID).To(Equal(555))
	g.Expect(state.PRDirection).To(Equal(git.DirectionShrink),
		"adoption must persist the direction the provider reported, not a hardcoded default")
}

// TestHandleNewProposal_RecordsDecisionDirectionOnCreate guards the create
// path directly: the direction and timestamp written to the lease after
// CreatePR must come from the decision actually passed to CreatePR, not a
// hardcoded default. Reconcile only ever routes to handleNewProposal with a
// grow decision today (Task 12 wires up the shrink route), so this calls
// handleNewProposal directly with a shrink decision to prove the plumbing
// itself — not just today's grow-only caller — is correct.
func TestHandleNewProposal_RecordsDecisionDirectionOnCreate(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	r, b := newPRTestReconciler(t)
	fakeGit := &FakeGitProvider{CreatePRID: 999}
	r.GitProvider = fakeGit

	quota := newResizeNeededQuota()
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prTestNS}}
	policy := sizing.DefaultPolicy()
	decision := sizing.Decision{
		Direction: sizing.DirectionShrink,
		Targets: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("5"),
		},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: prTestQuota, Namespace: prTestNS}}
	_, err := r.handleNewProposal(ctx, req, *quota, ns, policy, lock.State{}, decision)
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(fakeGit.LastDirection).To(Equal(git.DirectionShrink), "CreatePR must receive the decision's direction")

	got, err := b.locker.GetState(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.PRID).To(Equal(999))
	g.Expect(got.PRDirection).To(Equal(git.DirectionShrink),
		"the lease must record the direction actually passed to CreatePR")
	g.Expect(got.LastShrink.IsZero()).To(BeFalse(), "a shrink creation must stamp LastShrink")
	g.Expect(got.LastGrow.IsZero()).To(BeTrue(), "a shrink creation must not also stamp LastGrow")
}

// TestHandleNewProposal_CreatesWhenNoOrphan verifies normal creation when no
// existing PR is found.
func TestHandleNewProposal_CreatesWhenNoOrphan(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	r, b := newPRTestReconciler(t)
	fakeGit := &FakeGitProvider{ExistingPR: 0, CreatePRID: 777}
	r.GitProvider = fakeGit

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: prTestQuota, Namespace: prTestNS}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(fakeGit.FindOpenPRCalls).To(Equal(1))
	g.Expect(fakeGit.CreatePRCalls).To(Equal(1), "must create exactly one PR")

	id, err := b.locker.GetLock(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(777))
}

// TestHandleActivePR_MergeReleasesLock verifies that a successful auto-merge
// releases the lock immediately and records the last-modified timestamp, instead
// of relying on a follow-up status fetch (which is racy).
func TestHandleActivePR_MergeReleasesLock(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: prTestNS}}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: prTestQuota, Namespace: prTestNS}}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(fakeClient)
	g.Expect(locker.AcquireLock(ctx, prTestNS, prTestQuota, 123)).To(Succeed())

	fakeGit := &FakeGitProvider{PRStatus: &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "clean", ChecksState: "success"}}
	r := &ResourceQuotaReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		Recorder:        record.NewFakeRecorder(100),
		GitProvider:     fakeGit,
		Locker:          locker,
		Observer:        NewObserver(locker, time.Now),
		BasePolicy:      sizing.DefaultPolicy(),
		EnableAutoMerge: true,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: prTestQuota, Namespace: prTestNS}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).ToNot(HaveOccurred())

	// The PR was merged.
	g.Expect(fakeGit.MergedPRID).To(Equal(123))

	// The lock must be released right away.
	id, err := locker.GetLock(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0), "lock must be released immediately after merge")

	// The last-modified timestamp must be recorded to start the cooldown.
	lastMod, err := locker.GetLastModified(ctx, prTestNS, prTestQuota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(lastMod.IsZero()).To(BeFalse(), "last-modified must be set after merge")
}
