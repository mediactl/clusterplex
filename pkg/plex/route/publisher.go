package route

import (
	"context"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// dialInterval is how often WaitListening retries the port.
const dialInterval = 100 * time.Millisecond

// Publisher records on the Lease that this pod's Plex is accepting
// connections. Holding the Lease says a pod should run Plex; this says it
// actually can serve, which is what traffic follows.
type Publisher struct {
	Client    kubernetes.Interface
	Namespace string
	LeaseName string
	Pod       string
}

// Publish advertises this pod's Plex at address, and the manager beside it at
// manager.
func (p *Publisher) Publish(ctx context.Context, address, manager string) error {
	value, err := MarshalAvailability(Target{Pod: p.Pod, Address: address, Manager: manager})
	if err != nil {
		return err
	}
	return p.update(ctx, func(annotations map[string]string) bool {
		if annotations[AvailabilityAnnotation] == value {
			return false
		}
		annotations[AvailabilityAnnotation] = value
		return true
	})
}

// Clear withdraws this pod, so the tracker stops routing to it. It leaves an
// annotation naming another pod alone: a departing leader must not clear the
// availability its successor has already published.
func (p *Publisher) Clear(ctx context.Context) error {
	return p.update(ctx, func(annotations map[string]string) bool {
		raw, ok := annotations[AvailabilityAnnotation]
		if !ok {
			return false
		}
		if target, err := UnmarshalAvailability(raw); err == nil && target.Pod != p.Pod {
			return false
		}
		delete(annotations, AvailabilityAnnotation)
		return true
	})
}

// update applies mutate to the lease's annotations, re-reading and trying
// again when something else wrote the lease first.
//
// The conflict is ordinary here rather than exceptional: the holder renews
// this same lease every few seconds, and every pod annotates it to advertise
// itself, so the two race by design. Returning the conflict made the caller
// give up — and advertiseWhenAccepting gives up completely, leaving the pod
// unadvertised, never marked serving, never ready, and with no health watch
// to restart Plex. One lost race cost the whole pod.
func (p *Publisher) update(ctx context.Context, mutate func(map[string]string) bool) error {
	leases := p.Client.CoordinationV1().Leases(p.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lease, err := leases.Get(ctx, p.LeaseName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("get lease: %w", err)
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		if !mutate(lease.Annotations) {
			return nil
		}
		if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
			// Returned unwrapped: RetryOnConflict has to recognise it, and
			// wrapping hides it from apierrors.IsConflict.
			if apierrors.IsConflict(err) {
				return err
			}
			return fmt.Errorf("update lease: %w", err)
		}
		return nil
	})
}

// WaitListening blocks until addr accepts a TCP connection, or timeout passes.
// Plex takes seconds to bind after it starts, and advertising it before then
// sends clients to a closed port.
func WaitListening(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var dialer net.Dialer
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nothing listening on %s after %s: %w", addr, timeout, err)
		}
		sleep(ctx, dialInterval)
	}
}
