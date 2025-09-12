package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	// --- AWS Imports ---
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	// --- Google Cloud Imports ---
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gke "google.golang.org/api/container/v1"
	"google.golang.org/api/option"

	// --- Kubernetes Imports ---
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"

	"sigs.k8s.io/kube-network-policies/pkg/api"
	"sigs.k8s.io/kube-network-policies/pkg/ipcache"
)

// --- Command-line flags for discovery configuration ---
var (
	listenAddr        = flag.String("listen-address", "http://0.0.0.0:10999", "The address for the cache server to listen on.")
	etcdDir           = flag.String("etcd-dir", "./ipcache.etcd", "The directory for the embedded etcd server.")
	caFile            = flag.String("tls-ca-file", "", "The CA file for the server.")
	certFile          = flag.String("tls-cert-file", "", "The certificate file for the server.")
	keyFile           = flag.String("tls-key-file", "", "The key file for the server.")
	reconcileInterval = flag.Duration("reconcile-interval", 5*time.Minute, "How often to run the cluster discovery reconcile loop.")
	gcpClusterNames   = flag.String("gcp-cluster-names", "", "Comma-separated list of GKE clusters to watch, in 'project/location/cluster' format.")
	awsClusterNames   = flag.String("aws-cluster-names", "", "Comma-separated list of EKS clusters to watch, in 'region/cluster' format.")

	// Manages the lifecycle of each running cluster watcher.
	// map[clusterIdentifier]context.CancelFunc
	runningWatchers = &sync.Map{}
)

// DiscoveredCluster holds the dynamically discovered details of a cluster.
type DiscoveredCluster struct {
	// Unique identifier, e.g., "gke_project_location_cluster"
	Identifier  string
	Name        string
	Provider    string // "gke", "eks"
	Endpoint    string
	CAData      []byte
	ClusterName string // Provider-specific cluster name
	Region      string // EKS specific
}

// --- EKS Authentication ---
type eksTokenSource struct {
	stsClient   *sts.PresignClient
	clusterName string
}

func (s *eksTokenSource) Token() (*oauth2.Token, error) {
	presignedURL, err := s.stsClient.PresignGetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{}, func(options *sts.PresignOptions) {
		options.ClientOptions = append(options.ClientOptions, func(o *sts.Options) {
			o.APIOptions = append(o.APIOptions, smithyhttp.SetHeaderValue("x-k8s-aws-id", s.clusterName))
		})
	})
	if err != nil {
		return nil, fmt.Errorf("failed to presign EKS token: %w", err)
	}
	tokenString := "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presignedURL.URL))
	return &oauth2.Token{
		AccessToken: tokenString,
		Expiry:      time.Now().Add(14 * time.Minute),
	}, nil
}

// Struct to hold parsed cluster configs for cleaner processing
type clusterConfig struct {
	gkeClusters map[string]bool // key: "project/location/cluster"
	eksClusters map[string]bool // key: "region/cluster"
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	// --- Validate and parse cluster configuration flags ---
	if *gcpClusterNames == "" && *awsClusterNames == "" {
		klog.Fatal("At least one of --gcp-cluster-names or --aws-cluster-names must be provided.")
	}

	config, err := parseAndValidateClusterFlags()
	if err != nil {
		klog.Fatalf("Invalid cluster configuration: %v", err)
	}
	// --- End validation ---

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var opts []ipcache.EtcdOption
	if *caFile != "" && *certFile != "" && *keyFile != "" {
		opts = append(opts, ipcache.WithTLS(*certFile, *keyFile, *caFile))
	}

	if err := os.MkdirAll(*etcdDir, 0750); err != nil {
		klog.Fatalf("Failed to create etcd directory: %v", err)
	}

	cacheServer, err := ipcache.NewEtcdStore(*listenAddr, *etcdDir, opts...)
	if err != nil {
		klog.Fatalf("Failed to create ipcache server: %v", err)
	}
	defer cacheServer.Close()

	ticker := time.NewTicker(*reconcileInterval)
	defer ticker.Stop()

	// Run the first reconciliation immediately, then on the ticker.
	reconcileClusters(ctx, cacheServer, config)
	for {
		select {
		case <-ticker.C:
			reconcileClusters(ctx, cacheServer, config)
		case <-ctx.Done():
			klog.Infoln("Shutting down.")
			return
		}
	}
}

