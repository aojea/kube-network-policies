//go:build !linux

package nri

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/types"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"sigs.k8s.io/kube-network-policies/pkg/api"
	"sigs.k8s.io/kube-network-policies/pkg/podinfo"
)

var _ podinfo.IPResolver = &NRIResolver{}
var _ api.PodInfoProvider = &NRIResolver{}

// NRIResolver is a stub implementation of the NRI-based resolver for non-Linux platforms.
type NRIResolver struct{}

// NewNRIResolver returns an error on non-Linux platforms.
func NewNRIResolver(ctx context.Context, nodeName string, namespaceInformer coreinformers.NamespaceInformer) (*NRIResolver, error) {
	return nil, errors.New("NRI resolver is only supported on Linux")
}

// LookupPod always returns false on non-Linux platforms.
func (p *NRIResolver) LookupPod(ip string) (string, types.UID, bool) {
	return "", "", false
}

// GetPodInfoByIP always returns false on non-Linux platforms.
func (p *NRIResolver) GetPodInfoByIP(ip string) (*api.PodInfo, bool) {
	return nil, false
}
