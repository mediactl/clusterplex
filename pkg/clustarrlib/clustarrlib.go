// Package clustarrlib reads clustarr's library store: the Postgres tables
// clustarr keeps the kinds its controllers create in -- MediaFile, Episode
// and the rest -- since they stopped being custom resources (clustarr
// ADR-0024, docs/superpowers/specs/2026-11-26-library-store-design.md). The
// tables are the contract between the two repositories:
//
//	library.<kind>  (namespace, name, version, body, status, managed)
//	library.head    (kind, seq, floor)
//	library.change  (kind, seq, namespace, name, op, body, at)
//
// An object is body with status laid in under "status"; its version is the
// sequence number of the write that last touched it and is its
// resourceVersion. Writes to one kind commit in the order of their sequence
// numbers, so a reader that follows library.change by a cursor misses none.
// A write also notifies the channel "library" with the kind's name.
//
// Source turns one kind into a client-go ListerWatcher of unstructured
// objects, so an informer runs on it as it ran on the apiserver. This
// package only reads; clustarr's manager is the store's one writer.
package clustarrlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// objectSQL is a row's object as one JSON document.
const objectSQL = `CASE WHEN o.status IS NULL THEN o.body ELSE o.body || jsonb_build_object('status', o.status) END`

// window is how many sequence numbers one read of the feed spans.
const window = 1000

// DefaultPoll is how often a watch reads the feed with no notification to
// prompt it: what bounds a change's delay when its notification is lost.
const DefaultPoll = 15 * time.Second

// Source is one library kind in clustarr's store.
type Source struct {
	Pool *pgxpool.Pool
	// Kind is the kind's name (MediaFile) and Table its table in the
	// library schema (mediafile).
	Kind, Table string
	// Namespace restricts the source to one namespace; empty reads all.
	Namespace string
	// Poll is the fallback read of the feed; zero is DefaultPoll.
	Poll time.Duration
}

var _ cache.ListerWatcher = (*Source)(nil)

func (s *Source) object(body []byte, version int64) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &u.Object); err != nil {
		return nil, fmt.Errorf("clustarrlib: decode a %s: %w", s.Kind, err)
	}
	u.SetResourceVersion(strconv.FormatInt(version, 10))
	return u, nil
}

// List reads every object of the kind in one snapshot, with the kind's
// sequence at that snapshot as the list's resourceVersion.
func (s *Source) List(metav1.ListOptions) (runtime.Object, error) {
	ctx := context.Background()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var head int64
	if err := tx.QueryRow(ctx, `SELECT seq FROM library.head WHERE kind = $1`, s.Kind).Scan(&head); err != nil {
		return nil, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err)
	}
	query := `SELECT o.version, ` + objectSQL + ` FROM library.` + s.Table + ` o`
	var args []any
	if s.Namespace != "" {
		query += ` WHERE o.namespace = $1`
		args = append(args, s.Namespace)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err)
	}
	defer rows.Close()
	list := &unstructured.UnstructuredList{}
	for rows.Next() {
		var (
			version int64
			body    []byte
		)
		if err := rows.Scan(&version, &body); err != nil {
			return nil, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err)
		}
		u, err := s.object(body, version)
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err)
	}
	list.SetAPIVersion("catalog.clustarr.io/v1alpha1")
	list.SetKind(s.Kind + "List")
	list.SetResourceVersion(strconv.FormatInt(head, 10))
	return list, nil
}

// change is one object's state at the end of a window of the feed.
type change struct {
	typ     watch.EventType
	body    []byte
	version int64
}

