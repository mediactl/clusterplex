package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/viper"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

// The build Job (plex.liveTV.cloudGuideBuild "job", the default): the
// lease holder plans a cloud guide and has the pods fetch it as before,
// then starts a Job that reads the cache and writes the guide's staging
// schema (plexbootstrap.StageBuilt), and installs the staged build itself
// (plexbootstrap.SwapStaged) once PMS may have it. The build's CPU and
// memory are the Job pod's, off the Plex pod and on any node the chart's
// plex.liveTV.cloudGuideJob allows.
//
// Reliability rests on three things:
//   - The Job is named by the plan's stamp (a hash of the plan as cached), so
//     a holder that lost the lease mid-build and the one that follows find
//     the same Job rather than start a second.
//   - The build and its record in clusterplex.guide_builds commit in one
//     transaction, so a staged build is whole or absent; the holder swaps
//     only the stamp it planned (ErrNotStaged otherwise).
//   - The Job holds the guide's advisory lock while it builds, released if
//     its pod dies; a second build of the guide waits for the first.

const (
	// guideBuildCommand is the manager's subcommand a build Job runs.
	guideBuildCommand = "guide-build"
	// CloudGuideBuildJob and CloudGuideBuildInProcess are where a built
	// cloud guide is computed (LiveTVConfig.CloudGuideBuild).
	CloudGuideBuildJob       = "job"
	CloudGuideBuildInProcess = "inProcess"
	// cloudJobComponent labels a build Job and its pod; never "plex", which
	// the Plex Service, PDB and serving-pod selector match.
	cloudJobComponent = "guide-build"
	// cloudJobDeadline is a build Job's activeDeadlineSeconds unless the
	// chart sets one.
	cloudJobDeadline = time.Hour
	// cloudJobBackoff is a build Job's retries before it fails.
	cloudJobBackoff = int32(2)
	// cloudJobTTL keeps a finished Job, and its pod's log, this long.
	cloudJobTTL = int32(6 * 3600)
	// exitPlanMoved is a build Job's exit when the cached plan is no longer
	// the one it was started for: the holder planned again, and a retry
	// would fail the same way (podFailurePolicy fails the Job on it).
	exitPlanMoved = 3
	// labelGuideDVR and labelGuideStamp name a build Job's guide and plan.
	labelGuideDVR   = "clusterplex.io/guide-dvr"
	labelGuideStamp = "clusterplex.io/guide-stamp"
)

// cloudJobPoll is how often the holder looks at its Job and the record (a
// variable for the tests).
var cloudJobPoll = 10 * time.Second

// ptrTo is a pointer to v.
func ptrTo[T any](v T) *T { return &v }

// cloudGuideJobSpec is the build Job's pod as plex.liveTV.cloudGuideJob
// sets it (the chart renders it as JSON).
type cloudGuideJobSpec struct {
	Resources             corev1.ResourceRequirements `json:"resources,omitempty"`
	NodeSelector          map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations           []corev1.Toleration         `json:"tolerations,omitempty"`
	Affinity              *corev1.Affinity            `json:"affinity,omitempty"`
	ActiveDeadlineSeconds *int64                      `json:"activeDeadlineSeconds,omitempty"`
}

// parseCloudGuideJob reads plex.liveTV.cloudGuideJob; "" is the defaults.
func parseCloudGuideJob(s string) (cloudGuideJobSpec, error) {
	var spec cloudGuideJobSpec
	if strings.TrimSpace(s) == "" || strings.TrimSpace(s) == "null" {
		return spec, nil
	}
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return spec, fmt.Errorf("not the build Job's pod: %w", err)
	}
	if d := spec.ActiveDeadlineSeconds; d != nil && *d <= 0 {
		return spec, fmt.Errorf("activeDeadlineSeconds: %d is not positive", *d)
	}
	return spec, nil
}

// cloudPlanStamp names a plan by its cached bytes.
func cloudPlanStamp(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}

// cloudJobName is a plan's build Job's name: the DVR's first eight
// letters and digits and the stamp, well under 63 characters.
func cloudJobName(dvr, stamp string) string {
	return "plex-guide-build-" + dvrShort(dvr) + "-" + stamp
}

// dvrShort is a DVR uuid's first eight letters and digits, lowercase.
func dvrShort(dvr string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(dvr) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == 8 {
				break
			}
		}
	}
	return b.String()
}

