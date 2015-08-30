/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package e2e

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/resource"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var (
	buildBackOffDuration = time.Minute
	syncLoopFrequency    = time.Second
	maxContainerBackOff  = 300 * time.Second
	maxBackOffTolerance  = time.Duration(1.3 * float64(maxContainerBackOff))
)

func runPod(framework *Framework, pod *api.Pod) {
	By("submitting the pod to kubernetes")

	podClient := framework.Client.Pods(framework.Namespace.Name)
	pod, err := podClient.Create(pod)
	if err != nil {
		Failf("Failed to create pod: %v", err)
	}

	expectNoError(framework.WaitForPodRunning(pod.Name))

	By("verifying the pod is in kubernetes")
	pod, err = podClient.Get(pod.Name)
	if err != nil {
		Failf("failed to get pod: %v", err)
	}
}

func startPodAndGetBackOffs(framework *Framework, pod *api.Pod, podName string, containerName string, sleepAmount time.Duration) (time.Duration, time.Duration) {
	runPod(framework, pod)
	time.Sleep(sleepAmount)

	By("getting restart delay-1")
	delay1, err := getRestartDelay(framework.Client, pod, framework.Namespace.Name, podName, containerName)
	if err != nil {
		Failf("timed out waiting for container restart in pod=%s/%s", podName, containerName)
	}

	By("getting restart delay-2")
	delay2, err := getRestartDelay(framework.Client, pod, framework.Namespace.Name, podName, containerName)
	if err != nil {
		Failf("timed out waiting for container restart in pod=%s/%s", podName, containerName)
	}
	return delay1, delay2
}

func getRestartDelay(c *client.Client, pod *api.Pod, ns string, name string, containerName string) (time.Duration, error) {
	var startedAt time.Time
	beginTime := time.Now()
	for time.Since(beginTime) < (2 * maxBackOffTolerance) { // may just miss the 1st maxContainerBackOff delay
		time.Sleep(poll)
		pod, err := c.Pods(ns).Get(name)
		expectNoError(err, fmt.Sprintf("getting pod %s", name))
		status, ok := api.GetContainerStatus(pod.Status.ContainerStatuses, containerName)
		if !ok {
			continue
		}

		if startedAt.IsZero() && status.State.Running != nil && status.State.Running.StartedAt.Time.After(beginTime) {
			startedAt = status.State.Running.StartedAt.Time
		} else if !startedAt.IsZero() && status.LastTerminationState.Terminated != nil {
			Logf("finishedAt=%s restartedAt=%s (%s)", status.LastTerminationState.Terminated.FinishedAt.Time, startedAt, startedAt.Sub(status.LastTerminationState.Terminated.FinishedAt.Time))
			return startedAt.Sub(status.LastTerminationState.Terminated.FinishedAt.Time), nil
		}
	}
	return 0, fmt.Errorf("timeout getting pod restart delay")
}

