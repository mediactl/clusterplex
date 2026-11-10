package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
)

const testDVR = "1AED97B8-0f6e-4c0b-9d2a-5d0b3c2a1f00"

// holderPod is a Plex pod as the chart renders it, as far as the build
// Job reads it.
func holderPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "plex-0", Namespace: "media",
			Labels:          map[string]string{"app.kubernetes.io/name": "cluster-plex", "app.kubernetes.io/instance": "plex", "app.kubernetes.io/component": "plex"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "plex", UID: "sts-uid", Controller: ptrTo(true)}},
		},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "ghcr"}},
			Containers: []corev1.Container{{
				Name: "plex", Image: "ghcr.io/mediactl/cluster-plex:abc1234", ImagePullPolicy: corev1.PullIfNotPresent,
				Env: []corev1.EnvVar{
					{Name: "CLUSTERPLEX_POSTGRES_HOST", Value: "plex-db-rw"},
					{Name: "CLUSTERPLEX_POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "plex-db-app"}, Key: "password",
					}}},
					{Name: "CLUSTERPLEX_LEASE_NAME", Value: "cluster-plex-plextv"},
					{Name: "PLEX_CLAIM", Value: "claim-secret"},
					{Name: "GOMEMLIMIT", Value: "900MiB"},
				},
			}},
		},
	}
}

// TestABuildJobIsTheHoldersImageWithTheDatabaseAlone: the Job runs the
// holder's image and pull secrets with the PostgreSQL settings and nothing
// else of the Plex pod's, no ServiceAccount token, as nobody, owned by the
// StatefulSet, and its pod is never one the Plex Service or the serving
// selector matches.
func TestABuildJobIsTheHoldersImageWithTheDatabaseAlone(t *testing.T) {
	spec, err := parseCloudGuideJob(`{"resources":{"requests":{"cpu":"500m","memory":"1Gi"},"limits":{"memory":"3Gi"}},"nodeSelector":{"kubernetes.io/arch":"amd64"}}`)
	require.NoError(t, err)
	j, err := renderCloudJob(holderPod(), spec, testDVR, "0123456789abcdef")
	require.NoError(t, err)

	assert.Equal(t, "plex-guide-build-1aed97b8-0123456789abcdef", j.Name)
	assert.LessOrEqual(t, len(j.Name), 63)
	assert.Equal(t, "media", j.Namespace)
	require.Len(t, j.OwnerReferences, 1)
	assert.Equal(t, "StatefulSet", j.OwnerReferences[0].Kind)
	assert.Nil(t, j.OwnerReferences[0].Controller, "the StatefulSet owns the Job for clean-up only")
	assert.Equal(t, testDVR, j.Annotations[labelGuideDVR])

	pod := j.Spec.Template
	sel, err := labels.Parse(plexroute.DefaultPlexSelector)
	require.NoError(t, err)
	assert.False(t, sel.Matches(labels.Set(pod.Labels)), "never a Plex pod to the Service or the serving selector")
	assert.Equal(t, "cluster-plex", pod.Labels["app.kubernetes.io/name"])
	assert.Equal(t, cloudJobComponent, pod.Labels["app.kubernetes.io/component"])

	ps := pod.Spec
	assert.Equal(t, corev1.RestartPolicyNever, ps.RestartPolicy)
	assert.False(t, *ps.AutomountServiceAccountToken)
	assert.True(t, *ps.SecurityContext.RunAsNonRoot)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "ghcr"}}, ps.ImagePullSecrets)
	assert.Equal(t, map[string]string{"kubernetes.io/arch": "amd64"}, ps.NodeSelector)
	require.Len(t, ps.Containers, 1)
	c := ps.Containers[0]
	assert.Equal(t, "ghcr.io/mediactl/cluster-plex:abc1234", c.Image)
	assert.Equal(t, []string{"/usr/local/bin/manager", guideBuildCommand, "--dvr", testDVR, "--stamp", "0123456789abcdef"}, c.Command)
	var names []string
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"CLUSTERPLEX_POSTGRES_HOST", "CLUSTERPLEX_POSTGRES_PASSWORD", "GOMEMLIMIT"}, names,
		"the database's settings and the build's own memory limit; no lease, claim or Plex setting")
	assert.Equal(t, "plex-db-app", c.Env[1].ValueFrom.SecretKeyRef.Name, "the password stays a Secret reference")
	assert.Equal(t, strconv.FormatInt((3<<30)/10*9, 10), c.Env[2].Value, "90% of the memory limit")
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.Equal(t, resource.MustParse("3Gi"), c.Resources.Limits[corev1.ResourceMemory])

	assert.Equal(t, int64(3600), *j.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, cloudJobBackoff, *j.Spec.BackoffLimit)
	require.NotNil(t, j.Spec.PodFailurePolicy)
	assert.Equal(t, []int32{exitPlanMoved}, j.Spec.PodFailurePolicy.Rules[0].OnExitCodes.Values, "a moved plan fails the Job at once")
}

