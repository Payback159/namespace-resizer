package git

import (
	"context"
	"sync"
	"testing"

	. "github.com/onsi/gomega"
)

// TestStatefulLogProvider_ConcurrentAccess exercises the provider from many
// goroutines. Run with -race, it fails if the internal map is not synchronized.
func TestStatefulLogProvider_ConcurrentAccess(t *testing.T) {
	g := NewWithT(t)
	p := NewStatefulLogProvider()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := p.CreatePR(ctx, "quota", "ns", DirectionGrow, nil, nil)
			if err != nil {
				return
			}
			_, _ = p.GetPRStatus(ctx, id)
			_, _, _ = p.FindOpenPR(ctx, "ns", "quota")
			_ = p.MergePR(ctx, id, "squash")
			_, _ = p.GetPRStatus(ctx, id)
		}()
	}
	wg.Wait()

	// After merging, no open PR should remain for the ns/quota.
	openID, _, err := p.FindOpenPR(ctx, "ns", "quota")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(openID).To(Equal(0))
}
