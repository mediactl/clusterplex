package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mediactl/clusterplex/pkg/maintenance"
)

// MaintenancePrefix is where a CronJob asks for work to be distributed.
const MaintenancePrefix = "/api/v1/maintenance/"

// servingAnnotation marks a pod whose Plex is accepting connections. The
// maintenance fan-out needs the set of pods that can actually do work, which
// in elected mode is one pod and in active mode is all of them, so it cannot
// come from the Lease: that names a single holder.
const servingAnnotation = "clusterplex.io/plex-serving"

const maintenanceTimeout = 30 * time.Second

// registerMaintenance adds the fan-out endpoint. Scheduling lives in
// Kubernetes CronJobs rather than in this process, so this only ever reacts.
func (m *Manager) registerMaintenance(mux *http.ServeMux) {
	mux.HandleFunc(MaintenancePrefix, m.handleMaintenance)
}

func (m *Manager) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, MaintenancePrefix)
	task, ok := maintenance.Lookup(name)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown task %q; known tasks: %s",
			name, strings.Join(maintenance.Names(), ", ")), http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), maintenanceTimeout)
	defer cancel()

	result, err := m.fanout().Run(ctx, task)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		// The caller is a Job. Failing loudly is the point: it retries, and
		// the failure is visible as a failed Job rather than lost in a log.
		m.Logger.Error("maintenance fan-out failed", "task", name, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task": name, "error": err.Error(),
			"dispatched": len(result.Dispatched), "failed": len(result.Failed),
		})
		return
	}

	m.Logger.Info("maintenance dispatched", "task", name, "dispatched", len(result.Dispatched))
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task": name, "dispatched": result.Dispatched,
	})
}

func (m *Manager) fanout() *maintenance.Fanout {
	return &maintenance.Fanout{
		Libraries: m.plexLibraries,
		Members:   m.podsServingPlex,
		Dispatch:  m.dispatchToPlex,
		Logger:    m.Logger.With("component", "maintenance"),
	}
}

// sections is the shape of Plex's library listing.
type sections struct {
	Directory []struct {
		Key string `xml:"key,attr"`
	} `xml:"Directory"`
}

// plexLibraries asks the local Plex for its library sections.
func (m *Manager) plexLibraries(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.plexAddr+"/library/sections", nil)
	if err != nil {
		return nil, err
	}
	m.authenticate(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list libraries: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list libraries: %s", resp.Status)
	}

	var parsed sections
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse libraries: %w", err)
	}
	ids := make([]string, 0, len(parsed.Directory))
	for _, d := range parsed.Directory {
		if d.Key != "" {
			ids = append(ids, d.Key)
		}
	}
	return ids, nil
}

// podsServingPlex lists the pods whose Plex is accepting connections.
func (m *Manager) podsServingPlex(ctx context.Context) ([]string, error) {
	pods, err := m.K8sClient.CoreV1().Pods(m.Config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=plex",
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var out []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations[servingAnnotation] != "true" || p.Status.PodIP == "" || p.Status.Phase != corev1.PodRunning {
			continue
		}
		out = append(out, net.JoinHostPort(p.Status.PodIP, strconv.Itoa(m.Config.PMSPort)))
	}
	return out, nil
}

// dispatchToPlex asks one pod's Plex to do one piece of work. It reaches that
// pod's proxy, which forwards into the namespace where Plex is listening.
func (m *Manager) dispatchToPlex(ctx context.Context, member, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+member+path, nil)
	if err != nil {
		return err
	}
	m.authenticate(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return nil
}

// authenticate adds Plex's local admin token, which Plex writes to its own
// state directory for exactly this purpose: administering a claimed server
// from the machine it runs on.
func (m *Manager) authenticate(req *http.Request) {
	if token := m.localAdminToken(); token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	req.Header.Set("Accept", "application/xml")
}

// localAdminToken reads the token Plex writes into its own state directory so
// that the machine it runs on can administer it. Reading it per call keeps us
// honest about Plex rewriting it, and these calls are rare.
func (m *Manager) localAdminToken() string {
	b, err := os.ReadFile(filepath.Join(m.Config.PlexDir, ".LocalAdminToken"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// markServingPlex records on this pod whether its Plex is accepting work.
func (m *Manager) markServingPlex(ctx context.Context, serving bool) error {
	payload := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`,
		servingAnnotation, strconv.FormatBool(serving))
	_, err := m.patchPod(ctx, payload)
	return err
}