// renderCloudJob is the build Job for dvr's plan stamp, made from the
// holder's own pod: its image, pull secrets and PostgreSQL settings, the
// StatefulSet that owns it as the Job's owner. The pod gets no
// ServiceAccount token and no Plex token: it reads and writes PostgreSQL
// alone.
func renderCloudJob(own *corev1.Pod, spec cloudGuideJobSpec, dvr, stamp string) (*batchv1.Job, error) {
	var plex *corev1.Container
	for i := range own.Spec.Containers {
		if own.Spec.Containers[i].Name == "plex" {
			plex = &own.Spec.Containers[i]
		}
	}
	if plex == nil && len(own.Spec.Containers) > 0 {
		plex = &own.Spec.Containers[0]
	}
	if plex == nil {
		return nil, fmt.Errorf("pod %s has no container to take the image from", own.Name)
	}
	var env []corev1.EnvVar
	for _, e := range plex.Env {
		if strings.HasPrefix(e.Name, envPrefix+"_POSTGRES_") {
			env = append(env, e)
		}
	}
	if len(env) == 0 {
		return nil, fmt.Errorf("pod %s's container %s sets no %s_POSTGRES_ variable", own.Name, plex.Name, envPrefix)
	}
	if limit := spec.Resources.Limits.Memory(); limit != nil && !limit.IsZero() {
		env = append(env, corev1.EnvVar{Name: "GOMEMLIMIT", Value: strconv.FormatInt(limit.Value()/10*9, 10)})
	}
	labels := map[string]string{
		"app.kubernetes.io/component": cloudJobComponent,
		labelGuideDVR:                 dvrShort(dvr),
		labelGuideStamp:               stamp,
	}
	for _, k := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance"} {
		if v := own.Labels[k]; v != "" {
			labels[k] = v
		}
	}
	var owners []metav1.OwnerReference
	if ref := metav1.GetControllerOf(own); ref != nil {
		owners = []metav1.OwnerReference{{APIVersion: ref.APIVersion, Kind: ref.Kind, Name: ref.Name, UID: ref.UID}}
	}
	deadline := int64(cloudJobDeadline / time.Second)
	if spec.ActiveDeadlineSeconds != nil {
		deadline = *spec.ActiveDeadlineSeconds
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cloudJobName(dvr, stamp),
			Namespace:       own.Namespace,
			Labels:          labels,
			Annotations:     map[string]string{labelGuideDVR: dvr},
			OwnerReferences: owners,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrTo(cloudJobBackoff),
			ActiveDeadlineSeconds:   ptrTo(deadline),
			TTLSecondsAfterFinished: ptrTo(cloudJobTTL),
			PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{
				{Action: batchv1.PodFailurePolicyActionFailJob, OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
					Operator: batchv1.PodFailurePolicyOnExitCodesOpIn, Values: []int32{exitPlanMoved},
				}},
				{Action: batchv1.PodFailurePolicyActionIgnore, OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
					{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue},
				}},
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptrTo(false),
					EnableServiceLinks:           ptrTo(false),
					ImagePullSecrets:             own.Spec.ImagePullSecrets,
					NodeSelector:                 spec.NodeSelector,
					Tolerations:                  spec.Tolerations,
					Affinity:                     spec.Affinity,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptrTo(true),
						RunAsUser:      ptrTo(int64(65534)),
						RunAsGroup:     ptrTo(int64(65534)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            cloudJobComponent,
						Image:           plex.Image,
						ImagePullPolicy: plex.ImagePullPolicy,
						Command:         []string{"/usr/local/bin/manager", guideBuildCommand, "--dvr", dvr, "--stamp", stamp},
						Env:             env,
						Resources:       spec.Resources,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptrTo(false),
							ReadOnlyRootFilesystem:   ptrTo(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}, nil
}

// jobFinished is a Job's terminal condition: complete, or failed with its
// reason and message.
func jobFinished(j *batchv1.Job) (done, failed bool, why string) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false, ""
		case batchv1.JobFailed:
			return true, true, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return false, false, ""
}

