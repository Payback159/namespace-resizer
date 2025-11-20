package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/config"
	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
		// Update Namespace Annotation
		if ns.Annotations == nil {
			ns.Annotations = make(map[string]string)
		}
		if annotationVal != "" {
			ns.Annotations[config.AnnotationAutoMerge] = annotationVal
		} else {
			delete(ns.Annotations, config.AnnotationAutoMerge)
		}
		fakeClient.Update(context.TODO(), ns)

		fakeGit := &FakeGitProvider{
			PRStatus: prStatus,
		}

		r := &ResourceQuotaReconciler{
			Client:          fakeClient,
			Scheme:          scheme,
			GitProvider:     fakeGit,
			Locker:          locker,
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
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "blocked", ChecksState: "success"})
		g.Expect(fakeGit.MergedPRID).To(Equal(123))
	})

	// Case 6: Global True, No Annotation, PR Blocked by CI -> No Merge
	t.Run("PR Blocked by CI", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "blocked", ChecksState: "failure"})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})

	// Case 7: Global True, No Annotation, PR Unstable -> No Merge
	t.Run("PR Unstable", func(t *testing.T) {
		fakeGit := runReconcile(true, "", &git.PRStatus{IsOpen: true, Mergeable: true, MergeableState: "unstable", ChecksState: "failure"})
		g.Expect(fakeGit.MergedPRID).To(Equal(0))
	})
}