func TestABuildJobsPodSettingsAreChecked(t *testing.T) {
	spec, err := parseCloudGuideJob("")
	require.NoError(t, err)
	assert.Equal(t, cloudGuideJobSpec{}, spec)
	spec, err = parseCloudGuideJob(`{"activeDeadlineSeconds":7200}`)
	require.NoError(t, err)
	j, err := renderCloudJob(holderPod(), spec, testDVR, "s")
	require.NoError(t, err)
	assert.Equal(t, int64(7200), *j.Spec.ActiveDeadlineSeconds)
	for _, c := range j.Spec.Template.Spec.Containers[0].Env {
		assert.NotEqual(t, "GOMEMLIMIT", c.Name, "no limit, no GOMEMLIMIT")
	}
	_, err = parseCloudGuideJob(`{"activeDeadlineSeconds":0}`)
	assert.Error(t, err)
	_, err = parseCloudGuideJob(`{"resources":"lots"}`)
	assert.Error(t, err)

	noDB := holderPod()
	noDB.Spec.Containers[0].Env = noDB.Spec.Containers[0].Env[2:]
	_, err = renderCloudJob(noDB, cloudGuideJobSpec{}, testDVR, "s")
	assert.Error(t, err, "a Job that could not reach the database is not started")
}

func jobTestManager(objs ...metav1.Object) *Manager {
	cs := fake.NewSimpleClientset(holderPod())
	for _, o := range objs {
		if j, ok := o.(*batchv1.Job); ok {
			_, _ = cs.BatchV1().Jobs(j.Namespace).Create(context.Background(), j, metav1.CreateOptions{})
		}
	}
	return &Manager{
		Config:    Config{Namespace: "media", PodName: "plex-0"},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		K8sClient: cs,
	}
}

func finished(j *batchv1.Job, typ batchv1.JobConditionType, msg string) *batchv1.Job {
	j.Status.Conditions = []batchv1.JobCondition{{Type: typ, Status: corev1.ConditionTrue, Reason: string(typ), Message: msg}}
	return j
}

// TestAHolderAdoptsARunningBuildJobAndReplacesAFinishedOne: the next holder
// waits on the Job its predecessor started; a Job of the same plan that
// finished (installed, or failed) gives way to a new one.
func TestAHolderAdoptsARunningBuildJobAndReplacesAFinishedOne(t *testing.T) {
	ctx := context.Background()
	running, err := renderCloudJob(holderPod(), cloudGuideJobSpec{}, testDVR, "stamp1")
	require.NoError(t, err)
	running.Labels["adopted"] = "yes"
	m := jobTestManager(running)
	c := newCloudGuides(m)
	j, err := c.ensureCloudJob(ctx, testDVR, "stamp1")
	require.NoError(t, err)
	assert.Equal(t, "yes", j.Labels["adopted"], "the running Job is the one waited on")
	list, err := m.K8sClient.BatchV1().Jobs("media").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list.Items, 1)

	for _, typ := range []batchv1.JobConditionType{batchv1.JobComplete, batchv1.JobFailed} {
		old, err := renderCloudJob(holderPod(), cloudGuideJobSpec{}, testDVR, "stamp1")
		require.NoError(t, err)
		old.Labels["adopted"] = "old"
		m := jobTestManager(finished(old, typ, ""))
		j, err := newCloudGuides(m).ensureCloudJob(ctx, testDVR, "stamp1")
		require.NoError(t, err, typ)
		assert.Empty(t, j.Labels["adopted"], "%s: a new Job replaced the finished one", typ)
		assert.Empty(t, j.Status.Conditions, typ)
	}
}

