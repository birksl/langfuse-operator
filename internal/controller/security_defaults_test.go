/*
Copyright 2026 bitkaio LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// CRD defaults are per-struct: they materialise as soon as spec.security is
// present, whatever the user was actually trying to configure in it. So adding
// an egress port turns on pod hardening as a side effect. That is only safe if
// the hardening defaults are a combination that actually runs — runAsNonRoot on
// its own is not, because the Langfuse images declare a non-numeric USER.
var _ = Describe("SecuritySpec defaulting", func() {
	const namespace = "default"

	newInstance := func(name string, security *v1alpha1.SecuritySpec) *v1alpha1.LangfuseInstance {
		return &v1alpha1.LangfuseInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: v1alpha1.LangfuseInstanceSpec{
				Image:    v1alpha1.ImageSpec{Tag: "3"},
				Auth:     v1alpha1.AuthSpec{NextAuthUrl: "http://langfuse.invalid"},
				Security: security,
			},
		}
	}

	roundTrip := func(instance *v1alpha1.LangfuseInstance) *v1alpha1.SecuritySpec {
		GinkgoHelper()
		Expect(k8sClient.Create(context.Background(), instance)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), instance)).To(Succeed())
		})

		stored := &v1alpha1.LangfuseInstance{}
		Expect(k8sClient.Get(context.Background(),
			types.NamespacedName{Name: instance.Name, Namespace: namespace}, stored)).To(Succeed())
		return stored.Spec.Security
	}

	It("pairs runAsUser with runAsNonRoot when only an egress port was configured", func() {
		security := roundTrip(newInstance("sec-netpol-only", &v1alpha1.SecuritySpec{
			NetworkPolicy: &v1alpha1.NetworkPolicySpec{
				ExtraEgressPorts: []v1alpha1.NetworkPolicyPort{{Port: 5433}},
			},
		}))

		Expect(security.RunAsNonRoot).NotTo(BeNil())
		Expect(*security.RunAsNonRoot).To(BeTrue(), "defaulted as a side effect of the sibling field")
		Expect(security.RunAsUser).NotTo(BeNil(),
			"runAsNonRoot defaulted on its own would be rejected by the kubelet")
		Expect(*security.RunAsUser).To(BeEquivalentTo(1001))
	})

	It("leaves the whole block unset when the user omits it", func() {
		Expect(roundTrip(newInstance("sec-absent", nil))).To(BeNil(),
			"no security context is applied at all in this case — see INVESTIGATION notes")
	})

	It("honours an explicit UID for custom image builds", func() {
		security := roundTrip(newInstance("sec-custom-uid", &v1alpha1.SecuritySpec{
			RunAsUser: ptrTo(int64(2000)),
		}))

		Expect(*security.RunAsUser).To(BeEquivalentTo(2000))
	})
})
