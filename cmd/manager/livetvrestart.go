package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
)

// Each PMS reads its DVRs once, at its start, and no call refreshes them
// (recorded, docs/livetv-pms-calls.md). So after a pass that created or
// deleted a DVR, the lease holder asks every other serving pod, one at a
// time, to restart its PMS: it writes a generation to the pod's restart
// annotation, and the pod's own manager restarts PMS in place and answers
// with the same generation once Plex serves its library again.
const (
	liveTVRestartAnnotation   = "livetv.clusterplex.io/restart"
	liveTVRestartedAnnotation = "livetv.clusterplex.io/restarted"
	// liveTVStartMargin: a PMS started this long after a round began
	// already read the DVRs that round restarts for, so it need not restart.
	liveTVStartMargin = 30 * time.Second
)

// Variables so the tests can shorten them.
var (
	// liveTVRestartWait bounds the wait for one pod's answer.
	liveTVRestartWait = 5 * time.Minute
	// liveTVRestartPoll is how often the lease holder reads that answer, and
	// liveTVSelfPoll how often each pod reads its own annotation.
	liveTVRestartPoll = 5 * time.Second
	liveTVSelfPoll    = 10 * time.Second
)

// liveTVRestarts gathers the providers whose DVRs changed until the next
// round; a change during a round is taken by the one after.
type liveTVRestarts struct {
	mu      sync.Mutex
	pending map[string]plexlivetv.Provider
	kick    chan struct{}
}

func newLiveTVRestarts() *liveTVRestarts {
	return &liveTVRestarts{kick: make(chan struct{}, 1)}
}

// request asks for a round, for changed.
func (r *liveTVRestarts) request(changed []plexlivetv.Provider) {
	r.mu.Lock()
	if r.pending == nil {
		r.pending = map[string]plexlivetv.Provider{}
	}
	for _, p := range changed {
		r.pending[p.Namespace+"/"+p.Name] = p
	}
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *liveTVRestarts) take() (map[string]plexlivetv.Provider, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending
	r.pending = nil
	return p, p != nil
}

// runLiveTVRestarts is the lease holder's rounds, at most one per every.
func (m *Manager) runLiveTVRestarts(ctx context.Context, r *liveTVRestarts, every time.Duration, record func(plexlivetv.Event), log *slog.Logger) {
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		}
		if !last.IsZero() {
			if wait := time.Until(last.Add(every)); wait > 0 {
				log.Info("Live TV: a DVR changed; restarting PMS on the other pods after the rate limit", "in", wait.Round(time.Second))
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}
		changed, ok := r.take()
		if !ok {
			continue
		}
		last = time.Now()
		gen := strconv.FormatInt(last.UnixNano(), 10)
		pods, err := m.otherServingPods(ctx)
		if err != nil {
			log.Warn("Live TV: list the other Plex pods; their PMS keeps its old DVRs until it restarts", "error", err)
			continue
		}
		var restarted, missed []string
		for _, pod := range pods {
			if err := m.restartPodPMS(ctx, pod, gen); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("Live TV: a pod's PMS did not restart", "pod", pod, "error", err)
				missed = append(missed, pod)
				continue
			}
			restarted = append(restarted, pod)
		}
		msg := fmt.Sprintf("restarted PMS on %d other pod(s) [%s] so they list the new or removed DVRs", len(restarted), strings.Join(restarted, ", "))
		if len(missed) > 0 {
			msg += fmt.Sprintf("; no answer from [%s]", strings.Join(missed, ", "))
		}
		log.Info("Live TV: "+msg, "generation", gen)
		if len(pods) == 0 {
			continue
		}
		keys := make([]string, 0, len(changed))
		for k := range changed {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := changed[k]
			record(plexlivetv.Event{
				Namespace: p.Namespace, Name: p.Name, UID: p.UID,
				Reason: plexlivetv.ReasonPodsRestarted, Message: msg, Warning: len(missed) > 0,
			})
		}
	}
}