// jobTestDB is a connection to a fresh library database, and the guide
// the tests build.
func jobTestDB(t *testing.T) (*pgx.Conn, plexbootstrap.Guide) {
	t.Helper()
	db := libraryDatabase(t)
	cfg, err := pgx.ParseConfig(db.DSN())
	require.NoError(t, err)
	conn, err := plexbootstrap.ConnectWith(cfg)(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn, plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: testDVR}
}

// TestTheHolderWaitsForItsJobsBuildAndTakesOneStagedAlready.
func TestTheHolderWaitsForItsJobsBuildAndTakesOneStagedAlready(t *testing.T) {
	conn, g := jobTestDB(t)
	ctx := context.Background()
	before := cloudJobPoll
	cloudJobPoll = 50 * time.Millisecond
	t.Cleanup(func() { cloudJobPoll = before })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	stage := func(stamp string) {
		require.NoError(t, plexbootstrap.StageBuilt(ctx, quiet, conn, "plex", g, stamp, func(context.Context, pgx.Tx, string) error { return nil }))
	}

	// Staged already: the holder before this one waited for it.
	stage("stamp1")
	m := jobTestManager()
	require.NoError(t, newCloudGuides(m).stageInJob(ctx, quiet, conn, g, "stamp1"))
	list, err := m.K8sClient.BatchV1().Jobs("media").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items, "no Job for a build already staged")

	// A Job that stages while the holder waits.
	m = jobTestManager()
	done := make(chan error, 1)
	go func() { done <- newCloudGuides(m).stageInJob(ctx, quiet, conn, g, "stamp2") }()
	require.Eventually(t, func() bool {
		_, err := m.K8sClient.BatchV1().Jobs("media").Get(ctx, cloudJobName(testDVR, "stamp2"), metav1.GetOptions{})
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	// The build Job's own connection.
	db := libraryConn(t, conn)
	require.NoError(t, plexbootstrap.StageBuilt(ctx, quiet, db, "plex", g, "stamp2", func(context.Context, pgx.Tx, string) error { return nil }))
	require.NoError(t, <-done)

	// A Job that fails.
	m = jobTestManager()
	go func() { done <- newCloudGuides(m).stageInJob(ctx, quiet, conn, g, "stamp3") }()
	name := cloudJobName(testDVR, "stamp3")
	require.Eventually(t, func() bool {
		j, err := m.K8sClient.BatchV1().Jobs("media").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		_, err = m.K8sClient.BatchV1().Jobs("media").UpdateStatus(ctx, finished(j, batchv1.JobFailed, "Job has reached the specified backoff limit"), metav1.UpdateOptions{})
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	err = <-done
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backoff limit")
}

// libraryConn is a second connection to conn's database.
func libraryConn(t *testing.T, conn *pgx.Conn) *pgx.Conn {
	t.Helper()
	c, err := pgx.ConnectConfig(context.Background(), conn.Config())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// TestTheGuideBuildCommandStagesTheStampedPlanAndRefusesAMovedOne runs
// `manager guide-build` as a Job would: from the environment the Plex pod
// passes, against the cache the pods filled.
func TestTheGuideBuildCommandStagesTheStampedPlanAndRefusesAMovedOne(t *testing.T) {
	conn, g := jobTestDB(t)
	ctx := context.Background()
	cfg := conn.Config()
	for k, v := range map[string]string{
		"CLUSTERPLEX_POSTGRES_HOST": cfg.Host, "CLUSTERPLEX_POSTGRES_PORT": strconv.Itoa(int(cfg.Port)),
		"CLUSTERPLEX_POSTGRES_DATABASE": cfg.Database, "CLUSTERPLEX_POSTGRES_USER": cfg.User,
		"CLUSTERPLEX_POSTGRES_PASSWORD": cfg.Password, "CLUSTERPLEX_POSTGRES_SCHEMA": "plex",
	} {
		t.Setenv(k, v)
	}
	require.NoError(t, epgcloud.EnsureCache(ctx, conn))
	p := cloudPlan{
		DVR: testDVR, Lineup: "lineup1", Channels: []guidebuild.Channel{{GridKey: "k1", VCN: "3"}},
		Start: time.Now().UTC().Truncate(24 * time.Hour),
	}
	body, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, epgcloud.CachePut(ctx, conn, planPrefix+testDVR, body, time.Now()))
	stamp := cloudPlanStamp(body)

	assert.Equal(t, 1, runGuideBuild([]string{"--dvr", testDVR, "--stamp", stamp}), "no lineup cached: the holder fetches it first")
	require.NoError(t, epgcloud.CachePut(ctx, conn, epgcloud.LineupKey("lineup1"),
		[]byte(`{"MediaContainer":{"Channel":[{"id":"lineup1-k1","gridKey":"k1","vcn":"3","callSign":"WEAR","title":"WEAR"}]}}`), time.Now()))

	assert.Equal(t, exitPlanMoved, runGuideBuild([]string{"--dvr", testDVR, "--stamp", "another"}))
	have, _, err := plexbootstrap.StagedBuild(ctx, conn, g)
	require.NoError(t, err)
	assert.Empty(t, have, "a moved plan stages nothing")

	assert.Equal(t, 0, runGuideBuild([]string{"--dvr", testDVR, "--stamp", stamp}))
	have, _, err = plexbootstrap.StagedBuild(ctx, conn, g)
	require.NoError(t, err)
	assert.Equal(t, stamp, have)
	assert.Equal(t, 1, one(t, conn, fmt.Sprintf("SELECT count(*) FROM %s.tags WHERE tag_type = 310", pgx.Identifier{g.Staging()}.Sanitize())),
		"the plan's one channel is in the staged guide")

	assert.Equal(t, 2, runGuideBuild([]string{"--dvr", testDVR}), "a build without its stamp is a usage error")
}

func one(t *testing.T, conn *pgx.Conn, sql string) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(context.Background(), sql).Scan(&n))
	return n
}

// TestANewHolderSeesABuildItsPredecessorLeftRunning: at its first look a
// holder reads only when the guide was last installed, so a build Job left
// running by the holder before it must make the guide due, or the build is
// never swapped in.
func TestANewHolderSeesABuildItsPredecessorLeftRunning(t *testing.T) {
	ctx := context.Background()
	running, err := renderCloudJob(holderPod(), cloudGuideJobSpec{}, testDVR, "stamp1")
	require.NoError(t, err)
	c := newCloudGuides(jobTestManager(running))
	assert.True(t, c.inFlight(ctx, testDVR))
	assert.False(t, c.inFlight(ctx, "16cac8a1-0746-4e63-9a32-c9d0fd96a0c1"), "another DVR's Job is not this guide's")

	done, err := renderCloudJob(holderPod(), cloudGuideJobSpec{}, testDVR, "stamp1")
	require.NoError(t, err)
	c = newCloudGuides(jobTestManager(finished(done, batchv1.JobComplete, "")))
	assert.False(t, c.inFlight(ctx, testDVR), "a finished Job is no build under way")
}
