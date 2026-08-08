package lock

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// State is the complete controller-owned state for one quota, as persisted on
// its Lease. Reading and writing it as a unit keeps the number of API round
// trips constant as more fields are added.
type State struct {
	// PRID is the open pull request, or 0 when the lock is free.
	PRID int
	// PRDirection is "grow", "shrink", or empty when no PR is open.
	PRDirection string

	LastModified time.Time
	LastGrow     time.Time
	LastShrink   time.Time

	// Window is the raw JSON observation window. The lock package does not
	// interpret it; sizing.DecodeWindow does.
	Window string
}

// GetState reads the full state in a single API call. A missing Lease yields
// the zero State, which is the correct starting point for a new quota.
func (l *LeaseLocker) GetState(ctx context.Context, targetNS, quotaName string) (State, error) {
	leaseName := l.getLeaseName(targetNS, quotaName)

	var lease coordinationv1.Lease
	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if err != nil {
		if errors.IsNotFound(err) {
			return State{}, nil
		}
		return State{}, err
	}
	return stateFromLease(&lease), nil
}

// MutateState applies fn to the current state and writes the result back,
// retrying on optimistic-concurrency conflicts. The Lease is created if it does
// not exist yet, so callers need no separate bootstrap step.
func (l *LeaseLocker) MutateState(
	ctx context.Context,
	targetNS, quotaName string,
	fn func(*State),
) error {
	leaseName := l.getLeaseName(targetNS, quotaName)

	if err := l.ensureLeaseExists(ctx, leaseName, targetNS, quotaName); err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var lease coordinationv1.Lease
		key := client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}
		if err := l.client.Get(ctx, key, &lease); err != nil {
			return err
		}

		state := stateFromLease(&lease)
		fn(&state)
		applyStateToLease(&state, &lease)

		return l.client.Update(ctx, &lease)
	})
}

func stateFromLease(lease *coordinationv1.Lease) State {
	state := State{
		PRDirection:  lease.Annotations[AnnotationPRDirection],
		Window:       lease.Annotations[AnnotationWindow],
		LastModified: parseStamp(lease.Annotations[AnnotationLastModified]),
		LastGrow:     parseStamp(lease.Annotations[AnnotationLastGrow]),
		LastShrink:   parseStamp(lease.Annotations[AnnotationLastShrink]),
	}
	if lease.Spec.HolderIdentity != nil {
		var id int
		if _, err := fmt.Sscanf(*lease.Spec.HolderIdentity, "pr-%d", &id); err == nil {
			state.PRID = id
		}
	}
	return state
}

func applyStateToLease(state *State, lease *coordinationv1.Lease) {
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	setStamp(lease.Annotations, AnnotationLastModified, state.LastModified)
	setStamp(lease.Annotations, AnnotationLastGrow, state.LastGrow)
	setStamp(lease.Annotations, AnnotationLastShrink, state.LastShrink)
	setString(lease.Annotations, AnnotationPRDirection, state.PRDirection)
	setString(lease.Annotations, AnnotationWindow, state.Window)

	if state.PRID == 0 {
		lease.Spec.HolderIdentity = nil
		return
	}
	identity := fmt.Sprintf("pr-%d", state.PRID)
	lease.Spec.HolderIdentity = &identity
}

func parseStamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	stamp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return stamp
}

func setStamp(annotations map[string]string, key string, value time.Time) {
	if value.IsZero() {
		delete(annotations, key)
		return
	}
	annotations[key] = value.UTC().Format(time.RFC3339)
}

func setString(annotations map[string]string, key, value string) {
	if value == "" {
		delete(annotations, key)
		return
	}
	annotations[key] = value
}
