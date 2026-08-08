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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
)

var _ = Describe("ResourceQuota Controller", func() {
	Context("When reconciling a resource", func() {

		It("should successfully reconcile the resource", func() {
			// See internal/controller/event_analysis_test.go for logic tests.
			// Integration tests can be added here.
		})
	})
})

func TestReconcile_GrowUsesHeadroomFromLegacyThreshold(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-a",
			Annotations: map[string]string{
				"resizer.io/cpu-threshold": "80",
			},
		},
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "compute",
			Namespace: "team-a",
			UID:       types.UID("uid-1"),
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

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(c)
	provider := &FakeGitProvider{CreatePRID: 5}

	reconciler := &ResourceQuotaReconciler{
		Client:      c,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(10),
		GitProvider: provider,
		Locker:      locker,
		Observer:    NewObserver(locker, time.Now),
		BasePolicy:  sizing.DefaultPolicy(),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	// used == hard, so target = 10 * 1.25 = 12.5, well above the band.
	g.Expect(provider.CreatePRCalls).To(Equal(1))
	g.Expect(provider.LastLimits).To(HaveKey(corev1.ResourceRequestsCPU))
	g.Expect(provider.LastDirection).To(Equal("grow"))
}

func TestReconcile_QuietInsideToleranceBand(t *testing.T) {
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
				corev1.ResourceRequestsCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("8"),
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, quota).Build()
	locker := lock.NewLeaseLocker(c)
	provider := &FakeGitProvider{}

	reconciler := &ResourceQuotaReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		GitProvider: provider, Locker: locker,
		Observer:   NewObserver(locker, time.Now),
		BasePolicy: sizing.DefaultPolicy(),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "compute", Namespace: "team-a"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	// target = 8 * 1.25 = 10, exactly the current hard value: no action.
	g.Expect(provider.CreatePRCalls).To(Equal(0))
}
