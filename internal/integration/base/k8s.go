// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration_k8s

package base

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/siderolabs/gen/xslices"
	"github.com/siderolabs/go-retry/retry"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/remotecommand"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/kubectl/pkg/scheme"

	taloskubernetes "github.com/siderolabs/talos/pkg/kubernetes"
)

// K8sSuite is a base suite for K8s tests.
type K8sSuite struct {
	APISuite

	Clientset       *kubernetes.Clientset
	DynamicClient   dynamic.Interface
	DiscoveryClient *discovery.DiscoveryClient
	RestConfig      *rest.Config
	Mapper          *restmapper.DeferredDiscoveryRESTMapper
}

// SetupSuite initializes Kubernetes client.
func (k8sSuite *K8sSuite) SetupSuite() {
	k8sSuite.APISuite.SetupSuite()

	kubeconfig, err := k8sSuite.Client.Kubeconfig(context.Background())
	k8sSuite.Require().NoError(err)

	config, err := clientcmd.BuildConfigFromKubeconfigGetter("", func() (*clientcmdapi.Config, error) {
		return clientcmd.Load(kubeconfig)
	})
	k8sSuite.Require().NoError(err)

	// patch timeout
	config.Timeout = time.Minute
	if k8sSuite.K8sEndpoint != "" {
		config.Host = k8sSuite.K8sEndpoint
	}

	k8sSuite.RestConfig = config
	k8sSuite.Clientset, err = kubernetes.NewForConfig(config)
	k8sSuite.Require().NoError(err)

	k8sSuite.DynamicClient, err = dynamic.NewForConfig(config)
	k8sSuite.Require().NoError(err)

	k8sSuite.DiscoveryClient, err = discovery.NewDiscoveryClientForConfig(config)
	k8sSuite.Require().NoError(err)

	k8sSuite.Mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(k8sSuite.DiscoveryClient))
}

// GetK8sNodeByInternalIP returns the kubernetes node by its internal ip or error if it is not found.
func (k8sSuite *K8sSuite) GetK8sNodeByInternalIP(ctx context.Context, internalIP string) (*corev1.Node, error) {
	nodeList, err := k8sSuite.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, item := range nodeList.Items {
		for _, address := range item.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				if address.Address == internalIP {
					return &item, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("node with internal IP %s not found", internalIP)
}

// WaitForK8sNodeReadinessStatus waits for node to have the given status.
// It retries until the node with the name is found and matches the expected condition.
func (k8sSuite *K8sSuite) WaitForK8sNodeReadinessStatus(ctx context.Context, nodeName string, checkFn func(corev1.ConditionStatus) bool) error {
	return retry.Constant(5 * time.Minute).Retry(func() error {
		readinessStatus, err := k8sSuite.GetK8sNodeReadinessStatus(ctx, nodeName)
		if errors.IsNotFound(err) {
			return retry.ExpectedError(err)
		}

		if taloskubernetes.IsRetryableError(err) {
			return retry.ExpectedError(err)
		}

		if err != nil {
			return err
		}

		if !checkFn(readinessStatus) {
			return retry.ExpectedErrorf("node readiness status is %s", readinessStatus)
		}

		return nil
	})
}

// GetK8sNodeReadinessStatus returns the node readiness status of the node.
func (k8sSuite *K8sSuite) GetK8sNodeReadinessStatus(ctx context.Context, nodeName string) (corev1.ConditionStatus, error) {
	node, err := k8sSuite.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status, nil
		}
	}

	return "", fmt.Errorf("node %s has no readiness condition", nodeName)
}

// DeleteResource deletes the resource with the given GroupVersionResource, namespace and name.
// Does not return an error if the resource is not found.
func (k8sSuite *K8sSuite) DeleteResource(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) error {
	err := k8sSuite.DynamicClient.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}

	return err
}

// EnsureResourceIsDeleted ensures that the resource with the given GroupVersionResource, namespace and name does not exist on Kubernetes.
// It repeatedly checks the resource for the given duration.
func (k8sSuite *K8sSuite) EnsureResourceIsDeleted(
	ctx context.Context,
	duration time.Duration,
	gvr schema.GroupVersionResource,
	ns, name string,
) error {
	return retry.Constant(duration).RetryWithContext(ctx, func(ctx context.Context) error {
		_, err := k8sSuite.DynamicClient.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}

		return err
	})
}

// WaitForEventExists waits for the event with the given namespace and check condition to exist on Kubernetes.
func (k8sSuite *K8sSuite) WaitForEventExists(ctx context.Context, ns string, checkFn func(event eventsv1.Event) bool) error {
	return retry.Constant(15*time.Second).RetryWithContext(ctx, func(ctx context.Context) error {
		events, err := k8sSuite.Clientset.EventsV1().Events(ns).List(ctx, metav1.ListOptions{})

		filteredEvents := xslices.Filter(events.Items, func(item eventsv1.Event) bool {
			return checkFn(item)
		})

		if len(filteredEvents) == 0 {
			return retry.ExpectedError(err)
		}

		return nil
	})
}

// WaitForPodToBeRunning waits for the pod with the given namespace and name to be running.
func (k8sSuite *K8sSuite) WaitForPodToBeRunning(ctx context.Context, timeout time.Duration, namespace, podName string) error {
	return retry.Constant(timeout, retry.WithUnits(time.Second*10)).Retry(
		func() error {
			pod, err := k8sSuite.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return retry.ExpectedErrorf("error getting pod: %s", err)
			}

			if pod.Status.Phase != corev1.PodRunning {
				return retry.ExpectedErrorf("pod is not running yet: %s", pod.Status.Phase)
			}

			return nil
		},
	)
}

