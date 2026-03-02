// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eiffel-community/etos-api/pkg/executionspace/executionspace"
	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/net"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	watch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	BACKOFFLIMIT int32 = 0
	PARALLEL     int32 = 1
	COMPLETIONS  int32 = 1
	SECRETMODE   int32 = 0600
)

type KubernetesExecutor struct {
	client    *kubernetes.Clientset
	namespace string
}

// Kubernetes returns a new Kubernetes executor
func Kubernetes(namespace string) Executor {
	config, err := inCluster()
	if err != nil {
		config, err = outOfCluster()
	}
	if err != nil {
		panic(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	return &KubernetesExecutor{
		client:    client,
		namespace: namespace,
	}
}

// outOfCluster returns a configuration from $HOME/.kube/config
func outOfCluster() (*rest.Config, error) {
	if homedir.HomeDir() == "" {
		return nil, errors.New("no home directory for user")
	}
	kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// inCluster returns a configuration from within a kubernetes cluster.
func inCluster() (*rest.Config, error) {
	return rest.InClusterConfig()
}

// Name returns the name of this executor
func (k KubernetesExecutor) Name() string {
	return "kubernetes"
}

// Start starts a test runner Kubernetes pod.
func (k KubernetesExecutor) Start(ctx context.Context, logger *logrus.Entry, executorSpec *executionspace.ExecutorSpec) (string, error) {
	jobName := fmt.Sprintf("etr-%s", executorSpec.ID)
	logger.WithField("user_log", true).Infof("Starting up a test runner with id %s on Kubernetes", jobName)
	var envs []corev1.EnvVar
	for key, value := range executorSpec.Instructions.Environment {
		envs = append(envs, corev1.EnvVar{Name: key, Value: value})
	}
	var args []string
	for key, value := range executorSpec.Instructions.Parameters {
		args = append(args, fmt.Sprintf("%s=%s", key, value))
	}

	jobs := k.client.BatchV1().Jobs(k.namespace)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &BACKOFFLIMIT,
			Completions:  &COMPLETIONS,
			Parallelism:  &PARALLEL,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "etos-test-runner",
							Image: executorSpec.Instructions.Image,
							Args:  args,
							Env:   envs,
							EnvFrom: []corev1.EnvFromSource{
								{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "etos-encryption-key",
										},
									},
								},
							}},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	job, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		logger.WithField("user_log", true).Errorf("Create job error: %s", err)
		return "", err
	}
	return job.ObjectMeta.Name, nil
}

// isReady returns true if a pod is in the PodReady condition.
func isReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// podFromJob gets a pod connected to a job.
func (k KubernetesExecutor) podFromJob(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	pods := k.client.CoreV1().Pods(k.namespace)
	var pod corev1.Pod
	podlist, err := pods.List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("job-name=%s", job.ObjectMeta.Name)})
	if err != nil {
		return &pod, err
	}
	if len(podlist.Items) != 1 {
		return &pod, errors.New("no pod yet")
	}
	pod = podlist.Items[0]
	return &pod, nil
}

// Wait waits for a Kubernetes pod to start
func (k KubernetesExecutor) Wait(ctx context.Context, logger *logrus.Entry, name string, executorSpec *executionspace.ExecutorSpec) (string, string, error) {
	logger.WithField("user_log", true).Info("Waiting for a test runner Kubernetes pod to start")
	watcher, err := k.client.CoreV1().Pods(k.namespace).Watch(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("job-name=%s", name)})
	if err != nil {
		return "", "", err
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("timed out waiting for Kubernetes job %s to start", name)
		case event := <-watcher.ResultChan():
			pod := event.Object.(*corev1.Pod)
			if isReady(pod) {
				return name, "", nil
			}
		}
	}
}

// Stop stops a test runner Kubernetes pod
func (k KubernetesExecutor) Stop(ctx context.Context, logger *logrus.Entry, name string) error {
	logger.WithField("user_log", true).Info("Stopping test runner Kubernetes pod")
	jobs := k.client.BatchV1().Jobs(k.namespace)
	propagation := metav1.DeletePropagationForeground
	err := jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Warningf("Job %s not found, assuming already deleted", name)
			return nil
		} else {
			logger.Error(err.Error())
			return err
		}
	}
	watcher, err := k.client.CoreV1().Pods(k.namespace).Watch(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("job-name=%s", name)})
	if err != nil {
		if net.IsProbableEOF(err) {
			// Assume that there are no more active jobs.
			logger.Warningf("Did not find any pods for 'job-name=%s', reason=EOF. Assuming that there are no more active jobs", name)
			return nil
		}
		return err
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Kubernetes job %s to stop", name)
		case event := <-watcher.ResultChan():
			if event.Type == watch.Deleted {
				return nil
			}
		}
	}
}

// Cancel stops a Kubernetes job. Since Kubernetes has no queue concept, the cancel function does nothing else.
func (k KubernetesExecutor) Cancel(ctx context.Context, logger *logrus.Entry, id string) error {
	return k.Stop(ctx, logger, id)
}

// Alive checks that a Kubernetes pod running a test runner is still alive
func (k KubernetesExecutor) Alive(ctx context.Context, logger *logrus.Entry, id string) (bool, error) {
	jobs := k.client.BatchV1().Jobs(k.namespace)
	job, err := jobs.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		logger.Errorf("Get job error: %s", err)
		return false, err
	}
	pod, err := k.podFromJob(ctx, job)
	if err != nil {
		logger.Errorf("Get pod error: %s", err)
		return false, err
	}
	ready := isReady(pod)
	if !ready {
		pods := k.client.CoreV1().Pods(k.namespace)
		result := pods.GetLogs(pod.ObjectMeta.Name, &corev1.PodLogOptions{}).Do(ctx)
		if result.Error() != nil {
			logger.Errorf("Error getting logs from pod: %s", result.Error())
			return ready, nil
		}
		logs, err := result.Raw()
		if err != nil {
			logger.Errorf("Error reading logs from pod: %s", err)
		} else {
			splitLogs := strings.Split(string(logs), "\n")
			for _, line := range splitLogs {
				logger.WithField("user_log", true).Info(line)
			}
		}
	}
	return ready, nil
}
