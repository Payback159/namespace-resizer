package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/config"
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

func TestAutoMerge(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	nsName := "default"
	quotaName := "test-quota"

	// Setup Objects
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nsName,
			Annotations: map[string]string{},
		},
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      quotaName,
			Namespace: nsName,
		},
	}

	// Setup Fake Client
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ns, quota).
		Build()

	// Setup Locker with existing lock (PR ID 123)
	locker := lock.NewLeaseLocker(fakeClient)
	err := locker.AcquireLock(context.TODO(), nsName, quotaName, 123)
	g.Expect(err).ToNot(HaveOccurred())

	// Helper to run reconcile
	runReconcile := func(enableGlobal bool, annotationVal string, prStatus *git.PRStatus) *FakeGitProvider {
		// Ensure the lock is held by PR 123 at the start of each case. A
		// successful auto-merge now releases the lock immediately, so resetting
		// here keeps the sub-tests independent of execution order.
		_ = locker.MutateState(context.TODO(), nsName, quotaName, func(s *lock.State) {
			s.PRID = 0
			s.PRDirection = ""
		})
		g.Expect(locker.AcquireLock(context.TODO(), nsName, quotaName, 123)).To(Succeed())

		// Update Namespace Annotation
		if ns.Annotations == nil {
			ns.Annotations = make(map[string]string)
		}
		if annotationVal != "" {
			ns.Annotations[config.AnnotationAutoMerge] = annotationVal
		} else {
			delete(ns.Annotations, config.AnnotationAutoMerge)
		}
		g.Expect(fakeClient.Update(context.TODO(), ns)).To(Succeed())

		fakeGit := &FakeGitProvider{
			PRStatus: prStatus,
		}

		r := &ResourceQuotaReconciler{
			Client:          fakeClient,
			Scheme:          scheme,
			GitProvider:     fakeGit,
			Locker:          locker,
			Observer:        NewObserver(locker, time.Now),
			BasePolicy:      sizing.DefaultPolicy(),
			EnableAutoMerge: enableGlobal,
		}

		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: quotaName, Namespace: nsName}}
		_, err := r.Reconcile(context.TODO(), req)
		g.Expect(err).ToNot(HaveOccurred())

		return fakeGit
	}

	// Case 1: Global False -> No Merge
	t.Run("Global Disabled", func(t *testing.T) {
		fakeGit := runReconcile(false, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "clean"})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})

	// Case 2: Global True, Annotation False -> No Merge
	t.Run("Opt-Out", func(t *testing.T) {
		fakeGit := runReconcile(true, "false", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "clean"})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})

	// Case 3: Global True, No Annotation, PR Clean -> Merge
	t.Run("Auto-Merge Success", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "clean", ChecksState: "success"})
		g.Expect(fakeGit.MergedPRID).To(Equal(123))
	})

	// Case 4: Global True, No Annotation, PR Dirty -> No Merge
	t.Run("PR Not Mergeable", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: false, MergeableState: "dirty", ChecksState: "success"})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})

	// Case 5: Global True, No Annotation, PR Blocked (Reviews) -> Merge (Bypass)
	t.Run("PR Blocked by Reviews (Bypass)", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "blocked", ChecksState: "success", ChecksTotalCount: 1})
		g.Expect(fakeGit.MergedPRID).To(Equal(123))
	})

	// Case 6: Global True, No Annotation, PR Blocked by CI -> No Merge
	t.Run("PR Blocked by CI", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "blocked", ChecksState: "failure", ChecksTotalCount: 1})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})

	// Case 7: Global True, No Annotation, PR Unstable -> No Merge
	t.Run("PR Unstable", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "unstable", ChecksState: "failure", ChecksTotalCount: 1})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})

	// Case 8: Global True, No Annotation, PR Blocked (Reviews) but No CI -> Merge (Bypass)
	t.Run("PR Blocked by Reviews (No CI)", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "blocked", ChecksState: "pending", ChecksTotalCount: 0})
		g.Expect(fakeGit.MergedPRID).To(Equal(123))
	})
}

func TestAutoMerge_NeverMergesAShrink(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "compute", Namespace: "team-a", UID: types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("16"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("4"),
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(c)

	g.Expect(locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	provider := &FakeGitProvider{PRStatus: &git.PRStatus{
		IsOpen:         true,
		Mergeable:      true,
		MergeableState: git.MergeableStateClean,
		ChecksState:    git.ChecksStateSuccess,
	}}

	reconciler := &ResourceQuotaReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		GitProvider: provider, Locker: locker,
		Observer:        NewObserver(locker, time.Now),
		BasePolicy:      sizing.DefaultPolicy(),
		EnableAutoMerge: true,
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(provider.MergedPRID).To(Equal(0),
		"a shrink PR must never be auto-merged, however clean it looks")
}
