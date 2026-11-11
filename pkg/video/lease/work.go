package lease

import (
	"context"
	"fmt"
	"strconv"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WorkAnnotation is clustarr's count of the Intel class's transcode work in
// hand and waiting, which its admission publishes into a Lease of its own
// (clustarr's --intel-work-lease) by the rule this package publishes ours:
// renewed every 10 s while above zero, stale after StaleAfter.
const WorkAnnotation = "clustarr.io/intel-work"

// Work reads clustarr's intel-work Lease: while it is fresh and above zero,
// clustarr holds work for the Intel GPU's video engines, and the video
// route sends Plex's conversions to the CPU pool first
// (docs/superpowers/specs/2026-11-10-cpu-video-route-design.md).
type Work struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	// Annotation is the count's annotation. Default WorkAnnotation.
	Annotation string
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Busy is whether the Lease counts work: its annotation above zero within
// StaleAfter of its renewal. A Lease that does not exist is not busy; a
// Lease that cannot be read is an error, for the caller to log and read
// as not busy.
func (w *Work) Busy(ctx context.Context) (bool, error) {
	l, err := w.Client.CoordinationV1().Leases(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the intel-work lease %s/%s: %w", w.Namespace, w.Name, err)
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	ann := w.Annotation
	if ann == "" {
		ann = WorkAnnotation
	}
	return CountOf(l, ann, now()) > 0, nil
}

// CountOf is what a reader takes from a Lease's annotation: its value
// while the Lease was renewed within StaleAfter, else 0. Count reads ours
// through it, Work.Busy clustarr's, and clustarr's admission reads ours by
// the same rule (app/transcode/admission/videolease.go).
func CountOf(l *coordinationv1.Lease, annotation string, now time.Time) int {
	if l == nil || l.Spec.RenewTime == nil || now.Sub(l.Spec.RenewTime.Time) >= StaleAfter {
		return 0
	}
	n, err := strconv.Atoi(l.Annotations[annotation])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
