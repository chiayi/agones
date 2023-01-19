// Copyright 2023 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package extensions

import (
	"context"
	"testing"
	"time"

	"agones.dev/agones/pkg/util/runtime"
	e2eframework "agones.dev/agones/test/e2e/framework"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
)

/*
Test will show that even after deleting one of the extensions pod, the extensions service will still work.
Plan is after set up, delete one of the extensions pod
Make sure that one of the extensions pod is still running
Create a game server

	Ensure that game server is created.
*/
func TestGameServerHealthyAfterDeletingPodWhileOneExtensionsDown(t *testing.T) {
	logger := e2eframework.TestLogger(t)
	ctx := context.Background()

	if !runtime.FeatureEnabled(runtime.FeatureSplitControllerAndExtensions) {
		logger.Infof("Skip test. SplitControllerAndExtensions feature is not enabled: flag is %v", runtime.FeatureEnabled(runtime.FeatureSplitControllerAndExtensions))
		return
	}

	list, err := getAgoneseExtensionsPods(ctx)
	logger.Infof("Length of pod list is %v", len(list.Items))
	if err != nil {
		t.Fatalf("Could not get list of Extension pods: %v", err)
	}
	if len(list.Items) <= 1 {
		t.Fatal("Cluster has no Extensions pod or has only 1 extensions pod")
	}

	logger.Info("Removing one of the Extensions Pods")
	err = deleteAgonesExtensionsPods(ctx)
	assert.NoError(t, err)

	logger.Info("Creating default game server")
	gs := framework.DefaultGameServer(defaultNs)
	readyGs, err := framework.CreateGameServerAndWaitUntilReady(t, defaultNs, gs)
	if err != nil {
		t.Fatalf("Could not get a GameServer ready: %v", err)
	}
	logger.WithField("gsKey", readyGs.ObjectMeta.Name).Info("GameServer Ready")

	gsClient := framework.AgonesClient.AgonesV1().GameServers(defaultNs)
	podClient := framework.KubeClient.CoreV1().Pods(defaultNs)
	defer gsClient.Delete(ctx, readyGs.ObjectMeta.Name, metav1.DeleteOptions{}) // nolint: errcheck

	pod, err := podClient.Get(ctx, readyGs.ObjectMeta.Name, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, metav1.IsControlledBy(pod, readyGs))
}

// deleteAgonesExtensionsPod deletes one of the extensions pod for the Agones extensions,
// faking a extensions pod crash.
func deleteAgonesExtensionsPods(ctx context.Context) error {
	list, err := getAgoneseExtensionsPods(ctx)
	if err != nil {
		return err
	}

	policy := metav1.DeletePropagationBackground
	err = framework.KubeClient.CoreV1().Pods("agones-system").Delete(ctx, list.Items[1].ObjectMeta.Name,
		metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil {
		return err
	}
	return nil
}

func waitForAgonesExtensionsRunning(ctx context.Context) error {
	return wait.PollImmediate(time.Second, 5*time.Minute, func() (bool, error) {
		list, err := getAgoneseExtensionsPods(ctx)
		if err != nil {
			return true, err
		}

		for i := range list.Items {
			for _, c := range list.Items[i].Status.ContainerStatuses {
				if c.State.Running == nil {
					return false, nil
				}
			}
		}

		return true, nil
	})
}

// getAgonesExtensionsPods returns all the Agones extensions pods
func getAgoneseExtensionsPods(ctx context.Context) (*corev1.PodList, error) {
	opts := metav1.ListOptions{LabelSelector: labels.Set{"agones.dev/role": "extensions"}.String()}
	return framework.KubeClient.CoreV1().Pods("agones-system").List(ctx, opts)
}
