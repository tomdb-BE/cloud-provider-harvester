package ccm

import (
	"context"
	"fmt"
	"hash/crc32"
	"strconv"
	"time"

	lbv1 "github.com/harvester/harvester-load-balancer/pkg/apis/loadbalancer.harvesterhci.io/v1alpha1"
	ctllb "github.com/harvester/harvester-load-balancer/pkg/generated/controllers/loadbalancer.harvesterhci.io"
	ctllbv1 "github.com/harvester/harvester-load-balancer/pkg/generated/controllers/loadbalancer.harvesterhci.io/v1alpha1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	defaultWaitIPTimeout = time.Second * 5
	uuidKey              = prefix + "service-uuid"
	clusterNameKey       = prefix + "cluster"
)

type LoadBalancerManager struct {
	lbClient  ctllbv1.LoadBalancerClient
	namespace string
}

func newLoadBalancerManager(cfg *rest.Config, namespace string) (cloudprovider.LoadBalancer, error) {
	lbFactory, err := ctllb.NewFactoryFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &LoadBalancerManager{
		lbClient:  lbFactory.Loadbalancer().V1alpha1().LoadBalancer(),
		namespace: namespace,
	}, nil
}

func (l *LoadBalancerManager) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	name := l.GetLoadBalancerName(ctx, clusterName, service)
	lb, err := l.lbClient.Get(l.namespace, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		} else {
			return nil, false, err
		}
	}

	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{
				IP: lb.Status.Address,
			},
		},
	}, true, nil
}

func (l *LoadBalancerManager) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	name := clusterName + "-" + service.Namespace + "-" + service.Name + "-"

	digest := crc32.ChecksumIEEE([]byte(name + string(service.UID)))
	suffix := fmt.Sprintf("%8x", digest)
	name += suffix

	// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-subdomain-names
	// The name contains no more than 253 characters
	if len(name) > 253 {
		name = name[:253]
	}

	return name
}

// EnsureLoadBalancer is to create/update a Harvester load balancer for the service and return the loadBalancerStatus with an IP
// 1. watch the LB to get an IP asynchronously
// 2. create or update lb
// 3. wait for an ip and return the LoadBalancerStatus
func (l *LoadBalancerManager) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	name := l.GetLoadBalancerName(ctx, clusterName, service)
	ipChan := make(chan string)

	lb, getErr := l.lbClient.Get(l.namespace, name, metav1.GetOptions{})
	if getErr != nil && !errors.IsNotFound(getErr) {
		return nil, getErr
	}

	// watch the lb to get an ip
	w, err := l.lbClient.Watch(l.namespace, metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", name)})
	if err != nil {
		return nil, fmt.Errorf("watch loadbalancer in namespace %s error, %w", l.namespace, err)
	}
	defer w.Stop()
	go getIP(w, ipChan, lb)

	// create or update lb
	if getErr == nil {
		if err := l.updateLoadBalancer(lb, service, nodes); err != nil {
			return nil, err
		}
	} else {
		spec, err := getLBSpec(service, nodes)
		if err != nil {
			return nil, err
		}
		if _, err := l.lbClient.Create(&lbv1.LoadBalancer{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: l.namespace,
				Name:      name,
				Annotations: map[string]string{
					uuidKey: string(service.UID),
				},
				Labels: map[string]string{
					clusterNameKey: clusterName,
				},
			},
			Spec: *spec,
		}); err != nil {
			return nil, err
		}
	}

	// wait Kube-vip to allocate an ip
	select {
	case <-time.After(defaultWaitIPTimeout):
		return nil, fmt.Errorf("wait ip timeout")
	case ip := <-ipChan:
		return &v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{
					IP: ip,
				},
			},
		}, nil
	}
}

func (l *LoadBalancerManager) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	name := l.GetLoadBalancerName(ctx, clusterName, service)
	lb, err := l.lbClient.Get(l.namespace, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	return l.updateLoadBalancer(lb, service, nodes)
}

func (l *LoadBalancerManager) updateLoadBalancer(lb *lbv1.LoadBalancer, service *v1.Service, nodes []*v1.Node) error {
	lbCopy := lb.DeepCopy()
	spec, err := getLBSpec(service, nodes)
	if err != nil {
		return err
	}
	lbCopy.Spec = *spec

	if _, err := l.lbClient.Update(lbCopy); err != nil {
		return err
	}

	return nil
}