// changes reads the feed after cursor: at most one change per object, in
// the order of the objects' versions, and the sequence number the read
// reached. An object changed again beyond the window is left for the read
// that reaches that change, so a watch that resumes from any version it was
// given misses nothing. It answers a ResourceExpired error when cursor is
// below what clustarr has trimmed: the informer lists again.
func (s *Source) changes(ctx context.Context, cursor int64) ([]change, int64, error) {
	rows, err := s.Pool.Query(ctx, `
WITH h AS (SELECT seq, floor FROM library.head WHERE kind = $1),
w AS (
	SELECT DISTINCT ON (namespace, name) seq, namespace, name, op, body
	FROM library.change
	WHERE kind = $1 AND seq > $2 AND seq <= $2 + $3 AND ($4 = '' OR namespace = $4)
	ORDER BY namespace, name, seq DESC
)
SELECT h.seq, h.floor, w.seq, w.op::text, w.body, o.version, `+objectSQL+`
FROM h LEFT JOIN w ON true
LEFT JOIN library.`+s.Table+` o ON o.namespace = w.namespace AND o.name = w.name
ORDER BY w.seq`, s.Kind, cursor, window, s.Namespace)
	if err != nil {
		return nil, 0, fmt.Errorf("clustarrlib: read the %s feed: %w", s.Kind, err)
	}
	defer rows.Close()
	var (
		out         []change
		head, floor int64
	)
	for rows.Next() {
		var (
			seq, version *int64
			op           *string
			last, body   []byte
		)
		if err := rows.Scan(&head, &floor, &seq, &op, &last, &version, &body); err != nil {
			return nil, 0, fmt.Errorf("clustarrlib: read the %s feed: %w", s.Kind, err)
		}
		if seq == nil {
			continue
		}
		switch {
		case version != nil && *version == *seq:
			typ := watch.Modified
			if op != nil && *op == "A" {
				typ = watch.Added
			}
			out = append(out, change{typ: typ, body: body, version: *version})
		case op != nil && *op == "D" && version == nil:
			out = append(out, change{typ: watch.Deleted, body: last, version: *seq})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("clustarrlib: read the %s feed: %w", s.Kind, err)
	}
	if cursor < floor {
		return nil, 0, apierrors.NewResourceExpired(fmt.Sprintf("clustarrlib: the %s feed is trimmed up to %d; %d is too old", s.Kind, floor, cursor))
	}
	next := min(head, cursor+window)
	if next < cursor {
		return nil, 0, apierrors.NewResourceExpired(fmt.Sprintf("clustarrlib: the %s feed is at %d, behind the reader's %d", s.Kind, head, cursor))
	}
	return out, next, nil
}

// Watch follows the feed after opts.ResourceVersion. Asked to send its
// initial events (a reflector's streamed list), it first sends every object
// as it stands in one snapshot, one row decoded at a time, and a bookmark
// that ends them at the snapshot's sequence number: the informer's
// transform trims each object as it arrives, where a list would hold the
// whole kind decoded first.
func (s *Source) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	initial := opts.SendInitialEvents != nil && *opts.SendInitialEvents
	ctx, cancel := context.WithCancel(context.Background())
	var cursor int64
	if !initial {
		var err error
		if cursor, err = strconv.ParseInt(opts.ResourceVersion, 10, 64); err != nil {
			cancel()
			return nil, apierrors.NewBadRequest(fmt.Sprintf("clustarrlib: resourceVersion %q is not a sequence number", opts.ResourceVersion))
		}
		if _, _, err := s.changes(ctx, cursor); err != nil {
			cancel()
			return nil, err
		}
	}
	w := &watcher{src: s, result: make(chan watch.Event), cancel: cancel}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		defer close(w.result)
		if initial {
			var ok bool
			if cursor, ok = w.sendInitial(ctx); !ok {
				return
			}
		}
		w.follow(ctx, cursor)
	}()
	return w, nil
}

type watcher struct {
	src    *Source
	result chan watch.Event
	cancel context.CancelFunc
	done   sync.WaitGroup
}

func (w *watcher) ResultChan() <-chan watch.Event { return w.result }

func (w *watcher) Stop() {
	w.cancel()
	go func() {
		for range w.result { //nolint:revive // draining, so the producer is never left blocked
		}
	}()
	w.done.Wait()
}

