package testkit

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRequireDisposableContext(t *testing.T) {
	for name, allowed := range map[string]bool{
		"k3d-vela-demo": true,
		"kind-kind":     true,
		"":              false,
		"orbstack":      false,
		"arn:aws:eks:us-west-2:123456789012:cluster/prod": false,
		"my-k3d-lookalike": false,
	} {
		if err := RequireDisposableContext(name); (err == nil) != allowed {
			t.Errorf("context %q: allowed=%v, got error %v", name, allowed, err)
		}
	}
}

func resource(conditions ...map[string]any) *unstructured.Unstructured {
	list := make([]any, 0, len(conditions))
	for _, c := range conditions {
		list = append(list, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": list}}}
}

func condition(conditionType, status string) map[string]any {
	return map[string]any{"type": conditionType, "status": status}
}

func TestACKReady(t *testing.T) {
	cases := []struct {
		name   string
		obj    *unstructured.Unstructured
		ready  bool
		reason string
	}{
		{"no status yet", &unstructured.Unstructured{Object: map[string]any{}}, false, "ACK.ResourceSynced"},
		{"synced", resource(condition("ACK.ResourceSynced", "True")), true, ""},
		{"not synced", resource(condition("ACK.ResourceSynced", "False")), false, "ACK.ResourceSynced"},
		{"reconciling", resource(condition("ACK.ResourceSynced", "Unknown")), false, "ACK.ResourceSynced"},
		{"terminal false is fine", resource(condition("ACK.ResourceSynced", "True"), condition("ACK.Terminal", "False")), true, ""},
		// The controller gave up; Synced kept the value it had before.
		{"terminal with a stale synced", resource(condition("ACK.ResourceSynced", "True"), condition("ACK.Terminal", "True")), false, "ACK.Terminal"},
	}
	for _, tc := range cases {
		ready, reason := ACKReady(tc.obj)
		if ready != tc.ready || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: got ready=%v reason=%q, want ready=%v reason containing %q", tc.name, ready, reason, tc.ready, tc.reason)
		}
	}
}

func controller(image, endpoint string, available int32) *appsv1.Deployment {
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

func TestRequireACKOnEmulator(t *testing.T) {
	const image = "public.ecr.aws/aws-controllers-k8s/dynamodb-controller:1.16.0"
	cases := []struct {
		name       string
		groupFound bool
		deployment *appsv1.Deployment
		wantErr    string
	}{
		{"controller on the emulator", true, controller(image, AWSEmulatorEndpoint, 1), ""},
		{"controller not installed", false, nil, "is not installed"},
		// No endpoint override: this controller creates resources in AWS.
		{"controller on AWS", true, controller(image, "", 1), "refusing"},
		{"controller on another endpoint", true, controller(image, "http://floci.floci.svc:4566", 1), "refusing"},
		{"controller not running", true, controller(image, AWSEmulatorEndpoint, 0), "not running"},
		{"API without a controller", true, controller("example.com/other:1", AWSEmulatorEndpoint, 1), "no dynamodb-controller"},
	}
	for _, tc := range cases {
		kube := fake.NewClientset()
		if tc.deployment != nil {
			kube = fake.NewClientset(tc.deployment)
		}
		if tc.groupFound {
			kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
				GroupVersion: "dynamodb.services.k8s.aws/v1alpha1",
				APIResources: []metav1.APIResource{{Name: "tables", Kind: "Table", Namespaced: true}},
			}}
		}
		err := (&Cluster{Kube: kube}).RequireACKOnEmulator(context.Background(), "dynamodb")
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: got error %v, want one containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestACKInstallCommandPointsAtTheEmulator(t *testing.T) {
	command := ACKInstallCommand("s3", "1.2.3")
	for _, want := range []string{
		"oci://public.ecr.aws/aws-controllers-k8s/s3-chart --version 1.2.3",
		"aws.endpoint_url=" + AWSEmulatorEndpoint,
		"aws.allow_unsafe_aws_endpoint_urls=true",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("install command lacks %q:\n%s", want, command)
		}
	}
}
