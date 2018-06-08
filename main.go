package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"sync"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig string

	kubeClient *kubernetes.Clientset
)

func init() {
	flag.StringVar(&kubeconfig, "c", "~/.kube/config", "Path to the kubeconfig file")
	flag.Parse()
}

func main() {
	kubeClient = NewKubernetesClient(kubeconfig)
	nodeList, podList := getNodesAndPods()
	metrics := compute(nodeList, podList)
	printToJson(metrics)
}

func getNodesAndPods() (*v1.NodeList, *v1.PodList) {
	var nodeList *v1.NodeList
	var podList *v1.PodList

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		nodeList, err = kubeClient.Core().Nodes().List(metav1.ListOptions{})
		if err != nil {
			fatal(err, "list nodes")
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		podList, err = kubeClient.Core().Pods("").List(metav1.ListOptions{})
		if err != nil {
			fatal(err, "list pods")
		}
	}()
	wg.Wait()

	return nodeList, podList
}

type Use struct {
	Nodes map[string]NodeUse
	Total TotalUse
}

type NodeUse struct {
	Pods  map[string]PodUse
	Total TotalUse
}

type PodUse struct {
	CpuRequested float64
	CpuLimits    float64
	MemRequested float64
	MemLimits    float64
}

type TotalUse struct {
	CpuAllocable   float64
	MemAllocable   float64
	CpuRequested   float64
	CpuLimits      float64
	MemRequested   float64
	MemLimits      float64
	CpuPercentUsed float64
	MemPercentUsed float64
}

func compute(nodeList *v1.NodeList, podList *v1.PodList) Use {
	use := Use{
		Nodes: map[string]NodeUse{},
		Total: TotalUse{},
	}

	for _, node := range nodeList.Items {
		cpuAlloc := cpuFloat64(*node.Status.Allocatable.Cpu())
		memAlloc := memFloat64(*node.Status.Allocatable.Memory())

		use.Nodes[node.Name] = NodeUse{
			Pods: map[string]PodUse{},
			Total: TotalUse{
				CpuAllocable: cpuAlloc,
				MemAllocable: memAlloc,
			},
		}

		use.Total.CpuAllocable += cpuAlloc
		use.Total.MemAllocable += memAlloc
	}

	for _, pod := range podList.Items {
		reqs, limits := PodRequestsAndLimits(pod)

		// Set pod use
		cpuReqQ := reqs["cpu"]
		memReqQ := reqs["memory"]
		cpuLimitQ := limits["cpu"]
		memLimitQ := limits["memory"]
		cpuReq := cpuFloat64(cpuReqQ)
		cpuLimit := cpuFloat64(cpuLimitQ)
		memReq := memFloat64(memReqQ)
		memLimit := memFloat64(memLimitQ)
		podUse := use.Nodes[pod.Spec.NodeName].Pods[pod.Name]
		podUse.CpuRequested += cpuReq
		podUse.MemRequested += memReq
		podUse.CpuLimits += cpuLimit
		podUse.MemLimits += memLimit
		use.Nodes[pod.Spec.NodeName].Pods[pod.Name] = podUse

		// Incr node total use
		nodeUse := use.Nodes[pod.Spec.NodeName]
		nodeUse.Total.CpuRequested += cpuReq
		nodeUse.Total.MemRequested += memReq
		nodeUse.Total.CpuLimits += cpuLimit
		nodeUse.Total.MemLimits += memLimit
		use.Nodes[pod.Spec.NodeName] = nodeUse

		// Incr global total use
		use.Total.CpuRequested += cpuReq
		use.Total.MemRequested += memReq
		use.Total.CpuLimits += cpuLimit
		use.Total.MemLimits += memLimit
	}

	for _, node := range nodeList.Items {
		nodeUse := use.Nodes[node.Name]
		nodeUse.Total.CpuPercentUsed = percent(nodeUse.Total.CpuRequested, nodeUse.Total.CpuAllocable)
		nodeUse.Total.MemPercentUsed = percent(nodeUse.Total.MemRequested, nodeUse.Total.MemAllocable)
		use.Nodes[node.Name] = nodeUse
	}

	use.Total.CpuPercentUsed = percent(use.Total.CpuRequested, use.Total.CpuAllocable)
	use.Total.MemPercentUsed = percent(use.Total.MemRequested, use.Total.MemAllocable)

	return use
}

func cpuFloat64(q resource.Quantity) float64 {
	return float64(q.MilliValue()) / 1000
}

func memFloat64(q resource.Quantity) float64 {
	return float64(q.Value()) / 1024
}

func percent(part float64, total float64) float64 {
	percent := part * 100 / total
	unit := float64(100)
	return math.Round(percent*unit) / unit
}

func printToJson(obj interface{}) {
	jso, err := json.Marshal(obj)
	if err != nil {
		fatal(err, "marshal obj")
	}

	fmt.Println(string(jso))
}

func NewKubernetesClient(kubeconfig string) *kubernetes.Clientset {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fatal(err, "read kubernetes config")
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		fatal(err, "create kubernetes client")
	}

	return kubeClient
}

// https://github.com/kubernetes/kubernetes/blob/master/pkg/api/resource/helpers.go#L30
func PodRequestsAndLimits(pod v1.Pod) (reqs map[v1.ResourceName]resource.Quantity, limits map[v1.ResourceName]resource.Quantity) {
	reqs, limits = map[v1.ResourceName]resource.Quantity{}, map[v1.ResourceName]resource.Quantity{}
	for _, container := range pod.Spec.Containers {
		for name, quantity := range container.Resources.Requests {
			if value, ok := reqs[name]; !ok {
				reqs[name] = *quantity.Copy()
			} else {
				value.Add(quantity)
				reqs[name] = value
			}
		}
		for name, quantity := range container.Resources.Limits {
			if value, ok := limits[name]; !ok {
				limits[name] = *quantity.Copy()
			} else {
				value.Add(quantity)
				limits[name] = value
			}
		}
	}
	// init containers define the minimum of any resource
	for _, container := range pod.Spec.InitContainers {
		for name, quantity := range container.Resources.Requests {
			value, ok := reqs[name]
			if !ok {
				reqs[name] = *quantity.Copy()
				continue
			}
			if quantity.Cmp(value) > 0 {
				reqs[name] = *quantity.Copy()
			}
		}
		for name, quantity := range container.Resources.Limits {
			value, ok := limits[name]
			if !ok {
				limits[name] = *quantity.Copy()
				continue
			}
			if quantity.Cmp(value) > 0 {
				limits[name] = *quantity.Copy()
			}
		}
	}
	return
}

func fatal(err error, msg string) {
	log.WithError(err).Fatal("fail to " + msg)
}