// parseAndValidateClusterFlags ensures the provided cluster flags are in the correct format.
func parseAndValidateClusterFlags() (*clusterConfig, error) {
	config := &clusterConfig{
		gkeClusters: make(map[string]bool),
		eksClusters: make(map[string]bool),
	}

	if *gcpClusterNames != "" {
		clusters := strings.Split(*gcpClusterNames, ",")
		for _, c := range clusters {
			parts := strings.Split(strings.TrimSpace(c), "/")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
				return nil, fmt.Errorf("invalid GKE cluster format: %q. Expected 'project/location/cluster'", c)
			}
			config.gkeClusters[c] = true
		}
	}

	if *awsClusterNames != "" {
		clusters := strings.Split(*awsClusterNames, ",")
		for _, c := range clusters {
			parts := strings.Split(strings.TrimSpace(c), "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("invalid EKS cluster format: %q. Expected 'region/cluster'", c)
			}
			config.eksClusters[c] = true
		}
	}

	return config, nil
}

// reconcileClusters is the main loop that discovers and manages watchers.
func reconcileClusters(ctx context.Context, server *ipcache.EtcdStore, config *clusterConfig) {
	klog.Info("Starting cluster reconciliation loop...")
	discoveredClusters := make(map[string]DiscoveredCluster)

	// --- Discover clusters from all configured cloud providers ---
	if len(config.gkeClusters) > 0 {
		gkeClusters, err := fetchGKEClusters(ctx, config.gkeClusters)
		if err != nil {
			klog.Errorf("Error fetching GKE cluster details: %v", err)
		}
		for _, c := range gkeClusters {
			discoveredClusters[c.Identifier] = c
		}
	}
	if len(config.eksClusters) > 0 {
		eksClusters, err := fetchEKSClusters(ctx, config.eksClusters)
		if err != nil {
			klog.Errorf("Error fetching EKS cluster details: %v", err)
		}
		for _, c := range eksClusters {
			discoveredClusters[c.Identifier] = c
		}
	}

	klog.Infof("Successfully fetched details for %d clusters.", len(discoveredClusters))
	currentWatchers := make(map[string]bool)
	runningWatchers.Range(func(key, value interface{}) bool {
		currentWatchers[key.(string)] = true
		return true
	})

	// --- Start watchers for new clusters ---
	for id, cluster := range discoveredClusters {
		if _, exists := currentWatchers[id]; !exists {
			klog.Infof("New cluster configured: %s. Starting watcher.", id)
			go startWatcherForCluster(ctx, cluster, server)
		}
	}

	// --- Stop watchers for removed clusters ---
	for id := range currentWatchers {
		if _, exists := discoveredClusters[id]; !exists {
			klog.Infof("Cluster no longer configured: %s. Stopping watcher.", id)
			if cancelFunc, ok := runningWatchers.Load(id); ok {
				cancelFunc.(context.CancelFunc)()
				runningWatchers.Delete(id)
			}
		}
	}
}