// otherServingPods is every pod serving Plex but this one, by name.
func (m *Manager) otherServingPods(ctx context.Context) ([]string, error) {
	pods, err := m.K8sClient.CoreV1().Pods(m.Config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: plexroute.DefaultPlexSelector,
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Name == m.Config.PodName || p.Annotations[servingAnnotation] != "true" || p.Status.Phase != corev1.PodRunning {
			continue
		}
		out = append(out, p.Name)
	}
	slices.Sort(out)
	return out, nil
}

// restartPodPMS asks pod to restart its PMS under gen and waits for its
// answer.
func (m *Manager) restartPodPMS(ctx context.Context, pod, gen string) error {
	pods := m.K8sClient.CoreV1().Pods(m.Config.Namespace)
	payload := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, liveTVRestartAnnotation, gen)
	if _, err := pods.Patch(ctx, pod, types.MergePatchType, payload, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate: %w", err)
	}
	deadline := time.Now().Add(liveTVRestartWait)
	for {
		p, err := pods.Get(ctx, pod, metav1.GetOptions{})
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err == nil && p.Annotations[liveTVRestartedAnnotation] == gen:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("no answer within %s", liveTVRestartWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(liveTVRestartPoll):
		}
	}
}

// watchLiveTVRestart runs on every pod: on a new generation in its own
// restart annotation it restarts PMS in place -- withdrawn, drained,
// started again as at boot -- and answers once Plex serves its library.
func (m *Manager) watchLiveTVRestart(ctx context.Context) {
	log := m.Logger.With("component", "livetv-restart")
	pods := m.K8sClient.CoreV1().Pods(m.Config.Namespace)
	handled := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(liveTVSelfPoll):
		}
		p, err := pods.Get(ctx, m.Config.PodName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		gen := p.Annotations[liveTVRestartAnnotation]
		if gen == "" || gen == handled || gen == p.Annotations[liveTVRestartedAnnotation] {
			continue
		}
		handled = gen
		if m.pmsStartedAfter(gen) {
			log.Info("Live TV: PMS started after the DVR change; no restart needed", "generation", gen)
		} else {
			log.Info("Live TV: restarting PMS so it lists the new or removed DVRs", "generation", gen)
			if err := m.restartPMSInPlace(ctx); err != nil {
				log.Error("Live TV: restart PMS", "error", err)
				continue
			}
		}
		payload := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, liveTVRestartedAnnotation, gen)
		if _, err := m.patchPod(ctx, payload); err != nil {
			log.Warn("Live TV: answer the restart", "error", err)
		}
	}
}

// pmsStartedAfter reports whether this pod's PMS started well after the
// round gen (a UnixNano) began, and so already read its DVRs.
func (m *Manager) pmsStartedAfter(gen string) bool {
	ns, err := strconv.ParseInt(gen, 10, 64)
	if err != nil {
		return false
	}
	m.mu.RLock()
	started := m.pmsStarted
	m.mu.RUnlock()
	return !started.IsZero() && started.After(time.Unix(0, ns).Add(liveTVStartMargin))
}

// restartPMSInPlace stops and starts PMS the way a pod does at shutdown and
// boot, without replacing the pod: the health watch first, so it does not
// count the stop as a failure, then the routing withdrawal and the drain.
func (m *Manager) restartPMSInPlace(ctx context.Context) error {
	m.stopHealthWatch()
	m.withdraw(ctx)
	if err := m.sup.Stop(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	m.start(ctx)
	if err := plexroute.WaitServing(ctx, m.plexURL(), pmsStartTimeout); err != nil {
		return fmt.Errorf("PMS did not serve again: %w", err)
	}
	deadline := time.Now().Add(pmsStartTimeout)
	for {
		_, err := m.pms().Sections(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return fmt.Errorf("PMS did not answer for its library: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(liveTVRestartPoll):
		}
	}
}
