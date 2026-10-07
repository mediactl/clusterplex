package main

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
)

// fastRestarts shortens the waits and returns a context whose goroutines,
// started through goWith, end before the waits are restored.
func fastRestarts(t *testing.T) (context.Context, func(func())) {
	wait, poll, self := liveTVRestartWait, liveTVRestartPoll, liveTVSelfPoll
	liveTVRestartWait, liveTVRestartPoll, liveTVSelfPoll = 2*time.Second, 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { liveTVRestartWait, liveTVRestartPoll, liveTVSelfPoll = wait, poll, self })
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() }) // runs first: cleanups are LIFO
	return ctx, func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
}

func plexPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "media",
			Labels:      map[string]string{"app.kubernetes.io/component": "plex"},
			Annotations: map[string]string{plexroute.ServingAnnotation: "true"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// answerer plays the other pods' managers: it answers a restart after
// delay, recording which pod was asked while another still had an
// unanswered request.
type answerer struct {
	mu       sync.Mutex
	order    []string
	overlaps int
}

func (a *answerer) run(ctx context.Context, t *testing.T, m *Manager, delay time.Duration) {
	pods := m.K8sClient.CoreV1().Pods("media")
	asked := map[string]time.Time{}
	for ctx.Err() == nil {
		list, err := pods.List(ctx, metav1.ListOptions{})
		if err != nil {
			return
		}
		open := 0
		for _, p := range list.Items {
			if g := p.Annotations[liveTVRestartAnnotation]; g != "" && g != p.Annotations[liveTVRestartedAnnotation] {
				open++
				if _, ok := asked[p.Name]; !ok {
					asked[p.Name] = time.Now()
					a.mu.Lock()
					a.order = append(a.order, p.Name)
					a.mu.Unlock()
				}
				if time.Since(asked[p.Name]) >= delay {
					patch := `{"metadata":{"annotations":{"` + liveTVRestartedAnnotation + `":"` + g + `"}}}`
					_, _ = pods.Patch(ctx, p.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
					delete(asked, p.Name)
				}
			}
		}
		if open > 1 {
			a.mu.Lock()
			a.overlaps++
			a.mu.Unlock()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (a *answerer) asked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

func TestARoundRestartsEachOtherPodInTurnAndNeverItself(t *testing.T) {
	ctx, goWith := fastRestarts(t)
	m := newTestManager()
	m.K8sClient = fake.NewSimpleClientset(plexPod("plex-0"), plexPod("plex-1"), plexPod("plex-2"))
	a := &answerer{}
	goWith(func() { a.run(ctx, t, m, 50*time.Millisecond) })

	var mu sync.Mutex
	var got []plexlivetv.Event
	record := func(e plexlivetv.Event) { mu.Lock(); got = append(got, e); mu.Unlock() }
	r := newLiveTVRestarts()
	goWith(func() { m.runLiveTVRestarts(ctx, r, time.Hour, record, m.Logger) })
	r.request([]plexlivetv.Provider{{Namespace: "clustarr-system", Name: "polutv", UID: "uid-1"}})

	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"plex-1", "plex-2"}, a.asked(), "the others, by name; never plex-0, the lease holder")
	assert.Zero(t, a.overlaps, "the next pod is asked only once the previous one answered")
	assert.Equal(t, plexlivetv.ReasonPodsRestarted, got[0].Reason)
	assert.Equal(t, "polutv", got[0].Name)
	assert.False(t, got[0].Warning)
	self, err := m.K8sClient.CoreV1().Pods("media").Get(ctx, "plex-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, self.Annotations, liveTVRestartAnnotation)
}

func TestASecondChangeWithinRestartEveryWaitsForIt(t *testing.T) {
	ctx, goWith := fastRestarts(t)
	m := newTestManager()
	m.K8sClient = fake.NewSimpleClientset(plexPod("plex-0"), plexPod("plex-1"))
	goWith(func() { (&answerer{}).run(ctx, t, m, 0) })

	var mu sync.Mutex
	var at []time.Time
	record := func(plexlivetv.Event) { mu.Lock(); at = append(at, time.Now()); mu.Unlock() }
	r := newLiveTVRestarts()
	const every = 400 * time.Millisecond
	goWith(func() { m.runLiveTVRestarts(ctx, r, every, record, m.Logger) })
	p := []plexlivetv.Provider{{Namespace: "clustarr-system", Name: "polutv", UID: "uid-1"}}
	start := time.Now()
	r.request(p)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(at) == 1 }, 2*time.Second, 5*time.Millisecond)
	r.request(p)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(at) == 2 }, 3*time.Second, 5*time.Millisecond)
	assert.GreaterOrEqual(t, at[1].Sub(start), every, "the second round waits for restartEvery")
}