// ensureCloudJob starts dvr's build Job for stamp, or finds the one that
// is running. A finished one under the name is a build already installed
// or one that failed: it is deleted, and once it is gone a new one starts.
func (c *cloudGuides) ensureCloudJob(ctx context.Context, dvr, stamp string) (*batchv1.Job, error) {
	jobs := c.m.K8sClient.BatchV1().Jobs(c.m.Config.Namespace)
	name := cloudJobName(dvr, stamp)
	if j, err := jobs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		if done, _, _ := jobFinished(j); !done {
			return j, nil
		}
		if err := c.deleteCloudJob(ctx, name); err != nil {
			return nil, err
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read the build Job %s: %w", name, err)
	}
	own, err := c.m.K8sClient.CoreV1().Pods(c.m.Config.Namespace).Get(ctx, c.m.Config.PodName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read this pod for the build Job's image: %w", err)
	}
	spec, err := parseCloudGuideJob(c.m.Config.LiveTV.CloudGuideJob)
	if err != nil {
		return nil, err
	}
	j, err := renderCloudJob(own, spec, dvr, stamp)
	if err != nil {
		return nil, err
	}
	created, err := jobs.Create(ctx, j, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return jobs.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("start the build Job %s: %w", name, err)
	}
	c.log().Info("Live TV: started a cloud guide's build Job", "dvr", dvr, "job", name)
	return created, nil
}

// deleteCloudJob deletes a finished build Job and its pod, and waits until
// it is gone, so its name is free.
func (c *cloudGuides) deleteCloudJob(ctx context.Context, name string) error {
	jobs := c.m.K8sClient.BatchV1().Jobs(c.m.Config.Namespace)
	err := jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: ptrTo(metav1.DeletePropagationForeground)})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the finished build Job %s: %w", name, err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := jobs.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the finished build Job %s is still there", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// stageInJob has dvr's guide built under stamp by a Job, and returns once
