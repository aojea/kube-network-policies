package podinfo

import (
	"cmp"
	"slices"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/kube-network-policies/pkg/api"
)

// IPResolver defines an interface for resolving an IP address to the pod that owns it.
type IPResolver interface {
	// LookupPod abstracts the logic of finding the pod that owns an IP.
	// It returns the pod key ("namespace/name"), its UID and true if found,
	// otherwise false. The UID tells the pod apart from an older pod with
	// the same name.
	LookupPod(ip string) (string, types.UID, bool)
}

// LocalResolver resolves the IPs of the pods on the node from the container
// runtime. The runtime assigns the IP when it creates the sandbox and
// releases it when it stops it, the apiserver shows it later and keeps it
// longer, so the local resolver decides which local pod owns an IP. It also
// knows the pod labels, so it provides the pod details when the apiserver
// does not have the pod yet.
type LocalResolver interface {
	IPResolver
	api.PodInfoProvider
}

// --- Informer-based Resolver ---

const (
	// PodIPIndex is the name of the indexer that maps pod IPs to pod objects.
	PodIPIndex = "podIPKeyIndex"
)

// InformerResolver is an implementation of Resolver that uses a pod informer index.
type InformerResolver struct {
	podIndexer cache.Indexer
}

// NewInformerResolver creates a new resolver that looks up pods in a Kubernetes informer.
func NewInformerResolver(podInformer cache.SharedIndexInformer) (*InformerResolver, error) {
	// Add the IP indexer to the pod informer.
	err := podInformer.AddIndexers(cache.Indexers{
		PodIPIndex: func(obj interface{}) ([]string, error) {
			pod, ok := obj.(*v1.Pod)
			// A terminal pod keeps its IPs in the status after the runtime
			// released them, so another pod may own them now.
			if !ok || pod.Spec.HostNetwork || isTerminalPod(pod) {
				return []string{}, nil
			}
			ips := make([]string, 0, len(pod.Status.PodIPs))
			for _, ip := range pod.Status.PodIPs {
				ips = append(ips, ip.IP)
			}
			return ips, nil
		},
	})
	if err != nil {
		return nil, err
	}

	return &InformerResolver{
		podIndexer: podInformer.GetIndexer(),
	}, nil
}

// isTerminalPod returns true if the pod is Succeeded or Failed.
func isTerminalPod(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed
}

// LookupPod implements the Resolver interface using the informer's index.
func (r *InformerResolver) LookupPod(ip string) (string, types.UID, bool) {
	objs, err := r.podIndexer.ByIndex(PodIPIndex, ip)
	if err != nil || len(objs) == 0 {
		return "", "", false
	}
	pods := make([]*v1.Pod, 0, len(objs))
	for _, obj := range objs {
		if pod, ok := obj.(*v1.Pod); ok {
			pods = append(pods, pod)
		}
	}
	if len(pods) == 0 {
		return "", "", false
	}
	// If multiple pods share the IP prefer the one that is not being deleted,
	// then the Running one. A pod being deleted still reports Running with its
	// IPs after its sandbox is gone, and by then another pod may own the IP.
	best := slices.MaxFunc(pods, func(a, b *v1.Pod) int {
		return cmp.Compare(ownershipRank(a), ownershipRank(b))
	})
	key, err := cache.MetaNamespaceKeyFunc(best)
	if err != nil {
		return "", "", false
	}
	return key, best.UID, true
}

// ownershipRank scores how likely a pod is to own the IPs in its status.
func ownershipRank(pod *v1.Pod) int {
	rank := 0
	if pod.DeletionTimestamp == nil {
		rank += 2
	}
	if pod.Status.Phase == v1.PodRunning {
		rank++
	}
	return rank
}