func (l *LoadBalancerManager) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	name := l.GetLoadBalancerName(ctx, clusterName, service)
	_, err := l.lbClient.Get(l.namespace, name, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if err == nil {
		return l.lbClient.Delete(l.namespace, name, &metav1.DeleteOptions{})
	}

	return nil
}

// getIP by watching the loadbalancers
func getIP(w watch.Interface, ipChan chan string, lbBeforeEnsure *lbv1.LoadBalancer) {
	// if the lb has a ip before ensuring, return it directly
	if lbBeforeEnsure.Status.Address != "" {
		ipChan <- lbBeforeEnsure.Status.Address
		return
	}

	for event := range w.ResultChan() {
		if event.Type != watch.Added && event.Type != watch.Modified {
			continue
		}
		lb, ok := event.Object.(*lbv1.LoadBalancer)
		if !ok {
			klog.Errorf("type assert failed")
			return
		}

		if lb.Status.Address != "" {
			ipChan <- lb.Status.Address
			return
		}
	}
}

func getLBSpec(service *v1.Service, nodes []*v1.Node) (*lbv1.LoadBalancerSpec, error) {
	// ipam
	ipam := lbv1.Pool
	if ipamStr, ok := service.Annotations[loadBalancerIPAM]; ok {
		ipam = lbv1.IPAM(ipamStr)
	}

	// listeners
	listeners := []*lbv1.Listener{}
	for _, port := range service.Spec.Ports {
		listeners = append(listeners, &lbv1.Listener{
			Name:        port.Name,
			Port:        port.Port,
			Protocol:    port.Protocol,
			BackendPort: port.NodePort,
		})
	}

	// backendServers
	backendServers := []string{}
	for _, node := range nodes {
		for _, address := range node.Status.Addresses {
			if address.Type == v1.NodeInternalIP {
				backendServers = append(backendServers, address.Address)
			}
		}
	}

	// healthCheck
	healthCheck, err := extractHealthCheck(service.Annotations)
	if err != nil {
		return nil, fmt.Errorf("extract health check failed, error: %w", err)
	}

	return &lbv1.LoadBalancerSpec{
		Description:    service.Annotations[loadBalancerDescription],
		IPAM:           ipam,
		Listeners:      listeners,
		BackendServers: backendServers,
		HeathCheck:     healthCheck,
	}, nil
}

func extractHealthCheck(annotations map[string]string) (*lbv1.HeathCheck, error) {
	healthCheck := &lbv1.HeathCheck{}
	var err error

	// port
	portStr, ok := annotations[healthCheckPort]
	if !ok {
		return nil, nil
	} else {
		if healthCheck.Port, err = strconv.Atoi(portStr); err != nil {
			return nil, fmt.Errorf("atoi error, port: %s, error: %w", portStr, err)
		}
	}

	// successThreshold
	if healthCheck.SuccessThreshold, err = getAnnotationValue(annotations, healthCheckSuccessThreshold); err != nil {
		return nil, fmt.Errorf("get annotationsValue failed, key: %s, err: %w", healthCheckSuccessThreshold, err)
	}

	// failThreshold
	if healthCheck.FailureThreshold, err = getAnnotationValue(annotations, healthCheckFailureThreshold); err != nil {
		return nil, fmt.Errorf("get annotationsValue failed, key: %s, err: %w", healthCheckFailureThreshold, err)
	}

	// periodSeconds
	if healthCheck.PeriodSeconds, err = getAnnotationValue(annotations, healthCheckPeriodSeconds); err != nil {
		return nil, fmt.Errorf("get annotationsValue failed, key: %s, err: %w", healthCheckPeriodSeconds, err)
	}

	// timeout
	if healthCheck.TimeoutSeconds, err = getAnnotationValue(annotations, healthCheckTimeoutSeconds); err != nil {
		return nil, fmt.Errorf("get annotationsValue failed, key: %s, err: %w", healthCheckTimeoutSeconds, err)
	}

	return healthCheck, nil
}

func getAnnotationValue(annotations map[string]string, key string) (int, error) {
	valueStr, ok := annotations[key]
	if !ok {
		return defaultValue(key), nil
	}

	return strconv.Atoi(valueStr)
}

func defaultValue(key string) int {
	var value int

	switch key {
	case healthCheckSuccessThreshold:
		value = defaultSuccessThreshold
	case healthCheckFailureThreshold:
		value = defaultFailThreshold
	case healthCheckPeriodSeconds:
		value = defaultPeriodSeconds
	case healthCheckTimeoutSeconds:
		value = defaultTimeoutSeconds
	}

	return value
}
