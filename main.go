package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	crd "github.com/traefik/traefik/v2/pkg/provider/kubernetes/crd/generated/clientset/versioned"
	"github.com/traefik/traefik/v2/pkg/provider/kubernetes/crd/traefik/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	dns "sigs.k8s.io/external-dns/endpoint"
)

const (
	hostnameAnnotation         = "external-dns.alpha.kubernetes.io/internal-hostname"
	internalHostnameAnnotation = "external-dns.alpha.kubernetes.io/hostname"
)

var clientset *kubernetes.Clientset
var ctx context.Context = context.Background()

func main() {
	var config *rest.Config
	var err error

	// create a Kubernetes client
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("Failed to create Kubernetes config: %v", err)
		}
	}

	// create the clientset
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalln(err.Error())
	}

	traefikClient, err := crd.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	lbIPs, err := getLoadBalancerIP("app.kubernetes.io/instance=traefik-traefik")
	if err != nil {
		panic(err)
	}
	fmt.Printf("Detected load balancer IPs as %v\n", lbIPs)

	// create an informer to watch for changes in IngressRoute objects
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return traefikClient.TraefikV1alpha1().IngressRoutes("").List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return traefikClient.TraefikV1alpha1().IngressRoutes("").Watch(ctx, options)
			},
		},
		&v1alpha1.IngressRoute{},
		0, // don't resync
		cache.Indexers{},
	)

	// add an event handler for changes to IngressRoute objects
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ingressRoute := obj.(*v1alpha1.IngressRoute)
			fmt.Printf("IngressRoute %s/%s added\n", ingressRoute.Namespace, ingressRoute.Name)
			if h := hosToAdd(ingressRoute.Annotations); h != "" {
				fmt.Printf("Adding DNS entries for %s\n", h)
				createDNSRecord(h, ingressRoute.Namespace, lbIPs)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			ingressRoute := newObj.(*v1alpha1.IngressRoute)
			fmt.Printf("IngressRoute %s/%s updated\n", ingressRoute.Namespace, ingressRoute.Name)
		},
		DeleteFunc: func(obj interface{}) {
			ingressRoute := obj.(*v1alpha1.IngressRoute)
			if ingressRoute.Annotations["managed-by"] == "traefik2dns" {
				fmt.Printf("IngressRoute %s/%s deleted\n", ingressRoute.Namespace, ingressRoute.Name)
				if err := deleteDNSRecord(ingressRoute.Name, ingressRoute.Namespace); err != nil {
					fmt.Printf("Error deleting record: %v\n", err)
				}
			} else {
				fmt.Printf("IngressRoute %s/%s is not managed by traefik2dns. Ignored.\n", ingressRoute.Namespace, ingressRoute.Name)
			}
		},
	})

	// start the informer and wait for changes
	informer.Run(make(chan struct{}))
	for {
		time.Sleep(time.Second)
	}
}

func hosToAdd(anns map[string]string) string {
	if val, ok := anns[internalHostnameAnnotation]; ok {
		return val
	}
	if val, ok := anns[hostnameAnnotation]; ok {
		return val
	}
	return ""
}

func getLoadBalancerIP(labels string) ([]string, error) {
	var ips []string
	opts := metav1.ListOptions{
		LabelSelector: labels,
	}
	services, err := clientset.CoreV1().Services("").List(ctx, opts)
	if err != nil {
		return ips, err
	}

	for _, service := range services.Items {
		loadBalancerIPs := service.Status.LoadBalancer.Ingress
		for _, loadBalancerIP := range loadBalancerIPs {
			if loadBalancerIP.Hostname != "" {
				res, err := getIPs(loadBalancerIP.Hostname)
				if err != nil {
					return ips, err
				}
				ips = append(ips, res...)
			}
			if loadBalancerIP.IP != "" {
				fmt.Println(loadBalancerIP.IP)
				ips = append(ips, loadBalancerIP.IP)
			}
		}
	}
	return ips, err
}

func deleteDNSRecord(name string, namespace string) error {
	var err error

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err = clientset.CoreV1().RESTClient().
			Delete().
			AbsPath("/apis/externaldns.k8s.io/v1alpha1").
			Namespace(namespace).
			Name(name).
			Body(&metav1.DeleteOptions{}).
			Do().
			Error()
		if err != nil {
			return err
		}
		return nil
	})
	if retryErr != nil {
		return retryErr
	}

	return nil
}

func createDNSRecord(dnsName string, namespace string, lbIPs []string) error {
	dnsEndpoint := &dns.DNSEndpoint{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DNSEndpoint",
			APIVersion: "externaldns.k8s.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsName,
			Namespace: namespace,
			Annotations: map[string]string{
				"managed-by": "traefik2dns",
			},
			Labels: map[string]string{
				"managed-by": "traefik2dns",
			},
		},
		Spec: dns.DNSEndpointSpec{
			Endpoints: []*dns.Endpoint{
				{
					DNSName: dnsName,
					Targets: lbIPs,
				},
			},
		},
	}

	_, err := clientset.CoreV1().RESTClient().
		Post().
		AbsPath("/apis/externaldns.k8s.io/v1alpha1").
		Namespace(namespace).
		Body(dnsEndpoint).
		Resource("dnsendpoints").
		DoRaw(ctx)
	if err != nil {
		fmt.Println(err)
		if errors.IsAlreadyExists(err) {
			fmt.Println("DNS endpoint already exists")
		} else {
			panic(err.Error())
		}
	}
	return err
}

func getIPs(hostname string) ([]string, error) {
	// Resolve the hostname into a list of IPs
	ips, err := net.LookupHost(hostname)
	if err != nil {
		fmt.Println("Error:", err)
		return ips, err
	}
	return ips, err
}