func startWatcherForCluster(ctx context.Context, cluster DiscoveredCluster, server *ipcache.EtcdStore) {
	clusterCtx, cancel := context.WithCancel(ctx)
	runningWatchers.Store(cluster.Identifier, cancel)
	defer func() {
		cancel()
		runningWatchers.Delete(cluster.Identifier)
	}()

	clientset, err := getClientsetForCluster(clusterCtx, cluster)
	if err != nil {
		klog.Errorf("Could not create clientset for cluster %s: %v", cluster.Identifier, err)
		return
	}

	ns, err := clientset.CoreV1().Namespaces().Get(clusterCtx, metav1.NamespaceSystem, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get kube-system namespace to determine cluster ID for %s: %v", cluster.Identifier, err)
		return
	}
	clusterID := string(ns.UID)
	klog.Infof("Successfully connected to cluster %s (UID: %s). Starting informers...", cluster.Identifier, clusterID)

	factory := informers.NewSharedInformerFactory(clientset, 0)
	podInformer := factory.Core().V1().Pods().Informer()
	nsInformer := factory.Core().V1().Namespaces().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()

	_, _ = podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updatePodInCache(server, nsInformer.GetStore(), nodeInformer.GetStore(), obj.(*v1.Pod), clusterID)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, newPod := oldObj.(*v1.Pod), newObj.(*v1.Pod)
			if !reflect.DeepEqual(oldPod.Status.PodIPs, newPod.Status.PodIPs) || !reflect.DeepEqual(oldPod.Labels, newPod.Labels) {
				updatePodInCache(server, nsInformer.GetStore(), nodeInformer.GetStore(), newPod, clusterID)
			}
		},
		DeleteFunc: func(obj interface{}) { deletePodFromCache(server, obj) },
	})

	_, _ = nsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNs, newNs := oldObj.(*v1.Namespace), newObj.(*v1.Namespace)
			if reflect.DeepEqual(oldNs.Labels, newNs.Labels) {
				return
			}
			pods, err := factory.Core().V1().Pods().Lister().Pods(newNs.Name).List(labels.Everything())
			if err != nil {
				klog.Errorf("Error listing pods in namespace %s for cluster %s: %v", newNs.Name, cluster.Name, err)
				return
			}
			for _, pod := range pods {
				updatePodInCache(server, nsInformer.GetStore(), nodeInformer.GetStore(), pod, clusterID)
			}
		},
	})

	_, _ = nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, newNode := oldObj.(*v1.Node), newObj.(*v1.Node)
			if reflect.DeepEqual(oldNode.Labels, newNode.Labels) {
				return
			}
			pods, err := factory.Core().V1().Pods().Lister().List(labels.Everything())
			if err != nil {
				klog.Errorf("Error listing all pods for cluster %s: %v", cluster.Name, err)
				return
			}
			for _, pod := range pods {
				if pod.Spec.NodeName == newNode.Name {
					updatePodInCache(server, nsInformer.GetStore(), nodeInformer.GetStore(), pod, clusterID)
				}
			}
		},
	})

	factory.Start(clusterCtx.Done())
	if !cache.WaitForCacheSync(clusterCtx.Done(), podInformer.HasSynced, nsInformer.HasSynced, nodeInformer.HasSynced) {
		klog.Errorf("Failed to sync cache for cluster %s", cluster.Identifier)
	}
	<-clusterCtx.Done()
	klog.Infof("Watcher for cluster %s has stopped.", cluster.Identifier)
}

func getClientsetForCluster(ctx context.Context, cluster DiscoveredCluster) (*kubernetes.Clientset, error) {
	var ts oauth2.TokenSource

	switch cluster.Provider {
	case "gke":
		ts = google.ComputeTokenSource("", "https://www.googleapis.com/auth/cloud-platform")
	case "eks":
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cluster.Region))
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config for region %s: %w", cluster.Region, err)
		}
		ts = &eksTokenSource{
			stsClient:   sts.NewPresignClient(sts.NewFromConfig(awsCfg)),
			clusterName: cluster.ClusterName,
		}
	default:
		return nil, fmt.Errorf("unsupported cluster provider: %s", cluster.Provider)
	}

	config := &rest.Config{
		Host:            cluster.Endpoint,
		TLSClientConfig: rest.TLSClientConfig{CAData: cluster.CAData},
		WrapTransport:   transport.ResettableTokenSourceWrapTransport(transport.NewCachedTokenSource(ts)),
	}
	return kubernetes.NewForConfig(config)
}

// --- Cloud Fetch Functions ---

