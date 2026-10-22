package api

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// mirrorPodAnnotation holds, in the mirror pod the kubelet creates for a
// static pod, the UID of the static pod. It is the UID the kubelet gives to
// the runtime, so it is the one NRI reports.
const mirrorPodAnnotation = "kubernetes.io/config.mirror"

// PodUID returns the UID the kubelet and the runtime use for the pod. It is
// the pod UID, except for mirror pods where it is the UID of the static pod.
func PodUID(pod *v1.Pod) types.UID {
	if uid, ok := pod.Annotations[mirrorPodAnnotation]; ok && uid != "" {
		return types.UID(uid)
	}
	return pod.UID
}

// NewPodInfo creates a PodInfo object from a Pod and its corresponding Namespace and Node labels.
// This helper is useful for populators that have access to the full Kubernetes objects since
// contain all the necessary information for Network Policies selectors
func NewPodInfo(pod *v1.Pod, nsLabels map[string]string, nodeLabels map[string]string, clusterID string) *PodInfo {
	if pod == nil {
		return nil
	}

	containerPorts := make([]*ContainerPort, 0)
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			containerPorts = append(containerPorts, &ContainerPort{
				Name:     port.Name,
				Port:     port.ContainerPort,
				Protocol: string(port.Protocol),
			})
		}
	}

	return &PodInfo{
		Name:           pod.Name,
		Labels:         pod.Labels,
		ContainerPorts: containerPorts,
		Namespace: &Namespace{
			Name:   pod.Namespace,
			Labels: nsLabels,
		},
		Node: &Node{
			Name:   pod.Spec.NodeName,
			Labels: nodeLabels,
		},
		ClusterId:   clusterID,
		LastUpdated: time.Now().Unix(), // TODO: maybe get it from the managedFields metadata
		Uid:         string(PodUID(pod)),
	}
}

// SamePod returns true if both records belong to the same Pod. Records
// written before the UID existed are compared by namespace and name.
func SamePod(a, b *PodInfo) bool {
	if a.GetUid() != "" && b.GetUid() != "" {
		return a.GetUid() == b.GetUid()
	}
	return a.GetNamespace().GetName() == b.GetNamespace().GetName() && a.GetName() == b.GetName()
}
