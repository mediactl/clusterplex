//go:build e2e

// Package e2e deploys the kind overlay into the current kube context and
// checks the cluster settles into one leader running Plex behind the proxy
// and two labelled workers.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	namespace = "media"
	leaseName = "cluster-plex-litefs"
	pmsPort   = 32400
	proxyPort = 32499
	grpcPort  = 50051
)

func runCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	require.NoError(t, err, "%s %v failed: %s", name, args, out)
	return string(out)
}

func execPod(t *testing.T, pod string, cmd ...string) string {
	t.Helper()
	return runCmd(t, "kubectl", append([]string{"exec", "-n", namespace, pod, "--"}, cmd...)...)
}

// tryExecPod is execPod for wait loops: an exec that fails because the
// container is still coming up is "not yet", not a test failure.
func tryExecPod(pod string, cmd ...string) (string, error) {
	out, err := exec.Command("kubectl", append([]string{"exec", "-n", namespace, pod, "--"}, cmd...)...).CombinedOutput()
	return string(out), err
}

func getK8sClient(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.ExpandEnv("$HOME/.kube/config")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	cs, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	return cs
}

func waitFor(ctx context.Context, t *testing.T, what string, cond func() (bool, error)) {
	t.Helper()
	for {
		ok, err := cond()
		require.NoError(t, err, what)
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(2 * time.Second):
		}
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// listening reports whether /proc/net/tcp{,6} content shows a LISTEN socket on port.
func listening(procNetTCP string, port int) bool {
	suffix := fmt.Sprintf(":%04X", port)
	for _, line := range strings.Split(procNetTCP, "\n") {
		f := strings.Fields(line)
		if len(f) > 3 && strings.HasSuffix(f[1], suffix) && f[3] == "0A" {
			return true
		}
	}
	return false
}

func TestClusterPlexE2E(t *testing.T) {
	// StatefulSet volume claims and pod management policy are immutable, so
	// each run starts from a fresh StatefulSet. The per-pod LiteFS claims and
	// the shared claims survive, as they would across an upgrade.
	runCmd(t, "sh", "-c", "kubectl create namespace "+namespace+" --dry-run=client -o yaml | kubectl apply -f -")
	runCmd(t, "kubectl", "delete", "statefulset", "plex", "-n", namespace, "--ignore-not-found", "--cascade=foreground", "--wait=true")
	runCmd(t, "kubectl", "apply", "-k", "../../k8s/overlays/kind")
	cs := getK8sClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	waitFor(ctx, t, "three running pods", func() (bool, error) {
		pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=plex"})
		if err != nil {
			return false, err
		}
		running := 0
		for i := range pods.Items {
			if pods.Items[i].Status.Phase == corev1.PodRunning {
				running++
			}
		}
		return running == 3, nil
	})

	var leader string
	waitFor(ctx, t, "leader election", func() (bool, error) {
		lease, err := cs.CoordinationV1().Leases(namespace).Get(ctx, leaseName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return false, nil
		}
		leader = *lease.Spec.HolderIdentity
		return true, nil
	})
	t.Logf("leader: %s", leader)

	waitFor(ctx, t, "leader pod Ready and labelled", func() (bool, error) {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, leader, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return p.Labels["plex-role"] == "leader" && podReady(p), nil
	})

	waitFor(ctx, t, "two Ready workers", func() (bool, error) {
		pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=plex,plex-role=worker"})
		if err != nil {
			return false, err
		}
		ready := 0
		for i := range pods.Items {
			if podReady(&pods.Items[i]) {
				ready++
			}
		}
		return ready == 2, nil
	})

	// The leader runs Plex behind the proxy and accepts jobs.
	waitFor(ctx, t, "Plex, proxy and worker port listening on the leader", func() (bool, error) {
		tcp, err := tryExecPod(leader, "cat", "/proc/net/tcp", "/proc/net/tcp6")
		if err != nil {
			t.Logf("exec into %s not ready yet: %v", leader, err)
			return false, nil
		}
		return listening(tcp, pmsPort) && listening(tcp, proxyPort) && listening(tcp, grpcPort), nil
	})
	rules := execPod(t, leader, "iptables", "-t", "nat", "-S", "PREROUTING")
	assert.Contains(t, rules, fmt.Sprintf("--dport %d -j REDIRECT --to-ports %d", pmsPort, proxyPort))

	// Workers do not run Plex but do accept jobs.
	workers, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=plex,plex-role=worker"})
	require.NoError(t, err)
	for i := range workers.Items {
		tcp := execPod(t, workers.Items[i].Name, "cat", "/proc/net/tcp", "/proc/net/tcp6")
		assert.False(t, listening(tcp, pmsPort), "%s must not run Plex", workers.Items[i].Name)
		assert.True(t, listening(tcp, grpcPort), "%s must accept jobs", workers.Items[i].Name)
	}

	// The client-facing Service reaches exactly the leader's proxy port.
	waitFor(ctx, t, "plex-main endpoint on the leader", func() (bool, error) {
		slices, err := cs.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=plex-main"})
		if err != nil {
			return false, err
		}
		var ready []string
		var port int32
		for _, s := range slices.Items {
			for _, ep := range s.Endpoints {
				if ep.Conditions.Ready != nil && *ep.Conditions.Ready && ep.TargetRef != nil {
					ready = append(ready, ep.TargetRef.Name)
				}
			}
			for _, p := range s.Ports {
				if p.Port != nil {
					port = *p.Port
				}
			}
		}
		return len(ready) == 1 && ready[0] == leader && port == proxyPort, nil
	})
}