func (w *watcher) send(ctx context.Context, e watch.Event) bool {
	select {
	case w.result <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *watcher) fail(ctx context.Context, err error) {
	status := apierrors.NewInternalError(err).Status()
	var apiErr apierrors.APIStatus
	if errors.As(err, &apiErr) {
		status = apiErr.Status()
	}
	w.send(ctx, watch.Event{Type: watch.Error, Object: &status})
}

// sendInitial streams the kind as it stands in one snapshot, then the
// bookmark that ends the initial events.
func (w *watcher) sendInitial(ctx context.Context) (int64, bool) {
	s := w.src
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		w.fail(ctx, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err))
		return 0, false
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var head int64
	if err := tx.QueryRow(ctx, `SELECT seq FROM library.head WHERE kind = $1`, s.Kind).Scan(&head); err != nil {
		w.fail(ctx, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err))
		return 0, false
	}
	query := `SELECT o.version, ` + objectSQL + ` FROM library.` + s.Table + ` o`
	var args []any
	if s.Namespace != "" {
		query += ` WHERE o.namespace = $1`
		args = append(args, s.Namespace)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		w.fail(ctx, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err))
		return 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			version int64
			body    []byte
		)
		if err := rows.Scan(&version, &body); err != nil {
			w.fail(ctx, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err))
			return 0, false
		}
		u, err := s.object(body, version)
		if err != nil {
			w.fail(ctx, err)
			return 0, false
		}
		if !w.send(ctx, watch.Event{Type: watch.Added, Object: u}) {
			return 0, false
		}
	}
	if err := rows.Err(); err != nil {
		w.fail(ctx, fmt.Errorf("clustarrlib: list %s: %w", s.Kind, err))
		return 0, false
	}
	mark := &unstructured.Unstructured{}
	mark.SetAPIVersion("catalog.clustarr.io/v1alpha1")
	mark.SetKind(s.Kind)
	mark.SetResourceVersion(strconv.FormatInt(head, 10))
	mark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	return head, w.send(ctx, watch.Event{Type: watch.Bookmark, Object: mark})
}

func (w *watcher) follow(ctx context.Context, cursor int64) {
	s := w.src
	wake := s.listen(ctx)
	poll := s.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		for {
			changes, next, err := s.changes(ctx, cursor)
			if err != nil {
				if ctx.Err() == nil {
					w.fail(ctx, err)
				}
				return
			}
			for _, c := range changes {
				u, err := s.object(c.body, c.version)
				if err != nil {
					w.fail(ctx, err)
					return
				}
				if !w.send(ctx, watch.Event{Type: c.typ, Object: u}) {
					return
				}
			}
			if next == cursor {
				break
			}
			cursor = next
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// listen answers a channel signalled when clustarr notifies of a write to
// the kind. It holds one connection until ctx ends and reconnects when the
// connection is lost; a notification is only a wake-up, and the poll covers
// one that is missed.
func (s *Source) listen(ctx context.Context) <-chan struct{} {
	wake := make(chan struct{}, 1)
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go func() {
		for ctx.Err() == nil {
			func() {
				conn, err := s.Pool.Acquire(ctx)
				if err != nil {
					return
				}
				defer conn.Release()
				if _, err := conn.Exec(ctx, `LISTEN library`); err != nil {
					return
				}
				// The session's LISTEN ends with it, so the connection is
				// not handed back to the pool listening.
				defer func() { _, _ = conn.Exec(context.Background(), `UNLISTEN library`) }()
				signal()
				for {
					n, err := conn.Conn().WaitForNotification(ctx)
					if err != nil {
						return
					}
					if n.Payload == s.Kind {
						signal()
					}
				}
			}()
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return wake
}

// Informer is an informer of the kind's unstructured objects, indexed by
// namespace as a dynamic informer is.
func (s *Source) Informer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(s, &unstructured.Unstructured{}, 0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}
