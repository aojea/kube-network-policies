// SPDX-License-Identifier: APACHE-2.0

package networkpolicy_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	logsjson "k8s.io/component-base/logs/json"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	"sigs.k8s.io/kube-network-policies/pkg/api"
	"sigs.k8s.io/kube-network-policies/pkg/network"
	"sigs.k8s.io/kube-network-policies/pkg/networkpolicy"
	pluginsnpav1alpha2 "sigs.k8s.io/kube-network-policies/plugins/npa-v1alpha2"
	npav1alpha2 "sigs.k8s.io/network-policy-api/apis/v1alpha2"
	npaclientfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
	npainformers "sigs.k8s.io/network-policy-api/pkg/client/informers/externalversions"
)

type countingWriter struct {
	bytes int64
	lines int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	w.lines += int64(bytes.Count(p, []byte{'\n'}))
	return len(p), nil
}

func (w *countingWriter) Reset() {
	w.bytes = 0
	w.lines = 0
}

type staticPodInfoProvider struct {
	pods map[string]*api.PodInfo
}

func (s *staticPodInfoProvider) GetPodInfoByIP(podIP string) (*api.PodInfo, bool) {
	p, ok := s.pods[podIP]
	return p, ok
}

func configureBenchmarkLogger(b *testing.B, verbosity int, vmodule string, format string, w *countingWriter) context.Context {
	b.Helper()
	w.Reset()

	if format == "json" {
		klog.LogToStderr(false)
		logger, _ := logsjson.NewJSONLogger(logsapi.VerbosityLevel(verbosity), logsjson.AddNopSync(w), nil, nil)
		klog.SetLoggerWithOptions(logger, klog.ContextualLogger(true))
		return klog.NewContext(context.Background(), logger)
	}

	if vmodule != "" {
		tcfg := textlogger.NewConfig(
			textlogger.Verbosity(verbosity),
			textlogger.Output(w),
		)
		if err := tcfg.VModule().Set(vmodule); err != nil {
			b.Fatalf("failed to set vmodule: %v", err)
		}
		logger := textlogger.NewLogger(tcfg)
		klog.SetLoggerWithOptions(logger, klog.ContextualLogger(true))
		return klog.NewContext(context.Background(), logger)
	}

	klog.ClearLogger()
	klog.LogToStderr(false)
	klog.SetOutput(w)
	if _, err := logs.GlogSetter(fmt.Sprintf("%d", verbosity)); err != nil {
		b.Fatalf("failed to set verbosity: %v", err)
	}
	return klog.NewContext(context.Background(), klog.Background())
}