func fetchGKEClusters(ctx context.Context, gkeClusters map[string]bool) ([]DiscoveredCluster, error) {
	klog.Infof("Fetching details for %d GKE clusters...", len(gkeClusters))
	var discovered []DiscoveredCluster
	ts := google.ComputeTokenSource("", "https://www.googleapis.com/auth/cloud-platform")
	httpClient := oauth2.NewClient(ctx, ts)
	gkeService, err := gke.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("failed to create GKE service client: %w", err)
	}

	for fqn := range gkeClusters {
		parts := strings.Split(fqn, "/")
		project, location, name := parts[0], parts[1], parts[2]

		fullName := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, location, name)
		cluster, err := gkeService.Projects.Locations.Clusters.Get(fullName).Context(ctx).Do()
		if err != nil {
			klog.Errorf("Failed to get GKE cluster %q: %v", fqn, err)
			continue
		}

		if cluster.Status != "RUNNING" {
			klog.Warningf("GKE cluster %q is not in RUNNING state (state is %s), skipping.", fqn, cluster.Status)
			continue
		}
		caData, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClusterCaCertificate)
		if err != nil {
			klog.Errorf("Failed to decode CA for GKE cluster %q: %v", fqn, err)
			continue
		}
		id := fmt.Sprintf("gke_%s_%s_%s", project, location, name)
		discovered = append(discovered, DiscoveredCluster{
			Identifier:  id,
			Name:        cluster.Name,
			Provider:    "gke",
			Endpoint:    "https://" + cluster.Endpoint,
			CAData:      caData,
			ClusterName: cluster.Name,
		})
	}
	return discovered, nil
}

func fetchEKSClusters(ctx context.Context, eksClusters map[string]bool) ([]DiscoveredCluster, error) {
	klog.Infof("Fetching details for %d EKS clusters...", len(eksClusters))
	var discovered []DiscoveredCluster

	// Group clusters by region to create clients efficiently
	clustersByRegion := make(map[string][]string)
	for fqn := range eksClusters {
		parts := strings.Split(fqn, "/")
		region, name := parts[0], parts[1]
		clustersByRegion[region] = append(clustersByRegion[region], name)
	}

	for region, names := range clustersByRegion {
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config for region %s: %w", region, err)
		}
		eksClient := eks.NewFromConfig(awsCfg)

		for _, clusterName := range names {
			fqn := fmt.Sprintf("%s/%s", region, clusterName)
			desc, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &clusterName})
			if err != nil {
				klog.Errorf("Failed to describe EKS cluster %q: %v", fqn, err)
				continue
			}
			if desc.Cluster.Status != "ACTIVE" {
				klog.Warningf("EKS cluster %q is not in ACTIVE state (state is %s), skipping.", fqn, desc.Cluster.Status)
				continue
			}
			caData, err := base64.StdEncoding.DecodeString(*desc.Cluster.CertificateAuthority.Data)
			if err != nil {
				klog.Errorf("Failed to decode CA for EKS cluster %q: %v", fqn, err)
				continue
			}
			id := fmt.Sprintf("eks_%s_%s", region, clusterName)
			discovered = append(discovered, DiscoveredCluster{
				Identifier:  id,
				Name:        clusterName,
				Provider:    "eks",
				Endpoint:    *desc.Cluster.Endpoint,
				CAData:      caData,
				ClusterName: clusterName,
				Region:      region,
			})
		}
	}
	return discovered, nil
}

// --- Cache Update/Delete Functions (Unchanged) ---
func updatePodInCache(server *ipcache.EtcdStore, nsStore cache.Store, nodeStore cache.Store, pod *v1.Pod, clusterID string) {
	if pod.Spec.HostNetwork || len(pod.Status.PodIPs) == 0 {
		return
	}
	var nodeLabels, nsLabels map[string]string
	nsObj, exists, err := nsStore.GetByKey(pod.Namespace)
	if err == nil && exists {
		nsLabels = nsObj.(*v1.Namespace).Labels
	}
	nodeObj, exists, err := nodeStore.GetByKey(pod.Spec.NodeName)
	if err == nil && exists {
		nodeLabels = nodeObj.(*v1.Node).Labels
	}
	podInfo := api.NewPodInfo(pod, nsLabels, nodeLabels, clusterID)
	for _, podIP := range pod.Status.PodIPs {
		if err := server.Upsert(podIP.IP, podInfo); err != nil {
			klog.Errorf("fail to update IP address %s: %v", podIP.IP, err)
		}
	}
}

func deletePodFromCache(server *ipcache.EtcdStore, obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("could not get object from tombstone %+v", obj)
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			klog.Errorf("tombstone contained object that is not a Pod %+v", obj)
			return
		}
	}
	for _, podIP := range pod.Status.PodIPs {
		if err := server.Delete(podIP.IP); err != nil {
			klog.Errorf("fail to delete IP address %s: %v", podIP.IP, err)
		}
	}
}
