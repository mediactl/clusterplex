package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/mediaproxy"
	"github.com/mediactl/clusterplex/pkg/plexdb"
)

// authProbeTimeout bounds the authorization round trip to Plex.
const authProbeTimeout = 10 * time.Second

// resolveHandler answers the proxy's question: may this caller have this media,
// and which file is it?
//
// Authorization is delegated to Plex rather than reimplemented: the original
// request is replayed against Plex asking for a single byte, so whatever Plex
// would have decided is what the proxy gets. The path then comes from the
// library database. Only the answer crosses the network; the media itself is
// served by the proxy from shared storage.
type resolveHandler struct {
	PMSBase string
	DB      *plexdb.DB
	Client  *http.Client
}

func (h *resolveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	partID := r.URL.Query().Get("part")
	path := r.URL.Query().Get("path")
	if partID == "" || !strings.HasPrefix(path, "/") {
		http.Error(w, "part and path are required", http.StatusBadRequest)
		return
	}

	switch allowed, err := h.authorize(r, path); {
	case err != nil:
		http.Error(w, "cannot check authorization: "+err.Error(), http.StatusBadGateway)
		return
	case !allowed:
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	file, err := h.DB.PartFile(r.Context(), partID)
	if errors.Is(err, plexdb.ErrNotFound) {
		http.Error(w, "unknown part", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"file": file})
}

// authorize replays the caller's request against Plex for one byte. Plex's
// answer is the authorization decision.
func (h *resolveHandler) authorize(r *http.Request, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), authProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.PMSBase+path, nil)
	if err != nil {
		return false, err
	}
	for name, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-plex") {
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
	}
	req.Header.Set("Range", "bytes=0-0")

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: authProbeTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("ask Plex: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent, nil
}

// register adds the resolve endpoint to the manager's HTTP server.
func (m *Manager) registerResolve(mux *http.ServeMux) {
	mux.Handle(mediaproxy.ResolvePath, &resolveHandler{
		PMSBase: fmt.Sprintf("http://127.0.0.1:%d", m.Config.PMSPort),
		DB:      m.DB,
	})
}
