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
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/google/martian/log"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type PersistFn func(context.Context, client.Client, client.Object, MutateFn) (upserted bool, err error)

var DefaultPersister PersistFn = func(ctx context.Context, cl client.Client, obj client.Object, f MutateFn) (upserted bool, err error) {
	result, err := ctrl.CreateOrUpdate(ctx, cl, obj, func() error {
		return f()
	})

	return result == ctrlutil.OperationResultCreated || result == ctrlutil.OperationResultUpdated, err
}

// MutateFn is a function which mutates the existing object into it's desired state.
type MutateFn func() error

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
func WaitUntilAllStsPodsAreReady(ctx context.Context, cl client.Client, stsName, namespace string, podUpdateTimeout, podMaxPollingInterval time.Duration) error {
	logrus.Info("Waiting until all pods are in the ready state")
	f := func() error {
		var sts v1.StatefulSet
		if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: stsName}, &sts); err != nil {
			return err
		}

		got := int(sts.Status.ReadyReplicas)
		numCRDBPods := int(sts.Status.Replicas)
		if got != numCRDBPods {
			logrus.Errorf("Number of ready replicas found [%v], expected number of ready replicas [%v]", got, numCRDBPods)
			return fmt.Errorf("Replicas are not equal")
		}

		logrus.Info("All replicas are ready")
		return nil
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = podUpdateTimeout
	b.MaxInterval = podMaxPollingInterval
	return backoff.Retry(f, b)
}

func RollingUpdate(ctx context.Context, cl client.Client, stsName, namespace string, readinessWait, podUpdateTimeout time.Duration) error {
	var sts v1.StatefulSet
	if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: stsName}, &sts); err != nil {
		return err
	}

	logrus.Info("Performing rolling update after certificate rotation")
	for i := int32(0); i < sts.Status.Replicas; i++ {
		replicaName := stsName + "-" + strconv.Itoa(int(i))
		replica := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      replicaName,
				Namespace: namespace,
			},
		}

		if err := cl.Delete(ctx, replica); err != nil {
			log.Errorf("Failed to delete the statefulset replica [%s]", replicaName)
			return err
		}

		time.Sleep(5 * time.Second)
		if err := WaitForPodReady(ctx, cl, replicaName, namespace, podUpdateTimeout, 5*time.Second); err != nil {
			return err
		}

		// sleep for readinessWait period for the pod to become stable and ready
		logrus.Infof("waiting for %s duration for pod readiness", readinessWait.String())
		time.Sleep(readinessWait)
	}

	// extra safe side check for all replicas to come in available state
	if err := WaitUntilAllStsPodsAreReady(ctx, cl, stsName, namespace, podUpdateTimeout, 5*time.Second); err != nil {
		return err
	}
	return nil
}

// SighupSignalToPods sends SIGHUP signal to all the pods in the statefulset.
func SighupSignalToPods(ctx context.Context, config *rest.Config, cl client.Client, stsName, namespace string) error {

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	var sts v1.StatefulSet
	if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: stsName}, &sts); err != nil {
		return err
	}

	containerName := sts.Spec.Template.Spec.Containers[0].Name
	command := []string{"/bin/bash", "-c", "echo 'Send SIGHUP to cockroach'; kill -s 1 $(ls -l /proc/*/exe | grep cockroach | awk '{print $2}')"}
	for i := int32(0); i < sts.Status.Replicas; i++ {
		replicaName := stsName + "-" + strconv.Itoa(int(i))

		stdout, stderr, err := execCommandInPod(clientset, config, namespace, replicaName, containerName, command)
		if err != nil {
			logrus.Errorf("Failed to send SIGHUP signal to pod [%s], error: %v, stdout: %s, stderr: %s", replicaName, err, stdout, stderr)
		}
		logrus.Info(stdout)

		// Sleeping for 1 second to allow the pod to receive the signal
		time.Sleep(1 * time.Second)
	}

	return nil
}

// execCommandInPod executes the provided command in the given pod and returns the stdout and stderr.
func execCommandInPod(clientset *kubernetes.Clientset, config *rest.Config, namespace, podName, containerName string, command []string) (string, string, error) {
	logrus.Infof("Running command %s in pod %s in container %s", command, podName, containerName)

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		Param("container", containerName).
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), stderr.String(), err
	}

	return stdout.String(), stderr.String(), nil
}

func WaitForPodReady(ctx context.Context, cl client.Client, name, namespace string, podUpdateTimeout,
	podMaxPollingInterval time.Duration) error {
	f := func() error {
		var pod corev1.Pod
		if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
			return err
		}

		if pod.Status.Phase == corev1.PodPending || !IsPodReady(&pod) {
			return fmt.Errorf("Pod %s not in ready state", name)
		}

		logrus.Infof("Pod %s in ready state now", name)
		return nil
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = podUpdateTimeout
	b.MaxInterval = podMaxPollingInterval
	return backoff.Retry(f, b)
}
