package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type shrinkHarness struct {
	reconciler *ResourceQuotaReconciler
	provider   *FakeGitProvider
	locker     *lock.LeaseLocker
}

// shrinkHarnessOpts controls the two pieces of extra setup more than one
// shrink test needs, so neither has to be duplicated per test.
type shrinkHarnessOpts struct {
	// window, when true, seeds a fully covered observation window for every
	// day the default policy requires. Without it, GateWindow blocks any
	// shrink target and the decision is DirectionNone regardless of how
	// oversized the quota looks — a test that wants a genuine shrink
	// decision (not merely metrics that would produce one if the window
	// were complete) needs this set.
	window bool
	// failEventScan, when true, makes the event List call fail, simulating
	// a scan that could not observe a pending shortage.
	failEventScan bool
}

// shortageObjects returns the event and the owning ReplicaSet that make
// collectDeficits report a pending shortage of the given size. The ReplicaSet
// has to exist because the controller ignores events whose involved object is
// already gone, and it leaves Spec.Replicas nil so the deficit comes straight
// from the event message rather than from a replica calculation.
func shortageObjects(requestedCPU string) []client.Object {
	return []client.Object{
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "team-a"},
		},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "shortage", Namespace: "team-a"},
			Type:       corev1.EventTypeWarning,
			Reason:     "FailedCreate",
			Message: "exceeded quota: compute, requested: requests.cpu=" +
				requestedCPU + ", used: requests.cpu=4, limited: requests.cpu=16",
			InvolvedObject: corev1.ObjectReference{
				Kind:       "ReplicaSet",
				APIVersion: "apps/v1",
				Name:       "web-abc123",
				Namespace:  "team-a",
			},
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	}
}

// newShrinkHarness builds a reconciler whose quota is heavily oversized
// (hard 16, used 4) with shrinking enabled. Extra objects are seeded into the
// fake client, which is how a test stages a pending shortage.
func newShrinkHarness(
	t *testing.T,
	status *git.PRStatus,
	opts shrinkHarnessOpts,
	extra ...client.Object,
) *shrinkHarness {
	t.Helper()
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
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

	objects := append([]client.Object{ns, quota}, extra...)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
	if opts.failEventScan {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				ctx context.Context, cl client.WithWatch,
				list client.ObjectList, listOpts ...client.ListOption,
			) error {
				if _, ok := list.(*corev1.EventList); ok {
					return errors.New("injected event-scan failure")
				}
				return cl.List(ctx, list, listOpts...)
			},
		})
	}
	c := builder.Build()

	locker := lock.NewLeaseLocker(c)
	provider := &FakeGitProvider{PRStatus: status, CreatePRID: 43}

	policy := sizing.DefaultPolicy()
	policy.ShrinkEnabled = true

	if opts.window {
		now := time.Now()
		window := sizing.Window{}
		for i := 1; i <= policy.WindowDays; i++ {
			date := now.UTC().AddDate(0, 0, -i).Format("2006-01-02")
			window.Days = append(window.Days, sizing.DayBucket{
				Date: date, N: 2, First: "00:00", Last: "23:59",
				Peaks: map[string]string{"requests.cpu": "4"},
			})
		}
		encoded, err := sizing.EncodeWindow(window)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(locker.MutateState(context.Background(), "team-a", "compute",
			func(s *lock.State) {
				s.Window = encoded
			})).To(Succeed())
	}

	return &shrinkHarness{
		reconciler: &ResourceQuotaReconciler{
			Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
			GitProvider: provider, Locker: locker,
			Observer:   NewObserver(locker, time.Now),
			BasePolicy: policy,
		},
		provider: provider,
		locker:   locker,
	}
}

func (h *shrinkHarness) reconcile(ctx context.Context) error {
	_, err := h.reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})
	return err
}

func TestShrink_SupersededByAShortage(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A pending shortage of 20 CPU turns the decision into a grow.
	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Now().Add(-3 * 24 * time.Hour),
	}, shrinkHarnessOpts{window: true}, shortageObjects("20")...)

	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(1))
	g.Expect(h.provider.ClosedPRID).To(Equal(42))
	g.Expect(h.provider.ClosedComment).To(ContainSubstring("requests.cpu"))

	state, err := h.locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0), "the lock must be free for the grow PR")
	g.Expect(state.LastShrink.IsZero()).To(BeFalse(),
		"closing a shrink starts its cooldown, or it reopens immediately")
}

func TestShrink_ExpiresAfterTTL(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Now().Add(-8 * 24 * time.Hour),
	}, shrinkHarnessOpts{window: true})
	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(1))
	g.Expect(h.provider.ClosedComment).To(ContainSubstring("without review"))

	state, err := h.locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.LastShrink.IsZero()).To(BeFalse())
}

func TestShrink_YoungPRIsLeftAlone(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Now().Add(-2 * 24 * time.Hour),
	}, shrinkHarnessOpts{window: true})
	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(0))
	g.Expect(h.provider.MergedPRID).To(Equal(0))
}

// TestShrink_ZeroCreatedAtNeverExpires verifies that an unpopulated
// CreatedAt — the provider legitimately returning the zero value — is never
// read as infinitely old. Without the explicit zero check in
// shrinkPRShouldClose, time.Since(zero value) is many decades, which would
// make every shrink pull request appear to have blown its TTL on the very
// first reconcile after it opened.
func TestShrink_ZeroCreatedAtNeverExpires(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{
		IsOpen:    true,
		CreatedAt: time.Time{},
	}, shrinkHarnessOpts{window: true})
	g.Expect(h.locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.PRID = 42
		s.PRDirection = git.DirectionShrink
	})).To(Succeed())

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.ClosePRCalls).To(Equal(0),
		"a zero CreatedAt must not read as infinitely old")
}

// TestShrink_SuppressedByFailedScan verifies that a failed event scan
// suppresses an otherwise-actionable shrink instead of letting it reach
// handleNewProposal on an understated deficit picture. A deficit can only
// ever raise a target, never lower one, so a scan failure can silently tip a
// quota from "no action" into "shrink" — the fully covered window makes the
// metrics alone (Hard 16, Used 4, no deficit either way) already cross into
// a real shrink decision once the window gate clears. The event List call is
// then made to fail, and no pull request must be created.
func TestShrink_SuppressedByFailedScan(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{},
		shrinkHarnessOpts{window: true, failEventScan: true})

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.CreatePRCalls).To(Equal(0),
		"a failed event scan must suppress the shrink, not propose it understated")
}

// TestShrink_OpensAPullRequest pins the route this task exists to open: a
// genuine shrink decision, reached with no active lock, must actually create
// a pull request with the shrink direction — and persist that direction on
// the lease. Every other test in this file asserts that something is *not*
// done (a pull request closed, left alone, or suppressed); without this one,
// reverting the routing in Reconcile back to grow-only would leave the whole
// suite green.
func TestShrink_OpensAPullRequest(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	h := newShrinkHarness(t, &git.PRStatus{}, shrinkHarnessOpts{window: true})

	g.Expect(h.reconcile(ctx)).To(Succeed())

	g.Expect(h.provider.CreatePRCalls).To(Equal(1))
	g.Expect(h.provider.LastDirection).To(Equal(git.DirectionShrink))

	state, err := h.locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRDirection).To(Equal(git.DirectionShrink))
	g.Expect(state.PRID).To(Equal(43))
}
