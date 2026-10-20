package clustarrwatch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// Plex removes no gone file on an item's refresh, and the proxy, which
// sees only plain HTTP, misses the refreshes TLS clients ask for. Plex's
// own notification stream reports every refresh from every client: a
// library timeline entry for the item, ending in state 5. The watcher
// follows it and sweeps the item once the burst is over.

const (
	// libraryIdentifier names the library's timeline entries.
	libraryIdentifier = "com.plexapp.plugins.library"
	// timelineDone is the state of an entry Plex finished with.
	timelineDone = 5
	// defaultSweepDelay is SweepDelay's default.
	defaultSweepDelay = 10 * time.Second
	// sweepTimeout bounds one item's sweep: a few files.
	sweepTimeout = time.Minute
	// maxStreamBackoff caps the wait between reconnects to the stream.
	maxStreamBackoff = 30 * time.Second
)

// sweepKey is an item Plex refreshed in a section.
type sweepKey struct {
	section string
	item    int64
}

// readRefreshes reads Plex's event stream and calls done for each library
// item Plex finished with, by section key and item id, until the stream
// ends.
func readRefreshes(r io.Reader, done func(section string, item int64)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			event = ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && event == "timeline":
			var msg struct {
				TimelineEntry struct {
					Identifier string `json:"identifier"`
					SectionID  string `json:"sectionID"`
					ItemID     string `json:"itemID"`
					State      int    `json:"state"`
				}
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &msg) != nil {
				continue
			}
			e := msg.TimelineEntry
			if e.Identifier != libraryIdentifier || e.State != timelineDone {
				continue
			}
			if id, err := strconv.ParseInt(e.ItemID, 10, 64); err == nil && id > 0 {
				done(e.SectionID, id)
			}
		}
	}
	return sc.Err()
}

// followRefreshes keeps a connection to Plex's notification stream until
// ctx ends, reconnecting on a backoff when Plex closes it or is not up.
func (w *Watcher) followRefreshes(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := w.streamRefreshes(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		w.log().Debug("Plex's notification stream ended; reconnecting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxStreamBackoff)
	}
}

// streamRefreshes reads one connection to the stream; connected reports
// whether Plex answered it.
func (w *Watcher) streamRefreshes(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(w.PMS.BaseURL, "/")+"/:/eventsource/notifications?filters=timeline", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if w.PMS.Token != nil {
		if t := w.PMS.Token(); t != "" {
			req.Header.Set("X-Plex-Token", t)
		}
	}
	// No client timeout: the stream is meant to stay open.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("the notification stream answered %s", resp.Status)
	}
	return true, readRefreshes(resp.Body, w.enqueueSweep)
}

// enqueueSweep queues the sweep of an item Plex refreshed in a section the
// projection owns, due SweepDelay after the last refresh of it.
func (w *Watcher) enqueueSweep(section string, item int64) {
	if !w.projectsKey(section) {
		return
	}
	delay := w.SweepDelay
	if delay <= 0 {
		delay = defaultSweepDelay
	}
	w.projMu.Lock()
	defer w.projMu.Unlock()
	if w.sweeps == nil {
		w.sweeps = map[sweepKey]time.Time{}
	}
	w.sweeps[sweepKey{section, item}] = time.Now().Add(delay)
}

// flushSweeps sweeps every queued item that is due, on its section's lock.
func (w *Watcher) flushSweeps(ctx context.Context) {
	now := time.Now()
	var due []sweepKey
	w.projMu.Lock()
	for k, at := range w.sweeps {
		if !now.Before(at) {
			due = append(due, k)
			delete(w.sweeps, k)
		}
	}
	w.projMu.Unlock()
	for _, k := range due {
		ids, ok := w.provisioned(k.section)
		if !ok {
			continue
		}
		lock := w.sectionLock(k.section)
		lock.Lock()
		sctx, cancel := context.WithTimeout(ctx, sweepTimeout)
		w.sweep(sctx, k.section, ids.Section, plexproject.SweepScope{Item: k.item})
		cancel()
		lock.Unlock()
	}
}