func runLivenessTest(c *client.Client, ns string, podDescr *api.Pod, expectNumRestarts int) {
	By(fmt.Sprintf("Creating pod %s in namespace %s", podDescr.Name, ns))
	_, err := c.Pods(ns).Create(podDescr)
	expectNoError(err, fmt.Sprintf("creating pod %s", podDescr.Name))

	// At the end of the test, clean up by removing the pod.
	defer func() {
		By("deleting the pod")
		c.Pods(ns).Delete(podDescr.Name, api.NewDeleteOptions(0))
	}()

	// Wait until the pod is not pending. (Here we need to check for something other than
	// 'Pending' other than checking for 'Running', since when failures occur, we go to
	// 'Terminated' which can cause indefinite blocking.)
	expectNoError(waitForPodNotPending(c, ns, podDescr.Name),
		fmt.Sprintf("starting pod %s in namespace %s", podDescr.Name, ns))
	By(fmt.Sprintf("Started pod %s in namespace %s", podDescr.Name, ns))

	// Check the pod's current state and verify that restartCount is present.
	By("checking the pod's current state and verifying that restartCount is present")
	pod, err := c.Pods(ns).Get(podDescr.Name)
	expectNoError(err, fmt.Sprintf("getting pod %s in namespace %s", podDescr.Name, ns))
	initialRestartCount := api.GetExistingContainerStatus(pod.Status.ContainerStatuses, "liveness").RestartCount
	By(fmt.Sprintf("Initial restart count of pod %s is %d", podDescr.Name, initialRestartCount))

	// Wait for the restart state to be as desired.
	deadline := time.Now().Add(2 * time.Minute)
	lastRestartCount := initialRestartCount
	observedRestarts := 0
	for start := time.Now(); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		pod, err = c.Pods(ns).Get(podDescr.Name)
		expectNoError(err, fmt.Sprintf("getting pod %s", podDescr.Name))
		restartCount := api.GetExistingContainerStatus(pod.Status.ContainerStatuses, "liveness").RestartCount
		if restartCount != lastRestartCount {
			By(fmt.Sprintf("Restart count of pod %s/%s is now %d (%v elapsed)",
				ns, podDescr.Name, restartCount, time.Since(start)))
			if restartCount < lastRestartCount {
				Failf("Restart count should increment monotonically: restart cont of pod %s/%s changed from %d to %d",
					ns, podDescr.Name, lastRestartCount, restartCount)
			}
		}
		observedRestarts = restartCount - initialRestartCount
		if expectNumRestarts > 0 && observedRestarts >= expectNumRestarts {
			// Stop if we have observed more than expectNumRestarts restarts.
			break
		}
		lastRestartCount = restartCount
	}

	// If we expected 0 restarts, fail if observed any restart.
	// If we expected n restarts (n > 0), fail if we observed < n restarts.
	if (expectNumRestarts == 0 && observedRestarts > 0) || (expectNumRestarts > 0 &&
		observedRestarts < expectNumRestarts) {
		Failf("pod %s/%s - expected number of restarts: %t, found restarts: %t",
			ns, podDescr.Name, expectNumRestarts, observedRestarts)
	}
}

// testHostIP tests that a pod gets a host IP
func testHostIP(c *client.Client, ns string, pod *api.Pod) {
	podClient := c.Pods(ns)
	By("creating pod")
	defer podClient.Delete(pod.Name, api.NewDeleteOptions(0))
	if _, err := podClient.Create(pod); err != nil {
		Failf("Failed to create pod: %v", err)
	}
	By("ensuring that pod is running and has a hostIP")
	// Wait for the pods to enter the running state. Waiting loops until the pods
	// are running so non-running pods cause a timeout for this test.
	err := waitForPodRunningInNamespace(c, pod.Name, ns)
	Expect(err).NotTo(HaveOccurred())
	// Try to make sure we get a hostIP for each pod.
	hostIPTimeout := 2 * time.Minute
	t := time.Now()
	for {
		p, err := podClient.Get(pod.Name)
		Expect(err).NotTo(HaveOccurred())
		if p.Status.HostIP != "" {
			Logf("Pod %s has hostIP: %s", p.Name, p.Status.HostIP)
			break
		}
		if time.Since(t) >= hostIPTimeout {
			Failf("Gave up waiting for hostIP of pod %s after %v seconds",
				p.Name, time.Since(t).Seconds())
		}
		Logf("Retrying to get the hostIP of pod %s", p.Name)
		time.Sleep(5 * time.Second)
	}
}

