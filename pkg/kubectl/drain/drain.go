/*
Copyright 2019 The Kubernetes Authors.

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

package drain

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/genericclioptions/resource"
	"k8s.io/client-go/kubernetes"
)

const (
	EvictionKind        = "Eviction"
	EvictionSubresource = "pods/eviction"

	daemonsetFatal      = "DaemonSet-managed Pods (use --ignore-daemonsets to ignore)"
	daemonsetWarning    = "ignoring DaemonSet-managed Pods"
	localStorageFatal   = "Pods with local storage (use --delete-local-data to override)"
	localStorageWarning = "deleting Pods with local storage"
	unmanagedFatal      = "Pods not managed by ReplicationController, ReplicaSet, Job, DaemonSet or StatefulSet (use --force to override)"
	unmanagedWarning    = "deleting Pods not managed by ReplicationController, ReplicaSet, Job, DaemonSet or StatefulSet"
)

type Options struct {
	Client             kubernetes.Interface
	Force              bool
	DryRun             bool
	GracePeriodSeconds int
	IgnoreDaemonsets   bool
	Timeout            time.Duration
	DeleteLocalData    bool
	Selector           string
	PodSelector        string
	ErrOut             io.Writer
}

// Takes a pod and returns a bool indicating whether or not to operate on the
// pod, an optional warning message, and an optional fatal error.
type podFilter func(corev1.Pod) (include bool, w *warning, f *fatal)

type warning struct{ string }
type fatal struct{ string }

// SupportEviction uses Discovery API to find out if the server support eviction subresource
// If support, it will return its groupVersion; Otherwise, it will return ""
func SupportEviction(clientset kubernetes.Interface) (string, error) {
	discoveryClient := clientset.Discovery()
	groupList, err := discoveryClient.ServerGroups()
	if err != nil {
		return "", err
	}
	foundPolicyGroup := false
	var policyGroupVersion string
	for _, group := range groupList.Groups {
		if group.Name == "policy" {
			foundPolicyGroup = true
			policyGroupVersion = group.PreferredVersion.GroupVersion
			break
		}
	}
	if !foundPolicyGroup {
		return "", nil
	}
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion("v1")
	if err != nil {
		return "", err
	}
	for _, resource := range resourceList.APIResources {
		if resource.Name == EvictionSubresource && resource.Kind == EvictionKind {
			return policyGroupVersion, nil
		}
	}
	return "", nil
}

func (o *Options) DeletePod(pod corev1.Pod) error {
	deleteOptions := &metav1.DeleteOptions{}
	if o.GracePeriodSeconds >= 0 {
		gracePeriodSeconds := int64(o.GracePeriodSeconds)
		deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
	}
	return o.Client.CoreV1().Pods(pod.Namespace).Delete(pod.Name, deleteOptions)
}

func (o *Options) EvictPod(pod corev1.Pod, policyGroupVersion string) error {
	deleteOptions := &metav1.DeleteOptions{}
	if o.GracePeriodSeconds >= 0 {
		gracePeriodSeconds := int64(o.GracePeriodSeconds)
		deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
	}
	eviction := &policyv1beta1.Eviction{
		TypeMeta: metav1.TypeMeta{
			APIVersion: policyGroupVersion,
			Kind:       EvictionKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: deleteOptions,
	}
	// Remember to change change the URL manipulation func when Evction's version change
	return o.Client.PolicyV1beta1().Evictions(eviction.Namespace).Evict(eviction)
}

// getPodsForDeletion receives resource info for a node, and returns all the pods from the given node that we
// are planning on deleting. If there are any pods preventing us from deleting, we return that list in an error.
func (o *Options) GetPodsForDeletion(nodeInfo *resource.Info) (pods []corev1.Pod, err error) {
	labelSelector, err := labels.Parse(o.PodSelector)
	if err != nil {
		return pods, err
	}

	podList, err := o.Client.CoreV1().Pods(metav1.NamespaceAll).List(metav1.ListOptions{
		LabelSelector: labelSelector.String(),
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeInfo.Name}).String()})
	if err != nil {
		return pods, err
	}

	ws := podStatuses{}
	fs := podStatuses{}

	for _, pod := range podList.Items {
		podOk := true
		for _, filt := range []podFilter{o.daemonsetFilter, mirrorPodFilter, o.localStorageFilter, o.unreplicatedFilter} {
			filterOk, w, f := filt(pod)

			podOk = podOk && filterOk
			if w != nil {
				ws[w.string] = append(ws[w.string], pod.Name)
			}
			if f != nil {
				fs[f.string] = append(fs[f.string], pod.Name)
			}

			// short-circuit as soon as pod not ok
			// at that point, there is no reason to run pod
			// through any additional filters
			if !podOk {
				break
			}
		}
		if podOk {
			pods = append(pods, pod)
		}
	}

	if len(fs) > 0 {
		return []corev1.Pod{}, errors.New(fs.Message())
	}
	if len(ws) > 0 {
		fmt.Fprintf(o.ErrOut, "WARNING: %s\n", ws.Message())
	}
	return pods, nil
}

func (o *Options) localStorageFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	if !hasLocalStorage(pod) {
		return true, nil, nil
	}
	// Any finished pod can be removed.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, nil, nil
	}
	if !o.DeleteLocalData {
		return false, nil, &fatal{localStorageFatal}
	}

	return true, &warning{localStorageWarning}, nil
}

func (o *Options) unreplicatedFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	// any finished pod can be removed
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, nil, nil
	}

	controllerRef := getPodController(pod)
	if controllerRef != nil {
		return true, nil, nil
	}
	if o.Force {
		return true, &warning{unmanagedWarning}, nil
	}

	return false, nil, &fatal{unmanagedFatal}
}

func (o *Options) daemonsetFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	// Note that we return false in cases where the pod is DaemonSet managed,
	// regardless of flags.
	//
	// The exception is for pods that are orphaned (the referencing
	// management resource - including DaemonSet - is not found).
	// Such pods will be deleted if --force is used.
	controllerRef := getPodController(pod)
	if controllerRef == nil || controllerRef.Kind != "DaemonSet" {
		return true, nil, nil
	}
	// Any finished pod can be removed.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, nil, nil
	}

	if _, err := o.Client.AppsV1().DaemonSets(pod.Namespace).Get(controllerRef.Name, metav1.GetOptions{}); err != nil {
		// remove orphaned pods with a warning if --force is used
		if apierrors.IsNotFound(err) && o.Force {
			return true, &warning{err.Error()}, nil
		}

		return false, nil, &fatal{err.Error()}
	}

	if !o.IgnoreDaemonsets {
		return false, nil, &fatal{daemonsetFatal}
	}

	return false, &warning{daemonsetWarning}, nil
}

func getPodController(pod corev1.Pod) *metav1.OwnerReference {
	return metav1.GetControllerOf(&pod)
}

func mirrorPodFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	if _, found := pod.ObjectMeta.Annotations[corev1.MirrorPodAnnotationKey]; found {
		return false, nil, nil
	}
	return true, nil, nil
}

func hasLocalStorage(pod corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir != nil {
			return true
		}
	}

	return false
}

// Map of status message to a list of pod names having that status.
type podStatuses map[string][]string

func (ps podStatuses) Message() string {
	msgs := []string{}

	for key, pods := range ps {
		msgs = append(msgs, fmt.Sprintf("%s: %s", key, strings.Join(pods, ", ")))
	}
	return strings.Join(msgs, "; ")
}
