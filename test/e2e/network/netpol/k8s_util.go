/*
Copyright 2020 The Kubernetes Authors.

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

package netpol

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

// Scenario provides a convenience interface to kube functionality that we leverage for polling NetworkPolicy connections.
type Scenario struct {
	mutex     sync.Mutex
	podCache  map[string][]v1.Pod
	framework *framework.Framework
	ClientSet clientset.Interface
}

// NewScenario is a utility function that wraps creation of the stuff we're using for creating a cluster that expresses the global policy scenario we're testing.
func NewScenario(framework *framework.Framework) *Scenario {
	return &Scenario{
		podCache:  map[string][]v1.Pod{},
		framework: framework,
		ClientSet: framework.ClientSet,
	}
}

// InitializeCluster checks the state of the cluster, creating or updating namespaces and deployments as needed
func (k *Scenario) InitializeCluster(model *Model) error {
	var createdPods []*v1.Pod
	for _, ns := range model.Namespaces {
		_, err := k.CreateNamespace(ns.Spec())
		if err != nil {
			return err
		}

		for _, pod := range ns.Pods {
			framework.Logf("creating/updating pod %s/%s", ns.Name, pod.Name)

			kubePod, err := k.CreatePod(pod.KubePod())
			if err != nil {
				return err
			}
			createdPods = append(createdPods, kubePod)

			_, err = k.CreateService(pod.Service())
			if err != nil {
				return err
			}
		}
	}

	for _, podString := range model.AllPodStrings() {
		k8sPod, err := k.GetPodFromCache(podString.Namespace(), podString.PodName())
		if err != nil {
			return err
		}
		if k8sPod == nil {
			return errors.Errorf("unable to find pod in ns %s with key/val pod=%s", podString.Namespace(), podString.PodName())
		}
		err = e2epod.WaitForPodNameRunningInNamespace(k.ClientSet, k8sPod.Name, k8sPod.Namespace)
		if err != nil {
			return errors.Wrapf(err, "unable to wait for pod %s/%s", podString.Namespace(), podString.PodName())
		}
	}

	for _, createdPod := range createdPods {
		err := e2epod.WaitForPodRunningInNamespace(k.ClientSet, createdPod)
		if err != nil {
			return errors.Wrapf(err, "unable to wait for pod %s/%s", createdPod.Namespace, createdPod.Name)
		}
	}

	return nil
}

// GetPodFromCache returns a pod with the matching namespace and name
func (k *Scenario) GetPodFromCache(ns string, name string) (*v1.Pod, error) {
	pods, err := k.getPodsUncached(ns, "pod", name)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, nil
	}
	return &pods[0], nil
}

func (k *Scenario) getPodsUncached(ns string, key string, val string) ([]v1.Pod, error) {
	v1PodList, err := k.ClientSet.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%v=%v", key, val),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to list Pods in ns %s with key/val %s=%s", ns, key, val)
	}
	return v1PodList.Items, nil
}

// GetPodsFromCacheByKeyVal returns an array of all Pods in the given namespace having a k/v label pair.
func (k *Scenario) GetPodsFromCacheByKeyVal(ns string, key string, val string) ([]v1.Pod, error) {
	k.mutex.Lock()
	p, ok := k.podCache[fmt.Sprintf("%v_%v_%v", ns, key, val)]
	k.mutex.Unlock()
	if ok {
		return p, nil
	}

	v1PodList, err := k.getPodsUncached(ns, key, val)
	if err != nil {
		return nil, err
	}

	k.mutex.Lock()
	k.podCache[fmt.Sprintf("%v_%v_%v", ns, key, val)] = v1PodList
	k.mutex.Unlock()

	return v1PodList, nil
}

// GetPod gets a pod by namespace and name
func (k *Scenario) GetPod(ns string, name string) (*v1.Pod, error) {
	kubePod, err := k.ClientSet.CoreV1().Pods(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get pod %s/%s", ns, name)
	}
	return kubePod, nil
}

// Probe execs into a pod and checks its connectivity to another pod.
func (k *Scenario) Probe(nsFrom string, podFrom string, containerFrom string, addrTo string, protocol v1.Protocol, toPort int) (bool, string, error) {
	fromPods, err := k.GetPodsFromCacheByKeyVal(nsFrom, "pod", podFrom)
	if err != nil {
		return false, "", err
	}
	if len(fromPods) == 0 {
		return false, "", errors.Errorf("pod %s/%s not found", nsFrom, podFrom)
	}
	fromPod := fromPods[0]

	var cmd []string
	switch protocol {
	case v1.ProtocolSCTP:
		cmd = []string{"/agnhost", "connect", fmt.Sprintf("%s:%d", addrTo, toPort), "--timeout=1s", "--protocol=sctp"}
	case v1.ProtocolTCP:
		cmd = []string{"/agnhost", "connect", fmt.Sprintf("%s:%d", addrTo, toPort), "--timeout=1s", "--protocol=tcp"}
	case v1.ProtocolUDP:
		cmd = []string{"nc", "-v", "-z", "-w", "1", "-u", addrTo, fmt.Sprintf("%d", toPort)}
	default:
		framework.Failf("protocol %s not supported", protocol)
	}

	commandDebugString := fmt.Sprintf("kubectl exec %s -c %s -n %s -- %s", fromPod.Name, containerFrom, fromPod.Namespace, strings.Join(cmd, " "))
	stdout, stderr, err := k.ExecuteRemoteCommand(fromPod, containerFrom, cmd)
	if err != nil {
		framework.Logf("%s/%s -> %s: error when running command: err - %v /// stdout - %s /// stderr - %s", nsFrom, podFrom, addrTo, err, stdout, stderr)
		return false, commandDebugString, nil
	}
	return true, commandDebugString, nil
}

// ExecuteRemoteCommand executes a remote shell command on the given pod
func (k *Scenario) ExecuteRemoteCommand(pod v1.Pod, containerName string, command []string) (string, string, error) {
	return k.framework.ExecWithOptions(framework.ExecOptions{
		Command:            command,
		Namespace:          pod.Namespace,
		PodName:            pod.Name,
		ContainerName:      containerName,
		Stdin:              nil,
		CaptureStdout:      true,
		CaptureStderr:      true,
		PreserveWhitespace: false,
	})

}

// CreateNamespace is a convenience function for namespace setup
func (k *Scenario) CreateNamespace(ns *v1.Namespace) (*v1.Namespace, error) {
	createdNamespace, err := k.ClientSet.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to update namespace %s", ns.Name)
	}
	return createdNamespace, nil
}

// CreateService is a convenience function for service setup
func (k *Scenario) CreateService(service *v1.Service) (*v1.Service, error) {
	ns := service.Namespace
	name := service.Name

	createdService, err := k.ClientSet.CoreV1().Services(ns).Create(context.TODO(), service, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create service %s/%s", ns, name)
	}
	return createdService, nil
}

// CreatePod is a convenience function for pod setup
func (k *Scenario) CreatePod(pod *v1.Pod) (*v1.Pod, error) {
	ns := pod.Namespace
	framework.Logf("creating pod %s/%s", ns, pod.Name)

	createdPod, err := k.ClientSet.CoreV1().Pods(ns).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to update pod %s/%s", ns, pod.Name)
	}
	return createdPod, nil
}

// CleanNetworkPolicies is a convenience function for deleting network policies before startup of any new test.
func (k *Scenario) CleanNetworkPolicies(namespaces []string) error {
	for _, ns := range namespaces {
		framework.Logf("deleting policies in %s ..........", ns)
		l, err := k.ClientSet.NetworkingV1().NetworkPolicies(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return errors.Wrapf(err, "unable to list network policies in ns %s", ns)
		}
		for _, np := range l.Items {
			framework.Logf("deleting network policy %s/%s", ns, np.Name)
			err = k.ClientSet.NetworkingV1().NetworkPolicies(ns).Delete(context.TODO(), np.Name, metav1.DeleteOptions{})
			if err != nil {
				return errors.Wrapf(err, "unable to delete network policy %s/%s", ns, np.Name)
			}
		}
	}
	return nil
}

// ClearCache clears the kube pod cache
func (k *Scenario) ClearCache() {
	framework.Logf("Clearing pod cache")
	k.mutex.Lock()
	k.podCache = map[string][]v1.Pod{}
	k.mutex.Unlock()
	framework.Logf("Pod cache successfully cleared")
}

// CreateNetworkPolicy is a convenience function for creating netpols
func (k *Scenario) CreateNetworkPolicy(ns string, netpol *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	framework.Logf("creating network policy %s/%s", ns, netpol.Name)
	netpol.ObjectMeta.Namespace = ns
	np, err := k.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), netpol, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create network policy %s/%s", ns, netpol.Name)
	}
	return np, nil
}

// UpdateNetworkPolicy is a convenience function for updating netpols
func (k *Scenario) UpdateNetworkPolicy(ns string, netpol *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	framework.Logf("updating network policy %s/%s", ns, netpol.Name)
	netpol.ObjectMeta.Namespace = ns
	np, err := k.ClientSet.NetworkingV1().NetworkPolicies(ns).Update(context.TODO(), netpol, metav1.UpdateOptions{})
	if err != nil {
		return np, errors.Wrapf(err, "unable to update network policy %s/%s", ns, netpol.Name)
	}
	return np, nil
}

func (k *Scenario) getNamespace(ns string) (*v1.Namespace, error) {
	selectedNameSpace, err := k.ClientSet.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get namespace %s", ns)
	}
	return selectedNameSpace, nil
}

func (k *Scenario) setNamespaceLabels(ns string, labels map[string]string) error {
	selectedNameSpace, err := k.getNamespace(ns)
	if err != nil {
		return err
	}
	selectedNameSpace.ObjectMeta.Labels = labels
	_, err = k.ClientSet.CoreV1().Namespaces().Update(context.TODO(), selectedNameSpace, metav1.UpdateOptions{})
	return errors.Wrapf(err, "unable to update namespace %s", ns)
}

func (k *Scenario) deleteNamespaces(namespaces []string) error {
	for _, ns := range namespaces {
		err := k.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), ns, metav1.DeleteOptions{})
		if err != nil {
			return errors.Wrapf(err, "unable to delete namespace %s", ns)
		}
	}
	return nil
}

// waitForHTTPServers waits for all webservers to be up, on all protocols, and then validates them using the same probe logic as the rest of the suite.
func (k *Scenario) waitForHTTPServers(model *Model) error {
	const maxTries = 10
	framework.Logf("waiting for HTTP servers (ports 80 and 81) to become ready")

	testCases := map[string]*TestCase{}
	for _, port := range model.Ports {
		for _, protocol := range model.Protocols {
			fromPort := 81
			desc := fmt.Sprintf("%d->%d,%s", fromPort, port, protocol)
			testCases[desc] = &TestCase{FromPort: fromPort, ToPort: int(port), Protocol: protocol}
		}
	}
	notReady := map[string]bool{}
	for caseName := range testCases {
		notReady[caseName] = true
	}

	for i := 0; i < maxTries; i++ {
		for caseName, testCase := range testCases {
			if notReady[caseName] {
				reachability := NewReachability(model.AllPods(), true)
				testCase.Reachability = reachability
				ProbePodToPodConnectivity(k, model, testCase)
				_, wrong, _, _ := reachability.Summary(ignoreLoopback)
				if wrong == 0 {
					framework.Logf("server %s is ready", caseName)
					delete(notReady, caseName)
				} else {
					framework.Logf("server %s is not ready", caseName)
				}
			}
		}
		if len(notReady) == 0 {
			return nil
		}
		time.Sleep(waitInterval)
	}
	return errors.Errorf("after %d tries, %d HTTP servers are not ready", maxTries, len(notReady))
}
