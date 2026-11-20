// Package testkit prepares a disposable test cluster for tests that run cloud
// controllers against an emulator, and gives those tests a few helpers.
//
// It only works on k3d and kind clusters. A controller takes its endpoint when
// it is installed, so pointing one at an emulator is something to do on a
// cluster that is thrown away, never on one that manages real cloud resources.
//
// Every function returns an error instead of failing a test, so the package
// works with Ginkgo, with the standard testing package or from a program.
package testkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager is the server-side apply owner of everything this package applies.
const FieldManager = "cloud-emulators-testkit"

// pollInterval is how often the waits in this package look again.
const pollInterval = 2 * time.Second

// Cluster is a connection to a disposable test cluster.
type Cluster struct {
	// Context is the kubeconfig context the connection uses.
	Context string
	Config  *rest.Config
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface

	mapper meta.RESTMapper
}

// Connect loads the current kubeconfig context, from KUBECONFIG or the default
// location, and refuses it unless it is a local k3d or kind cluster; see
// RequireDisposableCluster.
func Connect() (*Cluster, error) {
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if err := RequireDisposableCluster(raw.CurrentContext, config.Host); err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Cluster{
		Context: raw.CurrentContext,
		Config:  config,
		Kube:    kube,
		Dynamic: dyn,
		mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(kube.Discovery())),
	}, nil
}

// RequireDisposableCluster returns an error unless the kubeconfig context is
// named like a k3d or kind cluster and its API server is on this machine.
//
// The name alone proves nothing: anyone can call a context "kind-prod". So the
// server address must also be one that only a local cluster has: a loopback or
// unspecified address, localhost, host.docker.internal, or the node container
// that k3d or kind creates for a cluster of that name. A cluster reached at any
// other address is refused, even if it is in fact disposable.
func RequireDisposableCluster(contextName, server string) error {
	var tool, name string
	switch {
	case strings.HasPrefix(contextName, "k3d-"):
		tool, name = "k3d", strings.TrimPrefix(contextName, "k3d-")
	case strings.HasPrefix(contextName, "kind-"):
		tool, name = "kind", strings.TrimPrefix(contextName, "kind-")
	default:
		return fmt.Errorf("context %q is not a k3d or kind cluster: emulator tests only run on a disposable cluster", contextName)
	}

	parsed, err := url.Parse(server)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("context %q has no usable API server address (%q)", contextName, server)
	}
	host := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return nil
		}
	} else {
		local := []string{"localhost", "host.docker.internal", "kubernetes.docker.internal"}
		if tool == "kind" {
			local = append(local, name+"-control-plane")
		} else {
			local = append(local, "k3d-"+name+"-serverlb", "k3d-"+name+"-server-0")
		}
		if slices.Contains(local, host) || strings.HasSuffix(host, ".localhost") {
			return nil
		}
	}
	return fmt.Errorf("context %q is named like a %s cluster but its API server %q is not on this machine: emulator tests only run on a local disposable cluster",
		contextName, tool, parsed.Host)
}

// Apply server-side applies one object of any kind.
func (c *Cluster) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	var client dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		client = c.Dynamic.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	}
	if _, err := client.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// ApplyManifest applies every object of a YAML or JSON stream, in order.
func (c *Cluster) ApplyManifest(ctx context.Context, manifest io.Reader) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(manifest, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read manifest: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Apply(ctx, obj); err != nil {
			return err
		}
	}
}

// Run is one test run's own corner of a shared cluster and a shared emulator.
type Run struct {
	// ID is unique to the run. Prefix names in the cloud service with it, so two
	// runs against the same emulator do not collide.
	ID string
	// Namespace exists only for this run.
	Namespace string
}

// NewRun creates a namespace for one test run. name says what the run tests,
// such as "dynamodb".
func (c *Cluster) NewRun(ctx context.Context, name string) (*Run, error) {
	run := &Run{ID: "e2e-" + rand.String(5)}
	run.Namespace = name + "-" + run.ID
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: run.Namespace}}
	if _, err := c.Kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", run.Namespace, err)
	}
	return run, nil
}

// EndRun deletes what the run created: every object of the owners in its
// namespace, then, once the controllers have removed every object of the
// dependents, the namespace. owners are what the test applied, such as KubeVela
// Applications; dependents are what a controller made from them and must clean
// up in the cloud service, such as ACK tables. Deleting the namespace first
// would leave those in the emulator.
func (c *Cluster) EndRun(ctx context.Context, run *Run, owners, dependents []schema.GroupVersionResource) error {
	for _, gvr := range owners {
		err := c.Dynamic.Resource(gvr).Namespace(run.Namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("delete %s in %s: %w", gvr.Resource, run.Namespace, err)
		}
	}
	for _, gvr := range append(append([]schema.GroupVersionResource{}, owners...), dependents...) {
		err := wait(ctx, func() (bool, error) {
			list, err := c.Dynamic.Resource(gvr).Namespace(run.Namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			return len(list.Items) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("waiting for %s in %s to be deleted: %w", gvr.Resource, run.Namespace, err)
		}
	}
	return c.Kube.CoreV1().Namespaces().Delete(ctx, run.Namespace, metav1.DeleteOptions{})
}

// wait polls until done reports true, done fails, or ctx ends.
func wait(ctx context.Context, done func() (bool, error)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