var _ = Describe("Pods", func() {
	framework := NewFramework("pods")

	PIt("should get a host IP", func() {
		name := "pod-hostip-" + string(util.NewUUID())
		testHostIP(framework.Client, framework.Namespace.Name, &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name: name,
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "test",
						Image: "gcr.io/google_containers/pause",
					},
				},
			},
		})
	})

	It("should be schedule with cpu and memory limits", func() {
		podClient := framework.Client.Pods(framework.Namespace.Name)

		By("creating the pod")
		name := "pod-update-" + string(util.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "nginx",
						Image: "gcr.io/google_containers/pause",
						Resources: api.ResourceRequirements{
							Limits: api.ResourceList{
								api.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
								api.ResourceMemory: *resource.NewQuantity(10*1024*1024, resource.DecimalSI),
							},
						},
					},
				},
			},
		}
		defer podClient.Delete(pod.Name, nil)
		_, err := podClient.Create(pod)
		if err != nil {
			Failf("Error creating a pod: %v", err)
		}
		expectNoError(framework.WaitForPodRunning(pod.Name))
	})

	It("should be submitted and removed", func() {
		podClient := framework.Client.Pods(framework.Namespace.Name)

		By("creating the pod")
		name := "pod-update-" + string(util.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "nginx",
						Image: "gcr.io/google_containers/nginx:1.7.9",
						Ports: []api.ContainerPort{{ContainerPort: 80}},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								HTTPGet: &api.HTTPGetAction{
									Path: "/index.html",
									Port: util.NewIntOrStringFromInt(8080),
								},
							},
							InitialDelaySeconds: 30,
						},
					},
				},
			},
		}

		By("setting up watch")
		pods, err := podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})), fields.Everything())
		if err != nil {
			Failf("Failed to query for pods: %v", err)
		}
		Expect(len(pods.Items)).To(Equal(0))
		w, err := podClient.Watch(
			labels.SelectorFromSet(labels.Set(map[string]string{"time": value})), fields.Everything(), pods.ListMeta.ResourceVersion)
		if err != nil {
			Failf("Failed to set up watch: %v", err)
		}

		By("submitting the pod to kubernetes")
		// We call defer here in case there is a problem with
		// the test so we can ensure that we clean up after
		// ourselves
		defer podClient.Delete(pod.Name, api.NewDeleteOptions(0))
		_, err = podClient.Create(pod)
		if err != nil {
			Failf("Failed to create pod: %v", err)
		}

		By("verifying the pod is in kubernetes")
		pods, err = podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})), fields.Everything())
		if err != nil {
			Failf("Failed to query for pods: %v", err)
		}
		Expect(len(pods.Items)).To(Equal(1))

		By("verifying pod creation was observed")
		select {
		case event, _ := <-w.ResultChan():
			if event.Type != watch.Added {
				Failf("Failed to observe pod creation: %v", event)
			}
		case <-time.After(podStartTimeout):
			Fail("Timeout while waiting for pod creation")
		}

		By("deleting the pod")
		if err := podClient.Delete(pod.Name, nil); err != nil {
			Failf("Failed to delete pod: %v", err)
		}

		By("verifying pod deletion was observed")
		deleted := false
		timeout := false
		timer := time.After(podStartTimeout)
		for !deleted && !timeout {
			select {
			case event, _ := <-w.ResultChan():
				if event.Type == watch.Deleted {
					deleted = true
				}
			case <-timer:
				timeout = true
			}
		}
		if !deleted {
			Fail("Failed to observe pod deletion")
		}

		pods, err = podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})), fields.Everything())
		if err != nil {
			Fail(fmt.Sprintf("Failed to list pods to verify deletion: %v", err))
		}
		Expect(len(pods.Items)).To(Equal(0))
	})

	It("should be updated", func() {
		podClient := framework.Client.Pods(framework.Namespace.Name)

		By("creating the pod")
		name := "pod-update-" + string(util.NewUUID())
		value := strconv.Itoa(time.Now().Nanosecond())
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"name": "foo",
					"time": value,
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "nginx",
						Image: "gcr.io/google_containers/nginx:1.7.9",
						Ports: []api.ContainerPort{{ContainerPort: 80}},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								HTTPGet: &api.HTTPGetAction{
									Path: "/index.html",
									Port: util.NewIntOrStringFromInt(8080),
								},
							},
							InitialDelaySeconds: 30,
						},
					},
				},
			},
		}

		By("submitting the pod to kubernetes")
		defer func() {
			By("deleting the pod")
			podClient.Delete(pod.Name, api.NewDeleteOptions(0))
		}()
		pod, err := podClient.Create(pod)
		if err != nil {
			Failf("Failed to create pod: %v", err)
		}

		expectNoError(framework.WaitForPodRunning(pod.Name))

		By("verifying the pod is in kubernetes")
		pods, err := podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})), fields.Everything())
		Expect(len(pods.Items)).To(Equal(1))

		// Standard get, update retry loop
		expectNoError(wait.Poll(time.Millisecond*500, time.Second*30, func() (bool, error) {
			By("updating the pod")
			value = strconv.Itoa(time.Now().Nanosecond())
			if pod == nil { // on retries we need to re-get
				pod, err = podClient.Get(name)
				if err != nil {
					return false, fmt.Errorf("failed to get pod: %v", err)
				}
			}
			pod.Labels["time"] = value
			pod, err = podClient.Update(pod)
			if err == nil {
				Logf("Successfully updated pod")
				return true, nil
			}
			if errors.IsConflict(err) {
				Logf("Conflicting update to pod, re-get and re-update: %v", err)
				pod = nil // re-get it when we retry
				return false, nil
			}
			return false, fmt.Errorf("failed to update pod: %v", err)
		}))

		expectNoError(framework.WaitForPodRunning(pod.Name))

		By("verifying the updated pod is in kubernetes")
		pods, err = podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})), fields.Everything())
		Expect(len(pods.Items)).To(Equal(1))
		Logf("Pod update OK")
	})

	It("should contain environment variables for services", func() {
		// Make a pod that will be a service.
		// This pod serves its hostname via HTTP.
		serverName := "server-envvars-" + string(util.NewUUID())
		serverPod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   serverName,
				Labels: map[string]string{"name": serverName},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "srv",
						Image: "gcr.io/google_containers/serve_hostname:1.1",
						Ports: []api.ContainerPort{{ContainerPort: 9376}},
					},
				},
			},
		}
		defer framework.Client.Pods(framework.Namespace.Name).Delete(serverPod.Name, api.NewDeleteOptions(0))
		_, err := framework.Client.Pods(framework.Namespace.Name).Create(serverPod)
		if err != nil {
			Failf("Failed to create serverPod: %v", err)
		}
		expectNoError(framework.WaitForPodRunning(serverPod.Name))

		// This service exposes port 8080 of the test pod as a service on port 8765
		// TODO(filbranden): We would like to use a unique service name such as:
		//   svcName := "svc-envvars-" + randomSuffix()
		// However, that affects the name of the environment variables which are the capitalized
		// service name, so that breaks this test.  One possibility is to tweak the variable names
		// to match the service.  Another is to rethink environment variable names and possibly
		// allow overriding the prefix in the service manifest.
		svcName := "fooservice"
		svc := &api.Service{
			ObjectMeta: api.ObjectMeta{
				Name: svcName,
				Labels: map[string]string{
					"name": svcName,
				},
			},
			Spec: api.ServiceSpec{
				Ports: []api.ServicePort{{
					Port:       8765,
					TargetPort: util.NewIntOrStringFromInt(8080),
				}},
				Selector: map[string]string{
					"name": serverName,
				},
			},
		}
		defer framework.Client.Services(framework.Namespace.Name).Delete(svc.Name)
		_, err = framework.Client.Services(framework.Namespace.Name).Create(svc)
		if err != nil {
			Failf("Failed to create service: %v", err)
		}

		// Make a client pod that verifies that it has the service environment variables.
		podName := "client-envvars-" + string(util.NewUUID())
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"name": podName},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    "env3cont",
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"sh", "-c", "env"},
					},
				},
				RestartPolicy: api.RestartPolicyNever,
			},
		}

		framework.TestContainerOutput("service env", pod, 0, []string{
			"FOOSERVICE_SERVICE_HOST=",
			"FOOSERVICE_SERVICE_PORT=",
			"FOOSERVICE_PORT=",
			"FOOSERVICE_PORT_8765_TCP_PORT=",
			"FOOSERVICE_PORT_8765_TCP_PROTO=",
			"FOOSERVICE_PORT_8765_TCP=",
			"FOOSERVICE_PORT_8765_TCP_ADDR=",
		})
	})

	It("should be restarted with a docker exec \"cat /tmp/health\" liveness probe", func() {
		runLivenessTest(framework.Client, framework.Namespace.Name, &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   "liveness-exec",
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    "liveness",
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"/bin/sh", "-c", "echo ok >/tmp/health; sleep 10; rm -rf /tmp/health; sleep 600"},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								Exec: &api.ExecAction{
									Command: []string{"cat", "/tmp/health"},
								},
							},
							InitialDelaySeconds: 15,
						},
					},
				},
			},
		}, 1)
	})

	It("should *not* be restarted with a docker exec \"cat /tmp/health\" liveness probe", func() {
		runLivenessTest(framework.Client, framework.Namespace.Name, &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   "liveness-exec",
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    "liveness",
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"/bin/sh", "-c", "echo ok >/tmp/health; sleep 600"},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								Exec: &api.ExecAction{
									Command: []string{"cat", "/tmp/health"},
								},
							},
							InitialDelaySeconds: 15,
						},
					},
				},
			},
		}, 0)
	})

	It("should be restarted with a /healthz http liveness probe", func() {
		runLivenessTest(framework.Client, framework.Namespace.Name, &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   "liveness-http",
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    "liveness",
						Image:   "gcr.io/google_containers/liveness",
						Command: []string{"/server"},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								HTTPGet: &api.HTTPGetAction{
									Path: "/healthz",
									Port: util.NewIntOrStringFromInt(8080),
								},
							},
							InitialDelaySeconds: 15,
						},
					},
				},
			},
		}, 1)
	})

	PIt("should have monotonically increasing restart count", func() {
		runLivenessTest(framework.Client, framework.Namespace.Name, &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   "liveness-http",
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    "liveness",
						Image:   "gcr.io/google_containers/liveness",
						Command: []string{"/server"},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								HTTPGet: &api.HTTPGetAction{
									Path: "/healthz",
									Port: util.NewIntOrStringFromInt(8080),
								},
							},
							InitialDelaySeconds: 5,
						},
					},
				},
			},
		}, 8)
	})

	It("should *not* be restarted with a /healthz http liveness probe", func() {
		runLivenessTest(framework.Client, framework.Namespace.Name, &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   "liveness-http",
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:  "liveness",
						Image: "gcr.io/google_containers/nettest:1.6",
						// These args are garbage but the image will exit if they're not there
						// we just care about /read serving a 200, which it always does.
						Args: []string{
							"-service=liveness-http",
							"-peers=1",
							"-namespace=" + framework.Namespace.Name},
						Ports: []api.ContainerPort{{ContainerPort: 8080}},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								HTTPGet: &api.HTTPGetAction{
									Path: "/read",
									Port: util.NewIntOrStringFromInt(8080),
								},
							},
							InitialDelaySeconds: 15,
						},
					},
				},
			},
		}, 0)
	})

	It("should have their container restart back-off timer increase exponentially", func() {
		podName := "pod-back-off-exponentially"
		containerName := "back-off"
		podClient := framework.Client.Pods(framework.Namespace.Name)
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"test": "back-off-image"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    containerName,
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"/bin/sh", "-c", "sleep 1", "/crash/missing"},
					},
				},
			},
		}

		defer func() {
			By("deleting the pod")
			podClient.Delete(pod.Name, api.NewDeleteOptions(0))
		}()

		delay1, delay2 := startPodAndGetBackOffs(framework, pod, podName, containerName, buildBackOffDuration)
		ratio := float64(delay2) / float64(delay1)
		if math.Floor(ratio) != 2 && math.Ceil(ratio) != 2 {
			Failf("back-off gap is not increasing exponentially pod=%s/%s delay1=%s delay2=%s", podName, containerName, delay1, delay2)
		}
	})

	It("should have their auto-restart back-off timer reset on image update", func() {
		podName := "pod-back-off-image"
		containerName := "back-off"
		podClient := framework.Client.Pods(framework.Namespace.Name)
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"test": "back-off-image"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    containerName,
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"/bin/sh", "-c", "sleep 1", "/crash/missing"},
					},
				},
			},
		}

		defer func() {
			By("deleting the pod")
			podClient.Delete(pod.Name, api.NewDeleteOptions(0))
		}()

		delay1, delay2 := startPodAndGetBackOffs(framework, pod, podName, containerName, buildBackOffDuration)

		By("updating the image")
		pod, err := podClient.Get(pod.Name)
		if err != nil {
			Failf("failed to get pod: %v", err)
		}
		pod.Spec.Containers[0].Image = "nginx"
		pod, err = podClient.Update(pod)
		if err != nil {
			Failf("error updating pod=%s/%s %v", podName, containerName, err)
		}
		time.Sleep(syncLoopFrequency)
		expectNoError(framework.WaitForPodRunning(pod.Name))

		By("get restart delay after image update")
		delayAfterUpdate, err := getRestartDelay(framework.Client, pod, framework.Namespace.Name, podName, containerName)
		if err != nil {
			Failf("timed out waiting for container restart in pod=%s/%s", podName, containerName)
		}

		if delayAfterUpdate > delay2 || delayAfterUpdate > delay1 {
			Failf("updating image did not reset the back-off value in pod=%s/%s d3=%s d2=%s d1=%s", podName, containerName, delayAfterUpdate, delay1, delay2)
		}
	})

	It("should not back-off restarting a container on LivenessProbe failure", func() {
		podClient := framework.Client.Pods(framework.Namespace.Name)
		podName := "pod-back-off-liveness"
		containerName := "back-off-liveness"
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    containerName,
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"/bin/sh", "-c", "echo ok >/tmp/health; sleep 5; rm -rf /tmp/health; sleep 600"},
						LivenessProbe: &api.Probe{
							Handler: api.Handler{
								Exec: &api.ExecAction{
									Command: []string{"cat", "/tmp/health"},
								},
							},
							InitialDelaySeconds: 2,
						},
					},
				},
			},
		}

		defer func() {
			By("deleting the pod")
			podClient.Delete(pod.Name, api.NewDeleteOptions(0))
		}()

		delay1, delay2 := startPodAndGetBackOffs(framework, pod, podName, containerName, buildBackOffDuration)

		ratio := float64(delay2) / float64(delay1)
		if math.Floor(ratio) != 1 && math.Ceil(ratio) != 1 {
			Failf("back-off increasing on LivenessProbe failure delay1=%s delay2=%s", delay1, delay2)
		}
	})

	It("should cap back-off at maxContainerBackOff", func() {
		podClient := framework.Client.Pods(framework.Namespace.Name)
		podName := "back-off-cap"
		containerName := "back-off-cap"
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"test": "liveness"},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{
					{
						Name:    containerName,
						Image:   "gcr.io/google_containers/busybox",
						Command: []string{"/bin/sh", "-c", "sleep 1", "/crash/missing"},
					},
				},
			},
		}

		defer func() {
			By("deleting the pod")
			podClient.Delete(pod.Name, api.NewDeleteOptions(0))
		}()

		runPod(framework, pod)
		time.Sleep(2 * maxContainerBackOff) // it takes slightly more than 2*x to get to a back-off of x

		// wait for a delay == capped delay of maxContainerBackOff
		By("geting restart delay when capped")
		var (
			delay1 time.Duration
			err    error
		)
		for i := 0; i < 3; i++ {
			delay1, err = getRestartDelay(framework.Client, pod, framework.Namespace.Name, podName, containerName)
			if err != nil {
				Failf("timed out waiting for container restart in pod=%s/%s", podName, containerName)
			}

			if delay1 < maxContainerBackOff {
				continue
			}
		}

		if (delay1 < maxContainerBackOff) || (delay1 > maxBackOffTolerance) {
			Failf("expected %s back-off got=%s in delay1", maxContainerBackOff, delay1)
		}

		By("getting restart delay after a capped delay")
		delay2, err := getRestartDelay(framework.Client, pod, framework.Namespace.Name, podName, containerName)
		if err != nil {
			Failf("timed out waiting for container restart in pod=%s/%s", podName, containerName)
		}

		if delay2 < maxContainerBackOff || delay2 > maxBackOffTolerance { // syncloop cumulative drift
			Failf("expected %s back-off got=%s on delay2", maxContainerBackOff, delay2)
		}
	})

	// The following tests for remote command execution and port forwarding are
	// commented out because the GCE environment does not currently have nsenter
	// in the kubelet's PATH, nor does it have socat installed. Once we figure
	// out the best way to have nsenter and socat available in GCE (and hopefully
	// all providers), we can enable these tests.
	/*
		It("should support remote command execution", func() {
			clientConfig, err := loadConfig()
			if err != nil {
				Failf("Failed to create client config: %v", err)
			}

			podClient := framework.Client.Pods(framework.Namespace.Name)

			By("creating the pod")
			name := "pod-exec-" + string(util.NewUUID())
			value := strconv.Itoa(time.Now().Nanosecond())
			pod := &api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: name,
					Labels: map[string]string{
						"name": "foo",
						"time": value,
					},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "nginx",
							Image: "gcr.io/google_containers/nginx:1.7.9",
						},
					},
				},
			}

			By("submitting the pod to kubernetes")
			_, err = podClient.Create(pod)
			if err != nil {
				Failf("Failed to create pod: %v", err)
			}
			defer func() {
				// We call defer here in case there is a problem with
				// the test so we can ensure that we clean up after
				// ourselves
				podClient.Delete(pod.Name, api.NewDeleteOptions(0))
			}()

			By("waiting for the pod to start running")
			expectNoError(framework.WaitForPodRunning(pod.Name))

			By("verifying the pod is in kubernetes")
			pods, err := podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})))
			if err != nil {
				Failf("Failed to query for pods: %v", err)
			}
			Expect(len(pods.Items)).To(Equal(1))

			pod = &pods.Items[0]
			By(fmt.Sprintf("executing command on host %s pod %s in container %s",
				pod.Status.Host, pod.Name, pod.Spec.Containers[0].Name))
			req := framework.Client.Get().
				Prefix("proxy").
				Resource("minions").
				Name(pod.Status.Host).
				Suffix("exec", framework.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)

			out := &bytes.Buffer{}
			e := remotecommand.New(req, clientConfig, []string{"whoami"}, nil, out, nil, false)
			err = e.Execute()
			if err != nil {
				Failf("Failed to execute command on host %s pod %s in container %s: %v",
					pod.Status.Host, pod.Name, pod.Spec.Containers[0].Name, err)
			}
			if e, a := "root\n", out.String(); e != a {
				Failf("exec: whoami: expected '%s', got '%s'", e, a)
			}
		})

		It("should support port forwarding", func() {
			clientConfig, err := loadConfig()
			if err != nil {
				Failf("Failed to create client config: %v", err)
			}

			podClient := framework.Client.Pods(framework.Namespace.Name)

			By("creating the pod")
			name := "pod-portforward-" + string(util.NewUUID())
			value := strconv.Itoa(time.Now().Nanosecond())
			pod := &api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: name,
					Labels: map[string]string{
						"name": "foo",
						"time": value,
					},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "nginx",
							Image: "gcr.io/google_containers/nginx:1.7.9",
							Ports: []api.Port{{ContainerPort: 80}},
						},
					},
				},
			}

			By("submitting the pod to kubernetes")
			_, err = podClient.Create(pod)
			if err != nil {
				Failf("Failed to create pod: %v", err)
			}
			defer func() {
				// We call defer here in case there is a problem with
				// the test so we can ensure that we clean up after
				// ourselves
				podClient.Delete(pod.Name, api.NewDeleteOptions(0))
			}()

			By("waiting for the pod to start running")
			expectNoError(framework.WaitForPodRunning(pod.Name))

			By("verifying the pod is in kubernetes")
			pods, err := podClient.List(labels.SelectorFromSet(labels.Set(map[string]string{"time": value})))
			if err != nil {
				Failf("Failed to query for pods: %v", err)
			}
			Expect(len(pods.Items)).To(Equal(1))

			pod = &pods.Items[0]
			By(fmt.Sprintf("initiating port forwarding to host %s pod %s in container %s",
				pod.Status.Host, pod.Name, pod.Spec.Containers[0].Name))

			req := framework.Client.Get().
				Prefix("proxy").
				Resource("minions").
				Name(pod.Status.Host).
				Suffix("portForward", framework.Namespace.Name, pod.Name)

			stopChan := make(chan struct{})
			pf, err := portforward.New(req, clientConfig, []string{"5678:80"}, stopChan)
			if err != nil {
				Failf("Error creating port forwarder: %s", err)
			}

			errorChan := make(chan error)
			go func() {
				errorChan <- pf.ForwardPorts()
			}()

			// wait for listeners to start
			<-pf.Ready

			resp, err := http.Get("http://localhost:5678/")
			if err != nil {
				Failf("Error with http get to localhost:5678: %s", err)
			}
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				Failf("Error reading response body: %s", err)
			}

			titleRegex := regexp.MustCompile("<title>(.+)</title>")
			matches := titleRegex.FindStringSubmatch(string(body))
			if len(matches) != 2 {
				Fail("Unable to locate page title in response HTML")
			}
			if e, a := "Welcome to nginx on Debian!", matches[1]; e != a {
				Failf("<title>: expected '%s', got '%s'", e, a)
			}
		})
	*/
})
