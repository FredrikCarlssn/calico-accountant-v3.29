package watch

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Simple structures to represent the data we need
type WorkloadEndpoint struct {
	Name      string
	Namespace string
	Node      string
	IP        string
	Labels    map[string]string
}

type CalicoWatcher interface {
	GetPolicyByChainName(string) string
	ListWorkloadEndpoints() []*WorkloadEndpoint
}

type calicoWatcher struct {
	policyCache        map[string]string
	policyCacheMtx     sync.RWMutex
	workloadCache      map[string]*WorkloadEndpoint
	workloadCacheMtx   sync.RWMutex
	client             dynamic.Interface
	node               string
	ready              chan struct{}
	ctx                context.Context
	cancel             context.CancelFunc
}

var (
	// Calico CRD schema definitions
	networkPolicyGVR = schema.GroupVersionResource{
		Group:    "crd.projectcalico.org",
		Version:  "v1",
		Resource: "networkpolicies",
	}
	globalNetworkPolicyGVR = schema.GroupVersionResource{
		Group:    "crd.projectcalico.org",
		Version:  "v1",
		Resource: "globalnetworkpolicies",
	}
	workloadEndpointGVR = schema.GroupVersionResource{
		Group:    "crd.projectcalico.org",
		Version:  "v1",
		Resource: "workloadendpoints",
	}
)

func New() (CalicoWatcher, error) {
	// Create Kubernetes client configuration
	config, err := getKubeConfig()
	if err != nil {
		return nil, err
	}

	// Create dynamic client
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	cw := &calicoWatcher{
		policyCache:   map[string]string{},
		workloadCache: map[string]*WorkloadEndpoint{},
		client:        client,
		node:          getNodeName(),
		ready:         make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}

	// Start polling for updates
	go cw.pollUpdates()

	// Initial sync
	go func() {
		if err := cw.syncPolicies(); err != nil {
			glog.Errorf("Error syncing policies: %v", err)
		}
		if err := cw.syncWorkloadEndpoints(); err != nil {
			glog.Errorf("Error syncing workload endpoints: %v", err)
		}
		close(cw.ready)
		glog.Info("Calico cache filled")
	}()

	<-cw.ready
	return cw, nil
}

func getKubeConfig() (*rest.Config, error) {
	// Try in-cluster config first
	if config, err := rest.InClusterConfig(); err == nil {
		return config, nil
	}

	// Fall back to kubeconfig file
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home := os.Getenv("HOME")
		if home != "" {
			kubeconfig = home + "/.kube/config"
		}
	}

	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func (cw *calicoWatcher) GetPolicyByChainName(chain string) string {
	cw.policyCacheMtx.RLock()
	defer cw.policyCacheMtx.RUnlock()
	return cw.policyCache[chain]
}

func (cw *calicoWatcher) ListWorkloadEndpoints() []*WorkloadEndpoint {
	cw.workloadCacheMtx.RLock()
	defer cw.workloadCacheMtx.RUnlock()

	ws := make([]*WorkloadEndpoint, 0, len(cw.workloadCache))
	for _, v := range cw.workloadCache {
		ws = append(ws, v)
	}
	return ws
}

func (cw *calicoWatcher) pollUpdates() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cw.ctx.Done():
			return
		case <-ticker.C:
			if err := cw.syncPolicies(); err != nil {
				glog.Errorf("Error syncing policies: %v", err)
			}
			if err := cw.syncWorkloadEndpoints(); err != nil {
				glog.Errorf("Error syncing workload endpoints: %v", err)
			}
		}
	}
}

