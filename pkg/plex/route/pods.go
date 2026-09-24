package plexroute

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

// ServingAnnotation marks a pod whose Plex is accepting connections.
//
// The Lease names one holder, which is the right shape for "who talks to
// plex.tv" and the wrong shape for "who can serve a request": with Plex running
// on every pod there are several answers. Each pod therefore records its own
// state on its own object, where it cannot disagree with anyone else's.
const ServingAnnotation = "clusterplex.io/plex-serving"

// DefaultPlexSelector matches the pods that run Plex.
const DefaultPlexSelector = "app.kubernetes.io/component=plex"

// PodTracker lists the pods currently serving Plex.
type PodTracker struct {
	Client    kubernetes.Interface
	Namespace string
	// Selector picks candidate pods; DefaultPlexSelector when empty.
	Selector string
	// Port is the port Plex is reachable on, through each pod's proxy.
	Port int
	// ManagerPort is where that pod's manager answers, which is who authorizes
	// and resolves a media request.
	ManagerPort int
}

// Serving returns every pod whose Plex is accepting, sorted by name so that
// each caller builds the same ring from the same cluster state.
func (p *PodTracker) Serving(ctx context.Context) ([]Target, error) {
	selector := p.Selector
	if selector == "" {
		selector = DefaultPlexSelector
	}
	pods, err := p.Client.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list Plex pods: %w", err)
	}

	var out []Target
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[ServingAnnotation] != "true" ||
			pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		target := Target{
			Pod:     pod.Name,
			Address: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(p.Port)),
		}
		if p.ManagerPort != 0 {
			target.Manager = "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(p.ManagerPort))
		}
		out = append(out, target)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
	return out, nil
}
