package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mediactl/clusterplex/pkg/lease"
	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// ScanPath is where a library scan of a provisioned section is asked for
// (spec 2026-10-15 §5, as amended): the proxy's divert sends Plex's own
// scan request here, and a CronJob or an operator may call it directly.
// The reconcile runs on the Lease holder, which has the clustarr watch;
// any other pod forwards to it.
const ScanPath = "/api/v1/scan"

// forwardedHeader marks a scan forwarded from another pod, so a pod that
// is not the holder either (the Lease moved meanwhile) answers 409 rather
// than forwarding again.
const forwardedHeader = "X-ClusterPlex-Forwarded"

const scanTimeout = 10 * time.Minute

// sectionReconciler is what the holder's watcher offers (ReconcileSection).
type sectionReconciler interface {
	ReconcileSection(ctx context.Context, key, folder string) (plexproject.Outcome, bool, error)
}

func (m *Manager) registerScan(mux *http.ServeMux) {
	mux.HandleFunc(ScanPath, m.handleScan)
}

func (m *Manager) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	section := r.URL.Query().Get("section")
	folder := r.URL.Query().Get("path")
	if section == "" {
		http.Error(w, "section is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()
	if !m.provisionedKeys(ctx)[section] {
		http.Error(w, fmt.Sprintf("section %s is not provisioned; Plex scans it", section), http.StatusNotFound)
		return
	}
	if m.holdsLease() {
		rec := m.sectionReconciler()
		if rec == nil {
			http.Error(w, "the clustarr watch is not running yet", http.StatusServiceUnavailable)
			return
		}
		out, ok, err := rec.ReconcileSection(ctx, section, folder)
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !ok:
			http.Error(w, fmt.Sprintf("section %s is not provisioned; Plex scans it", section), http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		}
		return
	}
	if r.Header.Get(forwardedHeader) != "" {
		http.Error(w, "this pod does not hold the lease (it moved); try again", http.StatusConflict)
		return
	}
	addr, err := m.holderAPI(ctx)
	if err != nil {
		http.Error(w, "no lease holder to run the scan: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	status, body, err := m.forwardScan(ctx, addr, r.URL.RawQuery)
	if err != nil {
		http.Error(w, "forward the scan to the lease holder: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// holdsLease reports whether this pod runs the leader-only work.
func (m *Manager) holdsLease() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stopLeader != nil
}

func (m *Manager) sectionReconciler() sectionReconciler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.reconciler != nil {
		return m.reconciler
	}
	if m.watcher != nil {
		return m.watcher
	}
	return nil
}

// holderAPI is the probe-port base URL of the pod the Lease names.
func (m *Manager) holderAPI(ctx context.Context) (string, error) {
	e := &lease.Elector{Client: m.K8sClient, Namespace: m.Config.Namespace, Name: m.Config.LeaseName, Identity: m.Config.PodName}
	holder, err := e.Holder(ctx)
	if err != nil {
		return "", err
	}
	if holder == "" {
		return "", errors.New("the lease is free")
	}
	pod, err := m.K8sClient.CoreV1().Pods(m.Config.Namespace).Get(ctx, holder, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("holder %s: %w", holder, err)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("holder %s has no IP yet", holder)
	}
	return "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(m.Config.ProbePort)), nil
}

func (m *Manager) forwardScan(ctx context.Context, addr, rawQuery string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+ScanPath+"?"+rawQuery, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set(forwardedHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// provisionedKeys is the set of section keys the projection owns: the
// holder's record from provisioning, or, on a pod that never provisioned,
// the configured library names resolved through Plex's section list,
// cached for keysTTL. A pod with neither knows none.
func (m *Manager) provisionedKeys(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for key := range m.provisionedSections() {
		out[key] = true
	}
	if len(out) > 0 {
		return out
	}
	names := m.Config.Provision.LibraryNames()
	if len(names) == 0 {
		return out
	}
	m.keysMu.Lock()
	defer m.keysMu.Unlock()
	if time.Since(m.keysAt) < keysTTL && m.keys != nil {
		return m.keys
	}
	sections, err := m.pms().Sections(ctx)
	if err != nil {
		return out
	}
	for _, s := range sections {
		for _, n := range names {
			if s.Title == n {
				out[s.Key] = true
			}
		}
	}
	m.keys, m.keysAt = out, time.Now()
	return out
}

const keysTTL = 5 * time.Minute

// keysCache is the provisionedKeys cache a non-holder keeps.
type keysCache struct {
	keysMu sync.Mutex
	keys   map[string]bool
	keysAt time.Time
}

// sectionOf is a metadata item's section key, for the analyze route.
func (m *Manager) sectionOf(ctx context.Context, item int64) (string, bool) {
	if m.pool == nil {
		return "", false
	}
	var section int64
	if err := m.pool.QueryRow(ctx, `SELECT library_section_id FROM metadata_items WHERE id = $1`, item).Scan(&section); err != nil {
		return "", false
	}
	return strconv.FormatInt(section, 10), true
}
