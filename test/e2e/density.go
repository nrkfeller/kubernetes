/*
Copyright 2015 The Kubernetes Authors.

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
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/client/cache"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	controllerframework "k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/sets"
	utiluuid "k8s.io/kubernetes/pkg/util/uuid"
	"k8s.io/kubernetes/pkg/watch"
	"k8s.io/kubernetes/test/e2e/framework"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	MinSaturationThreshold     = 2 * time.Minute
	MinPodsPerSecondThroughput = 8
)

// Maximum container failures this test tolerates before failing.
var MaxContainerFailures = 0

type DensityTestConfig struct {
	Configs      []framework.RCConfig
	Client       *client.Client
	Namespace    string
	PollInterval time.Duration
	PodCount     int
	Timeout      time.Duration
}

func density30AddonResourceVerifier(numNodes int) map[string]framework.ResourceConstraint {
	var apiserverMem uint64
	var controllerMem uint64
	var schedulerMem uint64
	apiserverCPU := math.MaxFloat32
	apiserverMem = math.MaxUint64
	controllerCPU := math.MaxFloat32
	controllerMem = math.MaxUint64
	schedulerCPU := math.MaxFloat32
	schedulerMem = math.MaxUint64
	framework.Logf("Setting resource constraings for provider: %s", framework.TestContext.Provider)
	if framework.ProviderIs("kubemark") {
		if numNodes <= 5 {
			apiserverCPU = 0.25
			apiserverMem = 150 * (1024 * 1024)
			controllerCPU = 0.1
			controllerMem = 100 * (1024 * 1024)
			schedulerCPU = 0.05
			schedulerMem = 50 * (1024 * 1024)
		} else if numNodes <= 100 {
			apiserverCPU = 1.5
			apiserverMem = 1500 * (1024 * 1024)
			controllerCPU = 0.75
			controllerMem = 750 * (1024 * 1024)
			schedulerCPU = 0.75
			schedulerMem = 500 * (1024 * 1024)
		} else if numNodes <= 500 {
			apiserverCPU = 2.5
			apiserverMem = 3400 * (1024 * 1024)
			controllerCPU = 1.3
			controllerMem = 1100 * (1024 * 1024)
			schedulerCPU = 1.5
			schedulerMem = 500 * (1024 * 1024)
		} else if numNodes <= 1000 {
			apiserverCPU = 4
			apiserverMem = 4000 * (1024 * 1024)
			controllerCPU = 3
			controllerMem = 2000 * (1024 * 1024)
			schedulerCPU = 1.5
			schedulerMem = 750 * (1024 * 1024)
		}
	} else {
		if numNodes <= 100 {
			// TODO: Investigate higher apiserver consumption and
			// potentially revert to 1.5cpu and 1.3GB - see #30871
			apiserverCPU = 1.8
			apiserverMem = 2200 * (1024 * 1024)
			controllerCPU = 0.5
			controllerMem = 300 * (1024 * 1024)
			schedulerCPU = 0.4
			schedulerMem = 150 * (1024 * 1024)
		}
	}

	constraints := make(map[string]framework.ResourceConstraint)
	constraints["fluentd-elasticsearch"] = framework.ResourceConstraint{
		CPUConstraint:    0.2,
		MemoryConstraint: 250 * (1024 * 1024),
	}
	constraints["elasticsearch-logging"] = framework.ResourceConstraint{
		CPUConstraint: 2,
		// TODO: bring it down to 750MB again, when we lower Kubelet verbosity level. I.e. revert #19164
		MemoryConstraint: 5000 * (1024 * 1024),
	}
	constraints["heapster"] = framework.ResourceConstraint{
		CPUConstraint:    2,
		MemoryConstraint: 1800 * (1024 * 1024),
	}
	constraints["kibana-logging"] = framework.ResourceConstraint{
		CPUConstraint:    0.2,
		MemoryConstraint: 100 * (1024 * 1024),
	}
	constraints["kube-proxy"] = framework.ResourceConstraint{
		CPUConstraint:    0.1,
		MemoryConstraint: 20 * (1024 * 1024),
	}
	constraints["l7-lb-controller"] = framework.ResourceConstraint{
		CPUConstraint:    0.1,
		MemoryConstraint: 60 * (1024 * 1024),
	}
	constraints["influxdb"] = framework.ResourceConstraint{
		CPUConstraint:    2,
		MemoryConstraint: 500 * (1024 * 1024),
	}
	constraints["kube-apiserver"] = framework.ResourceConstraint{
		CPUConstraint:    apiserverCPU,
		MemoryConstraint: apiserverMem,
	}
	constraints["kube-controller-manager"] = framework.ResourceConstraint{
		CPUConstraint:    controllerCPU,
		MemoryConstraint: controllerMem,
	}
	constraints["kube-scheduler"] = framework.ResourceConstraint{
		CPUConstraint:    schedulerCPU,
		MemoryConstraint: schedulerMem,
	}
	return constraints
}

func logPodStartupStatus(c *client.Client, expectedPods int, ns string, observedLabels map[string]string, period time.Duration, stopCh chan struct{}) {
	label := labels.SelectorFromSet(labels.Set(observedLabels))
	podStore := framework.NewPodStore(c, ns, label, fields.Everything())
	defer podStore.Stop()
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pods := podStore.List()
			startupStatus := framework.ComputeRCStartupStatus(pods, expectedPods)
			startupStatus.Print("Density")
		case <-stopCh:
			pods := podStore.List()
			startupStatus := framework.ComputeRCStartupStatus(pods, expectedPods)
			startupStatus.Print("Density")
			return
		}
	}
}

// runDensityTest will perform a density test and return the time it took for
// all pods to start
func runDensityTest(dtc DensityTestConfig) time.Duration {
	defer GinkgoRecover()
	// Create a listener for events.
	// eLock is a lock protects the events
	var eLock sync.Mutex
	events := make([](*api.Event), 0)
	_, controller := controllerframework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dtc.Client.Events(dtc.Namespace).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dtc.Client.Events(dtc.Namespace).Watch(options)
			},
		},
		&api.Event{},
		0,
		controllerframework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				eLock.Lock()
				defer eLock.Unlock()
				events = append(events, obj.(*api.Event))
			},
		},
	)
	stop := make(chan struct{})
	go controller.Run(stop)

	// Create a listener for api updates
	// uLock is a lock protects the updateCount
	var uLock sync.Mutex
	updateCount := 0
	label := labels.SelectorFromSet(labels.Set(map[string]string{"type": "densityPod"}))
	_, updateController := controllerframework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				options.LabelSelector = label
				return dtc.Client.Pods(dtc.Namespace).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				options.LabelSelector = label
				return dtc.Client.Pods(dtc.Namespace).Watch(options)
			},
		},
		&api.Pod{},
		0,
		controllerframework.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, _ interface{}) {
				uLock.Lock()
				defer uLock.Unlock()
				updateCount++
			},
		},
	)
	go updateController.Run(stop)

	// Start all replication controllers.
	startTime := time.Now()
	wg := sync.WaitGroup{}
	wg.Add(len(dtc.Configs))
	for i := range dtc.Configs {
		rcConfig := dtc.Configs[i]
		go func() {
			framework.ExpectNoError(framework.RunRC(rcConfig))
			wg.Done()
		}()
	}
	logStopCh := make(chan struct{})
	go logPodStartupStatus(dtc.Client, dtc.PodCount, dtc.Namespace, map[string]string{"type": "densityPod"}, dtc.PollInterval, logStopCh)
	wg.Wait()
	startupTime := time.Now().Sub(startTime)
	close(logStopCh)
	framework.Logf("E2E startup time for %d pods: %v", dtc.PodCount, startupTime)
	framework.Logf("Throughput (pods/s) during cluster saturation phase: %v", float32(dtc.PodCount)/float32(startupTime/time.Second))

	By("Waiting for all events to be recorded")
	last := -1
	current := len(events)
	lastCount := -1
	currentCount := updateCount
	for start := time.Now(); (last < current || lastCount < currentCount) && time.Since(start) < dtc.Timeout; time.Sleep(10 * time.Second) {
		func() {
			eLock.Lock()
			defer eLock.Unlock()
			last = current
			current = len(events)
		}()
		func() {
			uLock.Lock()
			defer uLock.Unlock()
			lastCount = currentCount
			currentCount = updateCount
		}()
	}
	close(stop)

	if current != last {
		framework.Logf("Warning: Not all events were recorded after waiting %.2f minutes", dtc.Timeout.Minutes())
	}
	framework.Logf("Found %d events", current)
	if currentCount != lastCount {
		framework.Logf("Warning: Not all updates were recorded after waiting %.2f minutes", dtc.Timeout.Minutes())
	}
	framework.Logf("Found %d updates", currentCount)

	// Tune the threshold for allowed failures.
	badEvents := framework.BadEvents(events)
	Expect(badEvents).NotTo(BeNumerically(">", int(math.Floor(0.01*float64(dtc.PodCount)))))
	// Print some data about Pod to Node allocation
	By("Printing Pod to Node allocation data")
	podList, err := dtc.Client.Pods(api.NamespaceAll).List(api.ListOptions{})
	framework.ExpectNoError(err)
	pausePodAllocation := make(map[string]int)
	systemPodAllocation := make(map[string][]string)
	for _, pod := range podList.Items {
		if pod.Namespace == api.NamespaceSystem {
			systemPodAllocation[pod.Spec.NodeName] = append(systemPodAllocation[pod.Spec.NodeName], pod.Name)
		} else {
			pausePodAllocation[pod.Spec.NodeName]++
		}
	}
	nodeNames := make([]string, 0)
	for k := range pausePodAllocation {
		nodeNames = append(nodeNames, k)
	}
	sort.Strings(nodeNames)
	for _, node := range nodeNames {
		framework.Logf("%v: %v pause pods, system pods: %v", node, pausePodAllocation[node], systemPodAllocation[node])
	}
	return startupTime
}

func cleanupDensityTest(dtc DensityTestConfig) {
	defer GinkgoRecover()
	By("Deleting ReplicationController")
	// We explicitly delete all pods to have API calls necessary for deletion accounted in metrics.
	for i := range dtc.Configs {
		rcName := dtc.Configs[i].Name
		rc, err := dtc.Client.ReplicationControllers(dtc.Namespace).Get(rcName)
		if err == nil && rc.Spec.Replicas != 0 {
			if framework.TestContext.GarbageCollectorEnabled {
				By("Cleaning up only the replication controller, garbage collector will clean up the pods")
				err := framework.DeleteRCAndWaitForGC(dtc.Client, dtc.Namespace, rcName)
				framework.ExpectNoError(err)
			} else {
				By("Cleaning up the replication controller and pods")
				err := framework.DeleteRCAndPods(dtc.Client, dtc.Namespace, rcName)
				framework.ExpectNoError(err)
			}
		}
	}
}

// This test suite can take a long time to run, and can affect or be affected by other tests.
// So by default it is added to the ginkgo.skip list (see driver.go).
// To run this suite you must explicitly ask for it by setting the
// -t/--test flag or ginkgo.focus flag.
// IMPORTANT: This test is designed to work on large (>= 100 Nodes) clusters. For smaller ones
// results will not be representative for control-plane performance as we'll start hitting
// limits on Docker's concurrent container startup.
var _ = framework.KubeDescribe("Density", func() {
	var c *client.Client
	var nodeCount int
	var RCName string
	var additionalPodsPrefix string
	var ns string
	var uuid string
	var e2eStartupTime time.Duration
	var totalPods int
	var nodeCpuCapacity int64
	var nodeMemCapacity int64
	var nodes *api.NodeList
	var masters sets.String

	// Gathers data prior to framework namespace teardown
	AfterEach(func() {
		saturationThreshold := time.Duration((totalPods / MinPodsPerSecondThroughput)) * time.Second
		if saturationThreshold < MinSaturationThreshold {
			saturationThreshold = MinSaturationThreshold
		}
		Expect(e2eStartupTime).NotTo(BeNumerically(">", saturationThreshold))
		saturationData := framework.SaturationTime{
			TimeToSaturate: e2eStartupTime,
			NumberOfNodes:  nodeCount,
			NumberOfPods:   totalPods,
			Throughput:     float32(totalPods) / float32(e2eStartupTime/time.Second),
		}
		framework.Logf("Cluster saturation time: %s", framework.PrettyPrintJSON(saturationData))

		// Verify latency metrics.
		highLatencyRequests, err := framework.HighLatencyRequests(c)
		framework.ExpectNoError(err)
		Expect(highLatencyRequests).NotTo(BeNumerically(">", 0), "There should be no high-latency requests")

		// Verify scheduler metrics.
		// TODO: Reset metrics at the beginning of the test.
		// We should do something similar to how we do it for APIserver.
		framework.ExpectNoError(framework.VerifySchedulerLatency(c))
	})

	// Explicitly put here, to delete namespace at the end of the test
	// (after measuring latency metrics, etc.).
	f := framework.NewDefaultFramework("density")
	f.NamespaceDeletionTimeout = time.Hour

	BeforeEach(func() {
		c = f.Client
		ns = f.Namespace.Name

		// In large clusters we may get to this point but still have a bunch
		// of nodes without Routes created. Since this would make a node
		// unschedulable, we need to wait until all of them are schedulable.
		framework.ExpectNoError(framework.WaitForAllNodesSchedulable(c))
		masters, nodes = framework.GetMasterAndWorkerNodesOrDie(c)
		nodeCount = len(nodes.Items)
		Expect(nodeCount).NotTo(BeZero())

		nodeCpuCapacity = nodes.Items[0].Status.Allocatable.Cpu().MilliValue()
		nodeMemCapacity = nodes.Items[0].Status.Allocatable.Memory().Value()

		// Terminating a namespace (deleting the remaining objects from it - which
		// generally means events) can affect the current run. Thus we wait for all
		// terminating namespace to be finally deleted before starting this test.
		err := framework.CheckTestingNSDeletedExcept(c, ns)
		framework.ExpectNoError(err)

		uuid = string(utiluuid.NewUUID())

		framework.ExpectNoError(framework.ResetMetrics(c))
		framework.ExpectNoError(os.Mkdir(fmt.Sprintf(framework.TestContext.OutputDir+"/%s", uuid), 0777))

		framework.Logf("Listing nodes for easy debugging:\n")
		for _, node := range nodes.Items {
			var internalIP, externalIP string
			for _, address := range node.Status.Addresses {
				if address.Type == api.NodeInternalIP {
					internalIP = address.Address
				}
				if address.Type == api.NodeExternalIP {
					externalIP = address.Address
				}
			}
			framework.Logf("Name: %v, clusterIP: %v, externalIP: %v", node.ObjectMeta.Name, internalIP, externalIP)
		}
	})

	type Density struct {
		// Controls if e2e latency tests should be run (they are slow)
		runLatencyTest bool
		podsPerNode    int
		// Controls how often the apiserver is polled for pods
		interval time.Duration
	}

	densityTests := []Density{
		// TODO: Expose runLatencyTest as ginkgo flag.
		{podsPerNode: 3, runLatencyTest: false, interval: 10 * time.Second},
		{podsPerNode: 30, runLatencyTest: true, interval: 10 * time.Second},
		{podsPerNode: 50, runLatencyTest: false, interval: 10 * time.Second},
		{podsPerNode: 95, runLatencyTest: true, interval: 10 * time.Second},
		{podsPerNode: 100, runLatencyTest: false, interval: 10 * time.Second},
	}

	for _, testArg := range densityTests {
		name := fmt.Sprintf("should allow starting %d pods per node", testArg.podsPerNode)
		switch testArg.podsPerNode {
		case 30:
			name = "[Feature:Performance] " + name
		case 95:
			name = "[Feature:HighDensityPerformance]" + name
		default:
			name = "[Feature:ManualPerformance] " + name
		}
		itArg := testArg
		It(name, func() {
			podsPerNode := itArg.podsPerNode
			if podsPerNode == 30 {
				f.AddonResourceConstraints = func() map[string]framework.ResourceConstraint { return density30AddonResourceVerifier(nodeCount) }()
			}
			totalPods = podsPerNode * nodeCount
			fileHndl, err := os.Create(fmt.Sprintf(framework.TestContext.OutputDir+"/%s/pod_states.csv", uuid))
			framework.ExpectNoError(err)
			defer fileHndl.Close()
			timeout := 10 * time.Minute

			// TODO: loop to podsPerNode instead of 1 when we're ready.
			numberOrRCs := 1
			RCConfigs := make([]framework.RCConfig, numberOrRCs)
			for i := 0; i < numberOrRCs; i++ {
				RCName := "density" + strconv.Itoa(totalPods) + "-" + strconv.Itoa(i) + "-" + uuid
				RCConfigs[i] = framework.RCConfig{Client: c,
					Image:                framework.GetPauseImageName(f.Client),
					Name:                 RCName,
					Namespace:            ns,
					Labels:               map[string]string{"type": "densityPod"},
					PollInterval:         itArg.interval,
					PodStatusFile:        fileHndl,
					Replicas:             (totalPods + numberOrRCs - 1) / numberOrRCs,
					CpuRequest:           nodeCpuCapacity / 100,
					MemRequest:           nodeMemCapacity / 100,
					MaxContainerFailures: &MaxContainerFailures,
					Silent:               true,
				}
			}

			dConfig := DensityTestConfig{Client: c,
				Configs:      RCConfigs,
				PodCount:     totalPods,
				Namespace:    ns,
				PollInterval: itArg.interval,
				Timeout:      timeout,
			}
			e2eStartupTime = runDensityTest(dConfig)
			if itArg.runLatencyTest {
				By("Scheduling additional Pods to measure startup latencies")

				createTimes := make(map[string]unversioned.Time, 0)
				nodes := make(map[string]string, 0)
				scheduleTimes := make(map[string]unversioned.Time, 0)
				runTimes := make(map[string]unversioned.Time, 0)
				watchTimes := make(map[string]unversioned.Time, 0)

				var mutex sync.Mutex
				checkPod := func(p *api.Pod) {
					mutex.Lock()
					defer mutex.Unlock()
					defer GinkgoRecover()

					if p.Status.Phase == api.PodRunning {
						if _, found := watchTimes[p.Name]; !found {
							watchTimes[p.Name] = unversioned.Now()
							createTimes[p.Name] = p.CreationTimestamp
							nodes[p.Name] = p.Spec.NodeName
							var startTime unversioned.Time
							for _, cs := range p.Status.ContainerStatuses {
								if cs.State.Running != nil {
									if startTime.Before(cs.State.Running.StartedAt) {
										startTime = cs.State.Running.StartedAt
									}
								}
							}
							if startTime != unversioned.NewTime(time.Time{}) {
								runTimes[p.Name] = startTime
							} else {
								framework.Failf("Pod %v is reported to be running, but none of its containers is", p.Name)
							}
						}
					}
				}

				additionalPodsPrefix = "density-latency-pod"
				latencyPodsStore, controller := controllerframework.NewInformer(
					&cache.ListWatch{
						ListFunc: func(options api.ListOptions) (runtime.Object, error) {
							options.LabelSelector = labels.SelectorFromSet(labels.Set{"type": additionalPodsPrefix})
							return c.Pods(ns).List(options)
						},
						WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
							options.LabelSelector = labels.SelectorFromSet(labels.Set{"type": additionalPodsPrefix})
							return c.Pods(ns).Watch(options)
						},
					},
					&api.Pod{},
					0,
					controllerframework.ResourceEventHandlerFuncs{
						AddFunc: func(obj interface{}) {
							p, ok := obj.(*api.Pod)
							Expect(ok).To(Equal(true))
							go checkPod(p)
						},
						UpdateFunc: func(oldObj, newObj interface{}) {
							p, ok := newObj.(*api.Pod)
							Expect(ok).To(Equal(true))
							go checkPod(p)
						},
					},
				)

				stopCh := make(chan struct{})
				go controller.Run(stopCh)

				// Create some additional pods with throughput ~5 pods/sec.
				var wg sync.WaitGroup
				wg.Add(nodeCount)
				// Explicitly set requests here.
				// Thanks to it we trigger increasing priority function by scheduling
				// a pod to a node, which in turn will result in spreading latency pods
				// more evenly between nodes.
				cpuRequest := *resource.NewMilliQuantity(nodeCpuCapacity/5, resource.DecimalSI)
				memRequest := *resource.NewQuantity(nodeMemCapacity/5, resource.DecimalSI)
				if podsPerNode > 30 {
					// This is to make them schedulable on high-density tests
					// (e.g. 100 pods/node kubemark).
					cpuRequest = *resource.NewMilliQuantity(0, resource.DecimalSI)
					memRequest = *resource.NewQuantity(0, resource.DecimalSI)
				}
				for i := 1; i <= nodeCount; i++ {
					name := additionalPodsPrefix + "-" + strconv.Itoa(i)
					go createRunningPodFromRC(&wg, c, name, ns, framework.GetPauseImageName(f.Client), additionalPodsPrefix, cpuRequest, memRequest)
					time.Sleep(200 * time.Millisecond)
				}
				wg.Wait()

				By("Waiting for all Pods begin observed by the watch...")
				for start := time.Now(); len(watchTimes) < nodeCount; time.Sleep(10 * time.Second) {
					if time.Since(start) < timeout {
						framework.Failf("Timeout reached waiting for all Pods being observed by the watch.")
					}
				}
				close(stopCh)

				nodeToLatencyPods := make(map[string]int)
				for _, item := range latencyPodsStore.List() {
					pod := item.(*api.Pod)
					nodeToLatencyPods[pod.Spec.NodeName]++
				}
				for node, count := range nodeToLatencyPods {
					if count > 1 {
						framework.Logf("%d latency pods scheduled on %s", count, node)
					}
				}

				selector := fields.Set{
					"involvedObject.kind":      "Pod",
					"involvedObject.namespace": ns,
					"source":                   api.DefaultSchedulerName,
				}.AsSelector()
				options := api.ListOptions{FieldSelector: selector}
				schedEvents, err := c.Events(ns).List(options)
				framework.ExpectNoError(err)
				for k := range createTimes {
					for _, event := range schedEvents.Items {
						if event.InvolvedObject.Name == k {
							scheduleTimes[k] = event.FirstTimestamp
							break
						}
					}
				}

				scheduleLag := make([]framework.PodLatencyData, 0)
				startupLag := make([]framework.PodLatencyData, 0)
				watchLag := make([]framework.PodLatencyData, 0)
				schedToWatchLag := make([]framework.PodLatencyData, 0)
				e2eLag := make([]framework.PodLatencyData, 0)

				for name, create := range createTimes {
					sched, ok := scheduleTimes[name]
					Expect(ok).To(Equal(true))
					run, ok := runTimes[name]
					Expect(ok).To(Equal(true))
					watch, ok := watchTimes[name]
					Expect(ok).To(Equal(true))
					node, ok := nodes[name]
					Expect(ok).To(Equal(true))

					scheduleLag = append(scheduleLag, framework.PodLatencyData{Name: name, Node: node, Latency: sched.Time.Sub(create.Time)})
					startupLag = append(startupLag, framework.PodLatencyData{Name: name, Node: node, Latency: run.Time.Sub(sched.Time)})
					watchLag = append(watchLag, framework.PodLatencyData{Name: name, Node: node, Latency: watch.Time.Sub(run.Time)})
					schedToWatchLag = append(schedToWatchLag, framework.PodLatencyData{Name: name, Node: node, Latency: watch.Time.Sub(sched.Time)})
					e2eLag = append(e2eLag, framework.PodLatencyData{Name: name, Node: node, Latency: watch.Time.Sub(create.Time)})
				}

				sort.Sort(framework.LatencySlice(scheduleLag))
				sort.Sort(framework.LatencySlice(startupLag))
				sort.Sort(framework.LatencySlice(watchLag))
				sort.Sort(framework.LatencySlice(schedToWatchLag))
				sort.Sort(framework.LatencySlice(e2eLag))

				framework.PrintLatencies(scheduleLag, "worst schedule latencies")
				framework.PrintLatencies(startupLag, "worst run-after-schedule latencies")
				framework.PrintLatencies(watchLag, "worst watch latencies")
				framework.PrintLatencies(schedToWatchLag, "worst scheduled-to-end total latencies")
				framework.PrintLatencies(e2eLag, "worst e2e total latencies")

				// Test whether e2e pod startup time is acceptable.
				podStartupLatency := framework.PodStartupLatency{Latency: framework.ExtractLatencyMetrics(e2eLag)}
				framework.ExpectNoError(framework.VerifyPodStartupLatency(podStartupLatency))

				framework.LogSuspiciousLatency(startupLag, e2eLag, nodeCount, c)

				By("Removing additional replication controllers")
				for i := 1; i <= nodeCount; i++ {
					name := additionalPodsPrefix + "-" + strconv.Itoa(i)
					c.ReplicationControllers(ns).Delete(name, nil)
				}
			}

			cleanupDensityTest(dConfig)
		})
	}

	// Calculate total number of pods from each node's max-pod
	It("[Feature:ManualPerformance] should allow running maximum capacity pods on nodes", func() {
		totalPods = 0
		for _, n := range nodes.Items {
			totalPods += int(n.Status.Capacity.Pods().Value())
		}
		totalPods -= framework.WaitForStableCluster(c, masters)

		fileHndl, err := os.Create(fmt.Sprintf(framework.TestContext.OutputDir+"/%s/pod_states.csv", uuid))
		framework.ExpectNoError(err)
		defer fileHndl.Close()
		rcCnt := 1
		RCConfigs := make([]framework.RCConfig, rcCnt)
		podsPerRC := int(totalPods / rcCnt)
		for i := 0; i < rcCnt; i++ {
			if i == rcCnt-1 {
				podsPerRC += int(math.Mod(float64(totalPods), float64(rcCnt)))
			}
			RCName = "density" + strconv.Itoa(totalPods) + "-" + strconv.Itoa(i) + "-" + uuid
			RCConfigs[i] = framework.RCConfig{Client: c,
				Image:                framework.GetPauseImageName(f.Client),
				Name:                 RCName,
				Namespace:            ns,
				Labels:               map[string]string{"type": "densityPod"},
				PollInterval:         10 * time.Second,
				PodStatusFile:        fileHndl,
				Replicas:             podsPerRC,
				MaxContainerFailures: &MaxContainerFailures,
				Silent:               true,
			}
		}
		dConfig := DensityTestConfig{Client: c,
			Configs:      RCConfigs,
			PodCount:     totalPods,
			Namespace:    ns,
			PollInterval: 10 * time.Second,
			Timeout:      10 * time.Minute,
		}
		e2eStartupTime = runDensityTest(dConfig)
		cleanupDensityTest(dConfig)
	})
})

func createRunningPodFromRC(wg *sync.WaitGroup, c *client.Client, name, ns, image, podType string, cpuRequest, memRequest resource.Quantity) {
	defer GinkgoRecover()
	defer wg.Done()
	labels := map[string]string{
		"type": podType,
		"name": name,
	}
	rc := &api.ReplicationController{
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: api.ReplicationControllerSpec{
			Replicas: 1,
			Selector: labels,
			Template: &api.PodTemplateSpec{
				ObjectMeta: api.ObjectMeta{
					Labels: labels,
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  name,
							Image: image,
							Resources: api.ResourceRequirements{
								Requests: api.ResourceList{
									api.ResourceCPU:    cpuRequest,
									api.ResourceMemory: memRequest,
								},
							},
						},
					},
					DNSPolicy: api.DNSDefault,
				},
			},
		},
	}
	_, err := c.ReplicationControllers(ns).Create(rc)
	framework.ExpectNoError(err)
	framework.ExpectNoError(framework.WaitForRCPodsRunning(c, ns, name))
	framework.Logf("Found pod '%s' running", name)
}
