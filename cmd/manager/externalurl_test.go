package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// urlManager is a Manager with just enough wired up to resolve an address.
func urlManager(cs *fake.Clientset, cfg Config) *Manager {
	return &Manager{
		Config:    cfg,
		K8sClient: cs,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// lbService is plex-main as a LoadBalancer that has been given an address.
func lbService(ingress corev1.LoadBalancerIngress) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "plex-main", Namespace: "media"},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "pms", Port: 32400}},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{ingress}},
		},
	}
}

func TestTheAdvertisedAddressComesFromTheServiceLoadBalancer(t *testing.T) {
	// Writing the address into the overlay by hand is how it went stale: it
	// pointed at a LoadBalancer that no longer existed, which passes
	// validation and fails only once a client signs in and switches to the
	// published connection.
	cs := fake.NewSimpleClientset(lbService(corev1.LoadBalancerIngress{IP: "172.19.0.6"}))
	m := urlManager(cs, Config{Namespace: "media", ExternalService: "plex-main"})

	assert.Equal(t, "http://172.19.0.6:32400", m.externalURL(t.Context()))
}

func TestAnExplicitAddressBeatsTheService(t *testing.T) {
	// An operator who has put a name and a certificate in front of the
	// Service means that name, not the address underneath it.
	cs := fake.NewSimpleClientset(lbService(corev1.LoadBalancerIngress{IP: "172.19.0.6"}))
	m := urlManager(cs, Config{Namespace: "media", ExternalService: "plex-main", ExternalURL: "https://plex.example.com"})

	assert.Equal(t, "https://plex.example.com", m.externalURL(t.Context()))
}

func TestAHostnameIsUsedWhenTheLoadBalancerHasNoAddress(t *testing.T) {
	// Cloud load balancers publish a name rather than an address.
	cs := fake.NewSimpleClientset(lbService(corev1.LoadBalancerIngress{Hostname: "plex.eu-west-1.elb.amazonaws.com"}))
	m := urlManager(cs, Config{Namespace: "media", ExternalService: "plex-main"})

	assert.Equal(t, "http://plex.eu-west-1.elb.amazonaws.com:32400", m.externalURL(t.Context()))
}

func TestAServiceWithNoAddressYetLeavesTheAdvertisementAlone(t *testing.T) {
	// An empty result means Enforced does not write customConnections at all,
	// which keeps whatever Plex already advertises. Clearing it would be
	// worse than being briefly out of date, and the address is resolved again
	// before every start.
	svc := lbService(corev1.LoadBalancerIngress{})
	svc.Status.LoadBalancer.Ingress = nil
	cs := fake.NewSimpleClientset(svc)
	m := urlManager(cs, Config{Namespace: "media", ExternalService: "plex-main"})

	assert.Empty(t, m.externalURL(t.Context()))
}

func TestAMissingServiceIsNotFatal(t *testing.T) {
	// Plex serves perfectly well while unreachable from outside, so failing
	// to start over this would turn a degraded cluster into a dead one.
	cs := fake.NewSimpleClientset()
	m := urlManager(cs, Config{Namespace: "media", ExternalService: "plex-main"})

	assert.Empty(t, m.externalURL(t.Context()))
}

func TestTheAdvertisedPortFollowsTheService(t *testing.T) {
	// Plex is reached on whatever port the Service publishes, which is not
	// always the one Plex itself binds.
	svc := lbService(corev1.LoadBalancerIngress{IP: "10.0.0.5"})
	svc.Spec.Ports = []corev1.ServicePort{{Name: "pms", Port: 443}}
	cs := fake.NewSimpleClientset(svc)
	m := urlManager(cs, Config{Namespace: "media", ExternalService: "plex-main"})

	require.Equal(t, "http://10.0.0.5:443", m.externalURL(t.Context()))
}
