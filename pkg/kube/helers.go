/*
Copyright 2021 The Cockroach Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kube

import (
	"context"
	"fmt"
	"time"

	k8sErrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/cenkalti/backoff"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func Get(ctx context.Context, cl client.Client, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)

	return cl.Get(ctx, key, obj)
}

type PersistFn func(context.Context, client.Client, client.Object, MutateFn) (upserted bool, err error)

var DefaultPersister PersistFn = func(ctx context.Context, cl client.Client, obj client.Object, f MutateFn) (upserted bool, err error) {
	result, err := ctrl.CreateOrUpdate(ctx, cl, obj, func() error {
		return f()
	})

	return result == ctrlutil.OperationResultCreated || result == ctrlutil.OperationResultUpdated, err
}

// MutateFn is a function which mutates the existing object into it's desired state.
type MutateFn func() error

// TODO this code is from https://github.com/kubernetes/kubernetes/blob/master/pkg/api/v1/pod/util.go
// We need to determine if this functionality is available via the client-go

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *corev1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status corev1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == corev1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status corev1.PodStatus) *corev1.PodCondition {
	_, condition := GetPodCondition(&status, corev1.PodReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *corev1.PodStatus, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

// WaitUntilAllStsPodsAreReady waits until all pods in the statefulset are in the
// ready state. The ready state implies all nodes are passing node liveness.
func WaitUntilAllStsPodsAreReady(ctx context.Context, clientset *kubernetes.Clientset, l logr.Logger, stsname, stsnamespace string,
	podUpdateTimeout, podMaxPollingInterval time.Duration) error {
	l.V(int(zapcore.DebugLevel)).Info("waiting until all pods are in the ready state")
	f := func() error {
		sts, err := clientset.AppsV1().StatefulSets(stsnamespace).Get(ctx, stsname, metav1.GetOptions{})
		if err != nil {
			return HandleStsError(err, l, stsname, stsnamespace)
		}
		got := int(sts.Status.ReadyReplicas)
		// TODO need to test this
		// we could also use the number of pods defined by the operator
		numCRDBPods := int(sts.Status.Replicas)
		if got != numCRDBPods {
			l.Error(err, fmt.Sprintf("number of ready replicas is %v, not equal to num CRDB pods %v", got, numCRDBPods))
			return err
		}

		l.V(int(zapcore.DebugLevel)).Info("all replicas are ready makeWaitUntilAllPodsReadyFunc update_cockroach_version.go")
		return nil
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = podUpdateTimeout
	b.MaxInterval = podMaxPollingInterval
	return backoff.Retry(f, b)
}

func HandleStsError(err error, l logr.Logger, stsName string, ns string) error {
	if k8sErrors.IsNotFound(err) {
		l.Error(err, "sts is not found", "stsName", stsName, "namespace", ns)
		return errors.Wrapf(err, "sts is not found: %s ns: %s", stsName, ns)
	} else if statusError, isStatus := err.(*k8sErrors.StatusError); isStatus {
		l.Error(statusError, fmt.Sprintf("Error getting statefulset %v", statusError.ErrStatus.Message), "stsName", stsName, "namespace", ns)
		return statusError
	}
	l.Error(err, "error getting statefulset", "stsName", stsName, "namspace", ns)
	return err
}