func TestAPodWhosePMSStartedAfterTheRoundAnswersWithoutRestarting(t *testing.T) {
	ctx, goWith := fastRestarts(t)
	m := newTestManager()
	m.Config.PodName = "plex-1"
	gen := strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10)
	pod := plexPod("plex-1")
	pod.Annotations[liveTVRestartAnnotation] = gen
	m.K8sClient = fake.NewSimpleClientset(pod)
	m.pmsStarted = time.Now() // m.sup is nil: a restart would panic
	goWith(func() { m.watchLiveTVRestart(ctx) })

	require.Eventually(t, func() bool {
		p, err := m.K8sClient.CoreV1().Pods("media").Get(ctx, "plex-1", metav1.GetOptions{})
		return err == nil && p.Annotations[liveTVRestartedAnnotation] == gen
	}, 2*time.Second, 10*time.Millisecond)
}

func TestPMSStartedAfterIsMeasuredFromTheRoundsStart(t *testing.T) {
	m := newTestManager()
	round := time.Now()
	gen := strconv.FormatInt(round.UnixNano(), 10)
	assert.False(t, m.pmsStartedAfter(gen), "PMS never started")
	m.pmsStarted = round.Add(-time.Minute)
	assert.False(t, m.pmsStartedAfter(gen), "started before the change")
	m.pmsStarted = round.Add(10 * time.Second)
	assert.False(t, m.pmsStartedAfter(gen), "within the margin a restart is still owed")
	m.pmsStarted = round.Add(time.Minute)
	assert.True(t, m.pmsStartedAfter(gen))
	assert.False(t, m.pmsStartedAfter("not-a-number"))
}

func TestAFailedInPlaceRestartLeavesThePodToTheHealthWatch(t *testing.T) {
	// A restart withdraws the pod and stops its health watch first. Plex
	// failing to come back must not leave it like that, with nothing to
	// restart Plex or replace the pod (main.go's onPlexLost).
	ctx, _ := fastRestarts(t)
	start, interval := liveTVPMSStartTimeout, liveTVHealthInterval
	liveTVPMSStartTimeout, liveTVHealthInterval = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { liveTVPMSStartTimeout, liveTVHealthInterval = start, interval })
	m := newTestManager()
	m.plexAddr = "127.0.0.1:1"          // nothing answers
	m.Config.Postgres.SSLMode = "bogus" // so the start fails preparing the databases
	m.sup = &Supervisor{Binary: fakePMS(t, gracefulPMS), Logger: m.Logger, Grace: time.Second}
	lost := make(chan error, 1)
	m.onPlexLost = func(err error) {
		select {
		case lost <- err:
		default:
		}
	}

	require.Error(t, m.restartPMSInPlace(ctx))
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing took the pod over after the failed restart")
	}
}

func TestALeaderWhoseDVRListIsStaleRestartsItsOwnPMS(t *testing.T) {
	m := newTestManager()
	m.K8sClient = fake.NewSimpleClientset(plexPod("plex-0"))
	before := time.Now()

	require.NoError(t, m.restartOwnPMS(t.Context()))
	p, err := m.K8sClient.CoreV1().Pods("media").Get(t.Context(), "plex-0", metav1.GetOptions{})
	require.NoError(t, err)
	gen, err := strconv.ParseInt(p.Annotations[liveTVRestartAnnotation], 10, 64)
	require.NoError(t, err, "a generation watchLiveTVRestart acts on")
	assert.False(t, time.Unix(0, gen).Before(before))
}
