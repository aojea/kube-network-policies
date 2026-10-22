package api

import (
	"context"
	"net"
	"net/netip"

	"sigs.k8s.io/kube-network-policies/pkg/network"
)

// PodInfoProvider defines an interface for components that can provide PodInfo.
type PodInfoProvider interface {
	GetPodInfoByIP(podIP string) (*PodInfo, bool)
}

// DomainResolver provides an interface for resolving domain names to IP addresses.
type DomainResolver interface {
	ContainsIP(domain string, ip net.IP) bool
}

// SyncFunc is a callback function that an evaluator can invoke to trigger
// a dataplane reconciliation.
type SyncFunc func()

// LocalPodIPsProvider exposes the IPs of the pods on the node as the container
// runtime assigns them. The runtime assigns them when it creates the sandbox,
// before the containers start and before the apiserver shows them.
type LocalPodIPsProvider interface {
	// LocalPodIPs returns the IPs of the pods on the node with the pod that
	// owns each one.
	LocalPodIPs() map[string]*PodInfo
	// OnNewPodIPs registers a function called with the IPs of a new pod before
	// its containers start. The function must be fast, the runtime waits for it.
	OnNewPodIPs(fn func(ips []netip.Addr))
}

// Verdict represents the outcome of a packet evaluation.
type Verdict int

const (
	// VerdictAccept allows the packet. In a directional pipeline, this means
	// the packet is allowed for that stage.
	VerdictAccept Verdict = iota
	// VerdictDeny denies the packet. This is a final decision for that direction.
	VerdictDeny
	// VerdictNext continues to the next evaluator in the pipeline.
	VerdictNext
)

// PolicyEvaluator is the complete interface for a policy plugin.
// It is responsible for both evaluating packets against its policies and
// providing the necessary configuration to the dataplane.
type PolicyEvaluator interface {
	// Name returns the identifier for this evaluator.
	Name() string
	// Ready returns true if the evaluator is initialized and ready to work.
	Ready() bool

	// EvaluateIngress performs runtime packet evaluation on the path into the
	// Pod.
	//
	// srcPod can be nil if the source of the network traffic is not a
	// Kubernetes pod (e.g. external traffic).
	EvaluateIngress(ctx context.Context, p *network.Packet, srcPod, dstPod *PodInfo) (Verdict, error)

	// EvaluateEgress  performs runtime packet evaluation on the path coming out
	// from the Pod.
	//
	// dstPod can be nil if the destination of the network traffic is not a
	// Kubernetes pod (e.g., external traffic).
	EvaluateEgress(ctx context.Context, p *network.Packet, srcPod, dstPod *PodInfo) (Verdict, error)

	// SetDataplaneSyncCallback allows the dataplane to provide a callback function.
	// The evaluator MUST call this function whenever its state changes in a way
	// that requires the dataplane rules to be re-synced.
	SetDataplaneSyncCallback(syncFn SyncFunc)

	// ManagedIPs returns the set of Pod IPs that this policy evaluator manages.
	// The dataplane uses this to build optimized nftables sets.
	// It can also return 'divertAll = true' to signal that all traffic
	// must be sent to the nfqueue, disabling the IP set optimization.
	ManagedIPs(ctx context.Context) (ips []netip.Addr, divertAll bool, err error)
}