// ExecuteCommandInPod executes the given command in the pod with the given namespace and name.
func (k8sSuite *K8sSuite) ExecuteCommandInPod(ctx context.Context, namespace, podName, command string) (string, string, error) {
	cmd := []string{
		"/bin/sh",
		"-c",
		command,
	}
	req := k8sSuite.Clientset.CoreV1().RESTClient().Post().Resource("pods").Name(podName).
		Namespace(namespace).SubResource("exec")
	option := &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   false,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	exec, err := remotecommand.NewSPDYExecutor(k8sSuite.RestConfig, "POST", req.URL())
	if err != nil {
		return "", "", err
	}

	var stdout, stderr bytes.Buffer

	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", "", err
	}

	return stdout.String(), stderr.String(), nil
}

// GetPodsWithLabel returns the pods with the given label in the specified namespace.
func (k8sSuite *K8sSuite) GetPodsWithLabel(ctx context.Context, namespace, label string) (*corev1.PodList, error) {
	podList, err := k8sSuite.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return nil, err
	}

	return podList, nil
}

// ParseManifests parses YAML manifest bytes into unstructured objects.
func (k8sSuite *K8sSuite) ParseManifests(manifests []byte) []unstructured.Unstructured {
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifests)))

	var parsedManifests []unstructured.Unstructured

	for {
		yamlManifest, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}

			k8sSuite.Require().NoError(err)
		}

		yamlManifest = bytes.TrimSpace(yamlManifest)

		if len(yamlManifest) == 0 {
			continue
		}

		jsonManifest, err := yaml.ToJSON(yamlManifest)
		if err != nil {
			k8sSuite.Require().NoError(err, "error converting manifest to JSON")
		}

		if bytes.Equal(jsonManifest, []byte("null")) || bytes.Equal(jsonManifest, []byte("{}")) {
			// skip YAML docs which contain only comments
			continue
		}

		var obj unstructured.Unstructured

		if err = json.Unmarshal(jsonManifest, &obj); err != nil {
			k8sSuite.Require().NoError(err, "error loading JSON manifest into unstructured")
		}

		parsedManifests = append(parsedManifests, obj)
	}

	return parsedManifests
}

// ApplyManifests applies the given manifests to the Kubernetes cluster.
func (k8sSuite *K8sSuite) ApplyManifests(ctx context.Context, manifests []unstructured.Unstructured) {
	for _, obj := range manifests {
		mapping, err := k8sSuite.Mapper.RESTMapping(obj.GetObjectKind().GroupVersionKind().GroupKind(), obj.GetObjectKind().GroupVersionKind().Version)
		if err != nil {
			k8sSuite.Require().NoError(err, "error creating mapping for object %s", obj.GetName())
		}

		dr := k8sSuite.DynamicClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())

		_, err = dr.Create(ctx, &obj, metav1.CreateOptions{})
		k8sSuite.Require().NoError(err, "error creating object %s", obj.GetName())

		k8sSuite.T().Logf("created object %s/%s/%s", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName())
	}
}

// DeleteManifests deletes the given manifests from the Kubernetes cluster.
func (k8sSuite *K8sSuite) DeleteManifests(ctx context.Context, manifests []unstructured.Unstructured) {
	// process in reverse orderd
	manifests = slices.Clone(manifests)
	slices.Reverse(manifests)

	for _, obj := range manifests {
		mapping, err := k8sSuite.Mapper.RESTMapping(obj.GetObjectKind().GroupVersionKind().GroupKind(), obj.GetObjectKind().GroupVersionKind().Version)
		if err != nil {
			k8sSuite.Require().NoError(err, "error creating mapping for object %s", obj.GetName())
		}

		dr := k8sSuite.DynamicClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())

		err = dr.Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
		if errors.IsNotFound(err) {
			continue
		}

		k8sSuite.Require().NoError(err, "error deleting object %s", obj.GetName())

		// wait for the object to be deleted
		fieldSelector := fields.OneTermEqualSelector("metadata.name", obj.GetName()).String()
		lw := &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = fieldSelector

				return dr.List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = fieldSelector

				return dr.Watch(ctx, options)
			},
		}

		preconditionFunc := func(store cache.Store) (bool, error) {
			var exists bool

			_, exists, err = store.Get(&metav1.ObjectMeta{Namespace: obj.GetNamespace(), Name: obj.GetName()})
			if err != nil {
				return true, err
			}

			if !exists {
				// since we're looking for it to disappear we just return here if it no longer exists
				return true, nil
			}

			return false, nil
		}

		_, err = watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, preconditionFunc, func(event watch.Event) (bool, error) {
			return event.Type == watch.Deleted, nil
		})

		k8sSuite.Require().NoError(err, "error waiting for the object to be deleted %s", obj.GetName())

		k8sSuite.T().Logf("deleted object %s/%s/%s", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName())
	}
}

// ToUnstructured converts the given runtime.Object to unstructured.Unstructured.
func (k8sSuite *K8sSuite) ToUnstructured(obj runtime.Object) unstructured.Unstructured {
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		k8sSuite.Require().NoError(err, "error converting object to unstructured")
	}

	u := unstructured.Unstructured{Object: unstructuredObj}
	u.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())

	return u
}