func buildBenchmarkEnv(b *testing.B, includeLoggingPolicy bool, denyIngress bool) (*networkpolicy.PolicyEngine, *network.Packet) {
	b.Helper()

	srcPodObj := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "frontend-pod",
			Namespace: "ns-a",
			Labels:    map[string]string{"app": "frontend", "role": "client"},
		},
		Spec: v1.PodSpec{NodeName: "node-1"},
		Status: v1.PodStatus{
			PodIP:  "10.0.1.10",
			PodIPs: []v1.PodIP{{IP: "10.0.1.10"}},
		},
	}
	dstPodObj := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend-pod",
			Namespace: "ns-b",
			Labels:    map[string]string{"app": "backend", "role": "server"},
		},
		Spec: v1.PodSpec{NodeName: "node-1"},
		Status: v1.PodStatus{
			PodIP:  "10.0.2.20",
			PodIPs: []v1.PodIP{{IP: "10.0.2.20"}},
		},
	}
	nsA := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns-a",
			Labels: map[string]string{"kubernetes.io/metadata.name": "ns-a", "team": "a"},
		},
	}
	nsB := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns-b",
			Labels: map[string]string{"kubernetes.io/metadata.name": "ns-b", "team": "b"},
		},
	}
	node1 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"kubernetes.io/hostname": "node-1"},
		},
	}

	kubeClient := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	nsInformer := informerFactory.Core().V1().Namespaces()
	podInformer := informerFactory.Core().V1().Pods()
	nodeInformer := informerFactory.Core().V1().Nodes()
	npInformer := informerFactory.Networking().V1().NetworkPolicies()

	_ = nsInformer.Informer().GetStore().Add(nsA)
	_ = nsInformer.Informer().GetStore().Add(nsB)
	_ = podInformer.Informer().GetStore().Add(srcPodObj)
	_ = podInformer.Informer().GetStore().Add(dstPodObj)
	_ = nodeInformer.Informer().GetStore().Add(node1)

	protoTCP := v1.ProtocolTCP
	port8080 := intstr.FromInt32(8080)

	// Egress policy on frontend-pod: allow TCP 8080 to backend-pod in ns-b
	egressNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-frontend-egress", Namespace: "ns-a"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protoTCP, Port: &port8080}},
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
							PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
						},
					},
				},
			},
		},
	}
	_ = npInformer.Informer().GetStore().Add(egressNP)

	// Ingress policy on backend-pod: either allow from team=a or deny (only allow from team=other)
	allowedTeam := "a"
	if denyIngress {
		allowedTeam = "other"
	}
	ingressNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "backend-ingress", Namespace: "ns-b"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protoTCP, Port: &port8080}},
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": allowedTeam}},
							PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
						},
					},
				},
			},
		},
	}
	_ = npInformer.Informer().GetStore().Add(ingressNP)

	npaClient := npaclientfake.NewSimpleClientset()
	npaFactory := npainformers.NewSharedInformerFactory(npaClient, 0)
	cnpInformer := npaFactory.Policy().V1alpha2().ClusterNetworkPolicies()

	// Admin CNP that passes traffic for ns-a/ns-b so evaluation traverses Admin -> Standard -> Baseline
	adminCNP := &npav1alpha2.ClusterNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-pass"},
		Spec: npav1alpha2.ClusterNetworkPolicySpec{
			Tier:     npav1alpha2.AdminTier,
			Priority: 50,
			Subject: npav1alpha2.ClusterNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
			Egress: []npav1alpha2.ClusterNetworkPolicyEgressRule{
				{
					Action: npav1alpha2.ClusterNetworkPolicyRuleActionPass,
					To:     []npav1alpha2.ClusterNetworkPolicyEgressPeer{{Namespaces: &metav1.LabelSelector{}}},
				},
			},
			Ingress: []npav1alpha2.ClusterNetworkPolicyIngressRule{
				{
					Action: npav1alpha2.ClusterNetworkPolicyRuleActionPass,
					From:   []npav1alpha2.ClusterNetworkPolicyIngressPeer{{Namespaces: &metav1.LabelSelector{}}},
				},
			},
		},
	}
	_ = cnpInformer.Informer().GetStore().Add(adminCNP)

	podInfoProvider := &staticPodInfoProvider{
		pods: map[string]*api.PodInfo{
			"10.0.1.10": api.NewPodInfo(srcPodObj, nsA.Labels, node1.Labels, ""),
			"10.0.2.20": api.NewPodInfo(dstPodObj, nsB.Labels, node1.Labels, ""),
		},
	}

	var evaluators []api.PolicyEvaluator
	if includeLoggingPolicy {
		evaluators = append(evaluators, networkpolicy.NewLoggingPolicy())
	}
	evaluators = append(evaluators,
		pluginsnpav1alpha2.NewClusterNetworkPolicy(npav1alpha2.AdminTier, cnpInformer, nil, nodeInformer.Lister()),
		networkpolicy.NewStandardNetworkPolicy("node-1", nsInformer, podInformer, npInformer),
		pluginsnpav1alpha2.NewClusterNetworkPolicy(npav1alpha2.BaselineTier, cnpInformer, nil, nodeInformer.Lister()),
	)

	engine := networkpolicy.NewPolicyEngine(podInfoProvider, evaluators)
	pkt := &network.Packet{
		ID:      42,
		Family:  v1.IPv4Protocol,
		SrcIP:   net.IPv4(10, 0, 1, 10),
		DstIP:   net.IPv4(10, 0, 2, 20),
		Proto:   v1.ProtocolTCP,
		SrcPort: 40243,
		DstPort: 8080,
		Payload: []byte{0x00, 0x00, 0xa0, 0x02, 0xfa, 0xf0, 0x19, 0x33, 0x00, 0x00, 0x02, 0x04, 0x05, 0xb4, 0x04, 0x02},
	}
	return engine, pkt
}

func BenchmarkEvaluatePacket(b *testing.B) {
	configs := []struct {
		name      string
		verbosity int
		vmodule   string
		format    string
	}{
		{name: "V0_Text", verbosity: 0, format: "text"},
		{name: "V0_VModuleEngine2_Text", verbosity: 0, vmodule: "engine=2", format: "text"},
		{name: "V0_VModuleEngine4_Text", verbosity: 0, vmodule: "engine=4", format: "text"},
		{name: "V2_Text", verbosity: 2, format: "text"},
		{name: "V2_JSON", verbosity: 2, format: "json"},
		{name: "V4_Text", verbosity: 4, format: "text"},
		{name: "V4_JSON", verbosity: 4, format: "json"},
	}

	scenarios := []struct {
		name        string
		denyIngress bool
		wantAllow   bool
	}{
		{name: "AcceptedPacket", denyIngress: false, wantAllow: true},
		{name: "DeniedPacket", denyIngress: true, wantAllow: false},
	}

	for _, sc := range scenarios {
		b.Run(sc.name, func(b *testing.B) {
			for _, cfg := range configs {
				b.Run(cfg.name, func(b *testing.B) {
					cw := &countingWriter{}
					ctx := configureBenchmarkLogger(b, cfg.verbosity, cfg.vmodule, cfg.format, cw)
					engine, pkt := buildBenchmarkEnv(b, false, sc.denyIngress)

					cw.Reset()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						allow, err := engine.EvaluatePacket(ctx, pkt)
						if err != nil || allow != sc.wantAllow {
							b.Fatalf("unexpected result: allow=%v, err=%v", allow, err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(cw.lines)/float64(b.N), "lines/pkt")
					b.ReportMetric(float64(cw.bytes)/float64(b.N), "log-B/pkt")
				})
			}
		})
	}
}
