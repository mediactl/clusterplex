package e2e

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
)

func runCmd(t *testing.T, cmdStr string, args ...string) {
	cmd := exec.Command(cmdStr, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run(), "Command failed: %s %v", cmdStr, args)
}

func getK8sClient(t *testing.T) *kubernetes.Clientset {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.ExpandEnv("$HOME/.kube/config")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	return clientset
}

func TestClusterPlexE2E(t *testing.T) {
	// Apply RBAC, Service, and StatefulSet
	runCmd(t, "kubectl", "apply", "-f", "../../k8s/rbac.yaml")
	runCmd(t, "kubectl", "apply", "-f", "../../k8s/service.yaml")
	runCmd(t, "kubectl", "apply", "-f", "../../k8s/statefulset.yaml")

	clientset := getK8sClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Wait for StatefulSet pods to be created and running
	t.Log("Waiting for pods to start...")
	var podNames []string
	for {
		select {
		case <-ctx.Done():
			t.Fatal("Timeout waiting for pods")
		default:
		}

		pods, err := clientset.CoreV1().Pods("media").List(ctx, metav1.ListOptions{LabelSelector: "app=plex"})
		require.NoError(t, err)

		running := 0
		podNames = nil
		for _, pod := range pods.Items {
			if pod.Status.Phase == "Running" {
				running++
				podNames = append(podNames, pod.Name)
			}
		}

		if running == 3 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	t.Logf("Pods are running: %v", podNames)

	// Verify Leader Election via Lease
	t.Log("Waiting for LiteFS leader election lease...")
	var leader string
	for {
		select {
		case <-ctx.Done():
			t.Fatal("Timeout waiting for leader election")
		default:
		}

		lease, err := clientset.CoordinationV1().Leases("media").Get(ctx, "cluster-plex-litefs", metav1.GetOptions{})
		if err == nil && lease.Spec.HolderIdentity != nil {
			leader = *lease.Spec.HolderIdentity
			break
		}
		time.Sleep(2 * time.Second)
	}

	t.Logf("Leader elected: %s", leader)
	assert.Contains(t, podNames, leader, "Leader should be one of the StatefulSet pods")

	// Wait for the leader pod to become Ready (signaling LiteFS is fully up)
	t.Log("Waiting for leader pod to become Ready...")
	for {
		select {
		case <-ctx.Done():
			t.Fatal("Timeout waiting for leader pod readiness")
		default:
		}

		pod, err := clientset.CoreV1().Pods("media").Get(ctx, leader, metav1.GetOptions{})
		require.NoError(t, err)

		isReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				isReady = true
				break
			}
		}

		if isReady {
			break
		}
		time.Sleep(2 * time.Second)
	}
	
	t.Log("E2E test passed!")
}
