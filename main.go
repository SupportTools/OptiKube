package main

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"embed"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type PodData struct {
	Name             string
	CPURequest       int64
	CPULimit         int64
	MemoryRequest    int64
	MemoryLimit      int64
	EfficiencyCPU    float64
	EfficiencyMemory float64
}

type NamespaceData struct {
	Name               string
	Pods               []*PodData
	TotalCPURequest    int64
	TotalCPULimit      int64
	TotalMemoryRequest int64
	TotalMemoryLimit   int64
}

type ClusterData struct {
	Namespaces         []*NamespaceData
	TotalCPURequest    int64
	TotalCPULimit      int64
	TotalMemoryRequest int64
	TotalMemoryLimit   int64
}

var clusterData *ClusterData
var mu sync.Mutex

var funcMap = template.FuncMap{
	"mul": func(a, b int64) int64 {
		return a * b
	},
}

//go:embed index.html
var content embed.FS

var tmpl = template.Must(template.New("tmpl").Funcs(funcMap).ParseFS(content, "index.html"))

func main() {
	log.Println("OptiKube is starting...")

	var config *rest.Config
	var err error

	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		log.Println("Using out-of-cluster configuration")
		kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("Failed to build config: %v", err)
		}
	} else {
		log.Println("Using in-cluster configuration")
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("Failed to build in-cluster config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	clusterData = fetchClusterData(clientset)
	log.Println("Initial cluster data fetched")

	ticker := time.NewTicker(15 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				log.Println("Fetching updated cluster data")
				mu.Lock()
				clusterData = fetchClusterData(clientset)
				mu.Unlock()
			}
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		err := tmpl.ExecuteTemplate(w, "index.html", clusterData)
		mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		log.Println("Handled web request")
	})

	log.Println("Starting web server")
	http.ListenAndServe(":8080", nil)
}

func fetchClusterData(clientset *kubernetes.Clientset) *ClusterData {
	log.Println("Processing namespaces...")
	namespaceList, err := clientset.CoreV1().Namespaces().List(context.Background(), v1.ListOptions{})
	if err != nil {
		log.Fatalf("Failed to get namespace list: %v", err)
	}

	clusterData := &ClusterData{
		Namespaces: make([]*NamespaceData, 0),
	}

	for _, ns := range namespaceList.Items {
		nsData := processNamespace(clientset, &ns)
		clusterData.Namespaces = append(clusterData.Namespaces, nsData)

		clusterData.TotalCPURequest += nsData.TotalCPURequest
		clusterData.TotalCPULimit += nsData.TotalCPULimit
		clusterData.TotalMemoryRequest += nsData.TotalMemoryRequest
		clusterData.TotalMemoryLimit += nsData.TotalMemoryLimit
	}
	log.Println("Namespaces processed")
	return clusterData
}

func processNamespace(clientset *kubernetes.Clientset, namespace *corev1.Namespace) *NamespaceData {
	log.Printf("Processing namespace: %s\n", namespace.Name)
	podList, _ := clientset.CoreV1().Pods(namespace.Name).List(context.Background(), v1.ListOptions{})
	nsData := &NamespaceData{Name: namespace.Name, Pods: make([]*PodData, 0)}

	for _, pod := range podList.Items {
		podData := processPod(&pod)
		nsData.Pods = append(nsData.Pods, podData)

		nsData.TotalCPURequest += podData.CPURequest
		nsData.TotalCPULimit += podData.CPULimit
		nsData.TotalMemoryRequest += podData.MemoryRequest
		nsData.TotalMemoryLimit += podData.MemoryLimit
	}
	log.Printf("Namespace processed: %s\n", namespace.Name)
	return nsData
}

func processPod(pod *corev1.Pod) *PodData {
	var CPURequest, CPULimit, MemoryRequest, MemoryLimit int64

	for _, container := range pod.Spec.Containers {
		if container.Resources.Requests != nil {
			if quantity, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				CPURequest += quantity.MilliValue()
			}

			if quantity, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				MemoryRequest += quantity.Value() / (1024 * 1024)
			}
		}

		if container.Resources.Limits != nil {
			if quantity, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
				CPULimit += quantity.MilliValue()
			}

			if quantity, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				MemoryLimit += quantity.Value() / (1024 * 1024)
			}
		}
	}

	return &PodData{
		Name:             pod.Name,
		CPURequest:       CPURequest,
		CPULimit:         CPULimit,
		MemoryRequest:    MemoryRequest,
		MemoryLimit:      MemoryLimit,
		EfficiencyCPU:    float64(CPURequest) / float64(CPULimit) * 100,
		EfficiencyMemory: float64(MemoryRequest) / float64(MemoryLimit) * 100,
	}
}