func (cw *calicoWatcher) syncPolicies() error {
	cw.policyCacheMtx.Lock()
	defer cw.policyCacheMtx.Unlock()

	// Clear old policies
	cw.policyCache = make(map[string]string)

	// List network policies
	policies, err := cw.client.Resource(networkPolicyGVR).List(cw.ctx, metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Error listing network policies (might not exist in this Calico version): %v", err)
		// Don't fail if network policies don't exist
	} else {
		for _, policy := range policies.Items {
			name := policy.GetName()
			// Generate chain names for policy (simplified approach)
			inboundChain := "cali-pi-" + name
			outboundChain := "cali-po-" + name
			
			cw.policyCache[inboundChain] = name
			cw.policyCache[outboundChain] = name
			glog.V(2).Infof("Storing policy %s against chain names %s, %s", name, inboundChain, outboundChain)
		}
	}

	// List global network policies
	globalPolicies, err := cw.client.Resource(globalNetworkPolicyGVR).List(cw.ctx, metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Error listing global network policies (might not exist in this Calico version): %v", err)
		// Don't fail if global policies don't exist
	} else {
		for _, policy := range globalPolicies.Items {
			name := policy.GetName()
			// Generate chain names for global policy
			inboundChain := "cali-pi-" + name
			outboundChain := "cali-po-" + name
			
			cw.policyCache[inboundChain] = name
			cw.policyCache[outboundChain] = name
			glog.V(2).Infof("Storing global policy %s against chain names %s, %s", name, inboundChain, outboundChain)
		}
	}

	return nil
}

func (cw *calicoWatcher) syncWorkloadEndpoints() error {
	cw.workloadCacheMtx.Lock()
	defer cw.workloadCacheMtx.Unlock()

	// Clear old endpoints
	cw.workloadCache = make(map[string]*WorkloadEndpoint)

	// List workload endpoints
	endpoints, err := cw.client.Resource(workloadEndpointGVR).List(cw.ctx, metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Error listing workload endpoints (might not exist in this Calico version): %v", err)
		return nil // Don't fail if workload endpoints don't exist
	}

	for _, endpoint := range endpoints.Items {
		we := cw.parseWorkloadEndpoint(&endpoint)
		if we != nil && we.Node == cw.node {
			key := we.Namespace + "/" + we.Name
			cw.workloadCache[key] = we
			glog.V(2).Infof("Adding workload %s", key)
		}
	}

	return nil
}

func (cw *calicoWatcher) parseWorkloadEndpoint(obj *unstructured.Unstructured) *WorkloadEndpoint {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil
	}

	node, _, _ := unstructured.NestedString(spec, "node")
	
	// Try to get IP from different possible locations
	var ip string
	
	// Try spec.ipNetworks first
	ipNetworks, found, _ := unstructured.NestedSlice(spec, "ipNetworks")
	if found && len(ipNetworks) > 0 {
		if ipNetwork, ok := ipNetworks[0].(string); ok {
			// Extract IP from CIDR notation if needed
			if strings.Contains(ipNetwork, "/") {
				ip = strings.Split(ipNetwork, "/")[0]
			} else {
				ip = ipNetwork
			}
		}
	}
	
	// Try spec.interfaceName or other fields if IP not found
	if ip == "" {
		// Could try other fields here if needed
		ip = "unknown"
	}

	return &WorkloadEndpoint{
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
		Node:      node,
		IP:        ip,
		Labels:    obj.GetLabels(),
	}
}

// getNodeName is intended to mimic the behaviour of calico node in determineNodeName()
func getNodeName() string {
	if nodeName, ok := os.LookupEnv("NODENAME"); ok {
		glog.V(2).Infof("Using NODENAME environment variable: %s", nodeName)
		return nodeName
	} else if nodeName := strings.ToLower(strings.TrimSpace(os.Getenv("HOSTNAME"))); nodeName != "" {
		glog.V(2).Infof("Using HOSTNAME environment variable as node name: %s", nodeName)
		return nodeName
	} else if nodeName, err := os.Hostname(); err == nil {
		nodeName = strings.ToLower(strings.TrimSpace(nodeName))
		glog.V(2).Infof("Using os.Hostname() as node name: %s", nodeName)
		return nodeName
	} else {
		glog.Fatalf("Error getting hostname: %v", err)
		// unreachable...
		return ""
	}
}
