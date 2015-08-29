/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package container

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/unversioned/record"
	"k8s.io/kubernetes/pkg/util"
)

// imagePuller pulls the image using Runtime.PullImage().
// It will check the presence of the image, and report the 'image pulling',
// 'image pulled' events correspondingly.
type imagePuller struct {
	recorder record.EventRecorder
	runtime  Runtime
	backOff  *util.Backoff
}

// NewImagePuller takes an event recorder and container runtime to create a
// image puller that wraps the container runtime's PullImage interface.
func NewImagePuller(recorder record.EventRecorder, runtime Runtime) ImagePuller {
	return &imagePuller{
		recorder: recorder,
		runtime:  runtime,
		backOff:  util.NewBackOff(BackOffInterval, BackOffMax),
	}
}

// shouldPullImage returns whether we should pull an image according to
// the presence and pull policy of the image.
func shouldPullImage(container *api.Container, imagePresent bool) bool {
	if container.ImagePullPolicy == api.PullNever {
		return false
	}

	if container.ImagePullPolicy == api.PullAlways ||
		(container.ImagePullPolicy == api.PullIfNotPresent && (!imagePresent)) {
		return true
	}

	return false
}

// reportImagePull reports 'image pulling', 'image pulled' or 'image pulling failed' events.
func (puller *imagePuller) reportImagePull(ref *api.ObjectReference, event string, image string, pullError error) {
	if ref == nil {
		return
	}

	switch event {
	case "pulling":
		puller.recorder.Eventf(ref, "pulling", "Pulling image %q", image)
	case "pulled":
		puller.recorder.Eventf(ref, "pulled", "Successfully pulled image %q", image)
	case "failed":
		puller.recorder.Eventf(ref, "failed", "Failed to pull image %q: %v", image, pullError)
	}
}

// PullImage pulls the image for the specified pod and container.
func (puller *imagePuller) PullImage(pod *api.Pod, container *api.Container, pullSecrets []api.Secret) error {
	ref, err := GenerateContainerRef(pod, container)
	if err != nil {
		glog.Errorf("Couldn't make a ref to pod %v, container %v: '%v'", pod.Name, container.Name, err)
	}
	spec := ImageSpec{container.Image}
	present, err := puller.runtime.IsImagePresent(spec)
	if err != nil {
		if ref != nil {
			puller.recorder.Eventf(ref, "failed", "Failed to inspect image %q: %v", container.Image, err)
		}
		return fmt.Errorf("failed to inspect image %q: %v", container.Image, err)
	}

	if !shouldPullImage(container, present) {
		var reason, msg string
		var r error
		if present {
			reason = "pulled"
			msg = fmt.Sprintf("Container image %q already present on machine", container.Image)
			r = nil
		} else {
			reason = "NeverPull"
			msg = fmt.Sprintf("Container image %q is not present on machine with pull policy set to NeverPull", container.Image)
			r = fmt.Errorf(msg)
		}

		if ref != nil {
			puller.recorder.Eventf(ref, reason, msg)
		}
		return r
	}

	if puller.backOff.IsInBackOffSinceUpdate(container.Image, time.Now()) {
		puller.reportImagePull(ref, "backoff", container.Image, nil)
		msg := fmt.Sprintf("back-off pulling image %q", container.Image)
		puller.recorder.Eventf(ref, "back-off", msg)
		return fmt.Errorf(msg)
	}
	puller.reportImagePull(ref, "pulling", container.Image, nil)
	if err = puller.runtime.PullImage(spec, pullSecrets); err != nil {
		puller.reportImagePull(ref, "failed", container.Image, err)
		return err
	}
	puller.reportImagePull(ref, "pulled", container.Image, nil)
	return nil
}
