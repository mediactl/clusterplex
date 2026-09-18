package remoteexec

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultWorkerSelector matches the pods the manager labels as workers.
const DefaultWorkerSelector = "app=plex,plex-role=worker"

// PodWorkerLister finds Ready worker pods through the API server. Jobs start
// rarely enough that a List per job is cheaper than keeping an informer.
type PodWorkerLister struct {
	Client    kubernetes.Interface
	Namespace string
	// Self is this pod's name; it is never returned.
	Self string
	// Selector picks worker pods; DefaultWorkerSelector when empty.
	Selector string
	// Port is the worker manager's gRPC port.
	Port int
}

// ListReady implements WorkerLister.
func (l *PodWorkerLister) ListReady(ctx context.Context) ([]Worker, error) {
	sel := l.Selector
	if sel == "" {
		sel = DefaultWorkerSelector
	}
	pods, err := l.Client.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list worker pods: %w", err)
	}
	var out []Worker
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Name == l.Self || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || !podReady(p) {
			continue
		}
		out = append(out, Worker{Name: p.Name, Addr: net.JoinHostPort(p.Status.PodIP, strconv.Itoa(l.Port))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
