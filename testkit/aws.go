package testkit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	// AWSEmulatorNamespace and AWSEmulatorName are fixed: a cluster has one
	// emulator, because a controller has one endpoint.
	AWSEmulatorNamespace = "aws-emulator"
	AWSEmulatorName      = "aws-emulator"
	// AWSEmulatorPort is the plain-HTTP port of the emulator's Service.
	AWSEmulatorPort = 4566
	// AWSEmulatorEndpoint is what a controller in the cluster is installed with.
	AWSEmulatorEndpoint = "http://aws-emulator.aws-emulator.svc.cluster.local:4566"
	// AWSEmulatorRegion is the region the examples and the install command use.
	// The emulator accepts any region.
	AWSEmulatorRegion = "us-west-2"

	// AWSEmulatorAccessKeyID and AWSEmulatorSecretAccessKey are made-up keys for
	// clients. The emulator runs with signing checks off and accepts anything.
	AWSEmulatorAccessKeyID     = "emulator-not-a-real-key"
	AWSEmulatorSecretAccessKey = "emulator-not-a-real-secret"

	awsEmulatorManifest = "https://github.com/kubevela/cloud-emulators/releases/download/aws-emulator-v%[1]s/aws-emulator-%[1]s.yaml"
)

// EnsureAWSEmulator makes sure an AWS Emulator is running on the cluster. One
// that is already there, with its Service, is used as it is, whatever its
// version: the emulator is shared by every test on the cluster. When the
// Deployment or the Service is missing, the manifest of the given
// kubevela/cloud-emulators release is applied; it names the image by digest. The call returns once the emulator is available, or when ctx ends.
func (c *Cluster) EnsureAWSEmulator(ctx context.Context, version string) error {
	deployments := c.Kube.AppsV1().Deployments(AWSEmulatorNamespace)
	_, err := deployments.Get(ctx, AWSEmulatorName, metav1.GetOptions{})
	if err == nil {
		// A Deployment without its Service is what an apply that stopped half way
		// leaves behind: nothing can reach that emulator, so it is repaired below.
		_, err = c.Kube.CoreV1().Services(AWSEmulatorNamespace).Get(ctx, AWSEmulatorName, metav1.GetOptions{})
	}
	switch {
	case apierrors.IsNotFound(err):
		url := fmt.Sprintf(awsEmulatorManifest, version)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download %s: %s", url, resp.Status)
		}
		if err := c.ApplyManifest(ctx, resp.Body); err != nil {
			return err
		}
	case err != nil:
		return err
	}

	err = wait(ctx, func() (bool, error) {
		d, err := deployments.Get(ctx, AWSEmulatorName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return d.Status.AvailableReplicas >= 1, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the AWS Emulator to be available: %w", err)
	}
	return nil
}

var (
	// An ACK service name is the lower-case AWS service identifier used in chart
	// and image names, such as "dynamodb" or "s3".
	ackServiceName = regexp.MustCompile(`^[a-z][a-z0-9]{0,62}$`)
	chartVersion   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,62}$`)
)

// ACKInstallCommand is the shell command that installs the ACK controller of
// an AWS service pointed at the emulator. version is the chart version; look
// up the latest one, do not guess it. Both arguments end up in a command a
// person pastes into a shell, so anything but a plain service name and a plain
// version is an error.
func ACKInstallCommand(service, version string) (string, error) {
	if !ackServiceName.MatchString(service) {
		return "", fmt.Errorf("%q is not an ACK service name such as dynamodb", service)
	}
	if !chartVersion.MatchString(version) {
		return "", fmt.Errorf("%q is not a chart version such as 1.16.0", version)
	}
	return ackInstallCommand(service, version), nil
}

func ackInstallCommand(service, version string) string {
	return fmt.Sprintf(`kubectl create namespace ack-system
kubectl -n ack-system create secret generic ack-emulator-credentials \
  --from-literal=credentials=$'[default]\naws_access_key_id = %[3]s\naws_secret_access_key = %[4]s\n'
helm install ack-%[1]s oci://public.ecr.aws/aws-controllers-k8s/%[1]s-chart --version %[2]s -n ack-system \
  --set aws.region=%[5]s --set aws.endpoint_url=%[6]s \
  --set aws.allow_unsafe_aws_endpoint_urls=true \
  --set aws.credentials.secretName=ack-emulator-credentials`,
		service, version, AWSEmulatorAccessKeyID, AWSEmulatorSecretAccessKey, AWSEmulatorRegion, AWSEmulatorEndpoint)
}

// RequireACKOnEmulator returns an error unless the ACK controller of an AWS
// service, such as "dynamodb", is installed, running, and configured with the
// emulator as its endpoint. A controller with another endpoint, or with none,
// talks to AWS: tests must never create resources through it, so this is the
// check to run before the first one. When the controller is not installed at
// all, the error carries the command that installs it.
func (c *Cluster) RequireACKOnEmulator(ctx context.Context, service string) error {
	if !ackServiceName.MatchString(service) {
		return fmt.Errorf("%q is not an ACK service name such as dynamodb", service)
	}
	group := service + ".services.k8s.aws"
	if _, err := c.Kube.Discovery().ServerResourcesForGroupVersion(group + "/v1alpha1"); err != nil {
		return fmt.Errorf("the ACK %s controller is not installed (no API group %s). Install it pointing at the emulator:\n%s",
			service, group, ackInstallCommand(service, "<chart version>"))
	}

	all, err := c.Kube.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	found := false
	for _, d := range all.Items {
		for _, container := range d.Spec.Template.Spec.Containers {
			if !strings.Contains(container.Image, "/"+service+"-controller") {
				continue
			}
			found = true
			endpoint := ""
			for _, env := range container.Env {
				if env.Name == "AWS_ENDPOINT_URL" {
					endpoint = env.Value
				}
			}
			if endpoint != AWSEmulatorEndpoint {
				return fmt.Errorf("ACK %s controller %s/%s has endpoint %q, not the emulator's %q: refusing to create resources through it",
					service, d.Namespace, d.Name, endpoint, AWSEmulatorEndpoint)
			}
			if d.Status.AvailableReplicas < 1 {
				return fmt.Errorf("ACK %s controller %s/%s is not running", service, d.Namespace, d.Name)
			}
		}
	}
	if !found {
		return fmt.Errorf("the ACK %s API is served but no %s-controller deployment was found", service, service)
	}
	return nil
}

// ForwardAWSEmulator forwards a free local port to the emulator and returns
// the endpoint URL for an AWS client in the test process, with a function that
// closes the connection.
func (c *Cluster) ForwardAWSEmulator(ctx context.Context) (endpoint string, stop func(), err error) {
	pods, err := c.Kube.CoreV1().Pods(AWSEmulatorNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + AWSEmulatorName,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return "", nil, err
	}
	if len(pods.Items) == 0 {
		return "", nil, fmt.Errorf("no running AWS Emulator pod in namespace %s", AWSEmulatorNamespace)
	}

	transport, upgrader, err := spdy.RoundTripperFor(c.Config)
	if err != nil {
		return "", nil, err
	}
	url := c.Kube.CoreV1().RESTClient().Post().Resource("pods").
		Namespace(AWSEmulatorNamespace).Name(pods.Items[0].Name).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)

	stopCh := make(chan struct{})
	ready := make(chan struct{})
	failed := make(chan error, 1)
	forwarder, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", AWSEmulatorPort)}, stopCh, ready, io.Discard, os.Stderr)
	if err != nil {
		return "", nil, err
	}
	go func() { failed <- forwarder.ForwardPorts() }()

	select {
	case <-ready:
	case err := <-failed:
		return "", nil, fmt.Errorf("port-forward to the AWS Emulator: %w", err)
	case <-ctx.Done():
		close(stopCh)
		return "", nil, ctx.Err()
	}
	ports, err := forwarder.GetPorts()
	if err != nil {
		close(stopCh)
		return "", nil, err
	}
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local), func() { close(stopCh) }, nil
}

// ACKReady reports whether an ACK resource is reconciled: its
// ACK.ResourceSynced condition is True and its ACK.Terminal condition is not.
// Terminal means the controller gave up, and Synced keeps its last value then,
// so both are read together. The reason says what is missing.
//
// ACK conditions carry no observedGeneration. Right after a change to the
// spec, a True can be the answer to the previous one; read the service's own
// API to prove a change arrived.
func ACKReady(obj *unstructured.Unstructured) (ready bool, reason string) {
	synced := ACKCondition(obj, "ACK.ResourceSynced")
	terminal := ACKCondition(obj, "ACK.Terminal")
	switch {
	case terminal == "True":
		return false, "ACK.Terminal is True: the controller stopped reconciling this resource"
	case synced != "True":
		return false, fmt.Sprintf("ACK.ResourceSynced is %q", synced)
	}
	return true, ""
}

// ACKCondition returns the status of one condition of an ACK resource, or ""
// when the resource does not have it.
func ACKCondition(obj *unstructured.Unstructured, conditionType string) string {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok || condition["type"] != conditionType {
			continue
		}
		status, _ := condition["status"].(string)
		return status
	}
	return ""
}