// the build is staged. A build already staged under the stamp is taken as
// it is: the holder before this one waited for it and lost the lease.
func (c *cloudGuides) stageInJob(ctx context.Context, log *slog.Logger, conn *pgx.Conn, g plexbootstrap.Guide, stamp string) error {
	if have, _, err := plexbootstrap.StagedBuild(ctx, conn, g); err != nil {
		return err
	} else if have == stamp {
		return nil
	}
	j, err := c.ensureCloudJob(ctx, g.DVR, stamp)
	if err != nil {
		return err
	}
	jobs := c.m.K8sClient.BatchV1().Jobs(c.m.Config.Namespace)
	wait := cloudJobDeadline
	if d := j.Spec.ActiveDeadlineSeconds; d != nil {
		wait = time.Duration(*d) * time.Second
	}
	deadline := time.Now().Add(wait + 10*time.Minute)
	finishedWithout := 0
	for {
		have, _, err := plexbootstrap.StagedBuild(ctx, conn, g)
		if err != nil {
			return err
		}
		if have == stamp {
			return nil
		}
		j, err = jobs.Get(ctx, j.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return fmt.Errorf("the build Job %s is gone and staged nothing", cloudJobName(g.DVR, stamp))
		case err != nil:
			log.Warn("Live TV: read a cloud guide's build Job", "job", cloudJobName(g.DVR, stamp), "error", err)
		default:
			if done, failed, why := jobFinished(j); failed {
				return fmt.Errorf("the build Job %s failed: %s", j.Name, why)
			} else if done {
				// The build commits before its process ends; one more look
				// rules out reading the Job before the record.
				if finishedWithout++; finishedWithout > 1 {
					return fmt.Errorf("the build Job %s completed but staged another build (%q)", j.Name, have)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the build Job %s staged nothing in %s", cloudJobName(g.DVR, stamp), wait+10*time.Minute)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cloudJobPoll):
		}
	}
}

// cloudBuildFunc writes in's guide into a staging schema: the sections the
// staging schema holds (the live guide's, or PMS's three) and the build's
// clock are set here, and the build's counts land in *stats.
func cloudBuildFunc(in *guidebuild.Input, stats *guidebuild.Stats) plexbootstrap.BuildFunc {
	return func(ctx context.Context, tx pgx.Tx, staging string) error {
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT
			coalesce(max(id) FILTER (WHERE name = 'Movies'), 1), coalesce(max(id) FILTER (WHERE name = 'Shows'), 2),
			coalesce(max(id) FILTER (WHERE name = 'Sports'), 3) FROM %s.library_sections`, pgx.Identifier{staging}.Sanitize())).
			Scan(&in.Sections.Movies, &in.Sections.Shows, &in.Sections.Sports); err != nil {
			return err
		}
		in.Now = time.Now()
		var err error
		*stats, err = guidebuild.Build(ctx, tx, staging, *in)
		return err
	}
}

// runGuideBuild is `manager guide-build --dvr <uuid> --stamp <stamp>`, a
// build Job's process: it reads the plan the holder cached for dvr, checks
// it is still the stamped one, reads its answers from the cache and stages
// the guide. It returns the exit code.
func runGuideBuild(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "livetv-guide-build")
	dvr, stamp, pg, err := guideBuildConfig(args)
	if err != nil {
		log.Error("invalid build configuration", "error", err)
		return 2
	}
	log = log.With("dvr", dvr, "stamp", stamp)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := pgx.ParseConfig(pg.DSN())
	if err != nil {
		log.Error("postgres settings", "error", err)
		return 2
	}
	conn, err := plexbootstrap.ConnectWith(cfg)(ctx)
	if err != nil {
		log.Error("connect to the library database", "error", err)
		return 1
	}
	defer func() { _ = conn.Close(context.Background()) }()
	began := time.Now()
	p, err := readStampedPlan(ctx, conn, dvr, stamp)
	if errors.Is(err, errPlanMoved) {
		log.Warn("the plan this build was started for is no longer the cached one; nothing built", "error", err)
		return exitPlanMoved
	} else if err != nil {
		log.Error("read the build's plan", "error", err)
		return 1
	}
	in, err := cloudInput(ctx, conn, p)
	if err != nil {
		log.Error("read the build's answers from the cache", "error", err)
		return 1
	}
	var stats guidebuild.Stats
	g := plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: dvr}
	if err := plexbootstrap.StageBuilt(ctx, log, conn, pg.Schema, g, stamp, cloudBuildFunc(&in, &stats)); err != nil {
		log.Error("stage the guide", "error", err)
		return 1
	}
	log.Info("Live TV: staged a cloud guide built from Plex's guide service", "channels", stats.Channels,
		"items", stats.Items, "airings", stats.Airings, "took", time.Since(began).Round(time.Second))
	if len(stats.Unmatched) > 0 {
		log.Warn("Live TV: mapped channels the cloud lineup lists no station for were left out of its guide",
			"lineup", p.Lineup, "gridKeys", stats.Unmatched)
	}
	return 0
}

// errPlanMoved is a cached plan that is not the stamped one, or none.
var errPlanMoved = errors.New("the cached plan moved")

// readStampedPlan reads dvr's cached plan and checks it is the stamped one.
func readStampedPlan(ctx context.Context, conn *pgx.Conn, dvr, stamp string) (cloudPlan, error) {
	var p cloudPlan
	body, _, ok, err := cachedPlan(ctx, conn, dvr)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, fmt.Errorf("%w: no plan is cached for DVR %s", errPlanMoved, dvr)
	}
	if got := cloudPlanStamp(body); got != stamp {
		return p, fmt.Errorf("%w: the cached plan is %s", errPlanMoved, got)
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("read the plan: %w", err)
	}
	return p, nil
}

// guideBuildConfig reads guide-build's flags and the library database's
// settings, which the Job's pod has from the Plex pod's environment.
func guideBuildConfig(args []string) (dvr, stamp string, pg plexdb.Config, err error) {
	fs := newFlagSet()
	fs.String("dvr", "", "the DVR whose cloud guide to build")
	fs.String("stamp", "", "the stamp of the plan to build")
	if err := fs.Parse(args); err != nil {
		return "", "", pg, err
	}
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(fs); err != nil {
		return "", "", pg, fmt.Errorf("bind flags: %w", err)
	}
	dvr, stamp = strings.TrimSpace(v.GetString("dvr")), strings.TrimSpace(v.GetString("stamp"))
	if dvr == "" || stamp == "" {
		return "", "", pg, errors.New("--dvr and --stamp are required")
	}
	pg = plexdb.Config{
		Host:     v.GetString("postgres-host"),
		Port:     v.GetInt("postgres-port"),
		Database: v.GetString("postgres-database"),
		User:     v.GetString("postgres-user"),
		Password: v.GetString("postgres-password"),
		Schema:   v.GetString("postgres-schema"),
		PoolSize: v.GetInt("postgres-pool-size"),
		PoolMax:  v.GetInt("postgres-pool-max"),
		SSLMode:  v.GetString("postgres-sslmode"),
	}
	return dvr, stamp, pg, pg.Validate()
}
