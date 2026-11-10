package epgcloud

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The cache keeps the service's answers in the library's database, in
// cluster-plex's own schema, shared by every pod: the pod that fetched an
// answer and the lease holder that builds from it need not be one.
const (
	CacheSchema = "clusterplex"
	cacheTable  = CacheSchema + ".epg_cache"
)

// Querier is a connection or a pool.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Cache keys.
func LineupKey(lineup string) string      { return "lineup/" + lineup }
func MetadataKey(ratingKey string) string { return "metadata/" + ratingKey }
func GridKey(gridKey string, start, end time.Time) string {
	return "grid/" + gridKey + "/" + start.UTC().Format(time.DateOnly) + "/" + end.UTC().Format(time.DateOnly)
}

// EnsureCache makes the cache's schema and table.
func EnsureCache(ctx context.Context, q Querier) error {
	for _, s := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + CacheSchema,
		"CREATE TABLE IF NOT EXISTS " + cacheTable + " (key text PRIMARY KEY, fetched_at timestamptz NOT NULL, body bytea NOT NULL)",
	} {
		if _, err := q.Exec(ctx, s); err != nil {
			return fmt.Errorf("epgcloud: make the cache: %w", err)
		}
	}
	return nil
}

// CacheGet is an answer and when it was fetched; ok false when there is none.
func CacheGet(ctx context.Context, q Querier, key string) (body []byte, fetched time.Time, ok bool, err error) {
	err = q.QueryRow(ctx, "SELECT body, fetched_at FROM "+cacheTable+" WHERE key = $1", key).Scan(&body, &fetched)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	return body, fetched, err == nil, err
}

// CacheFresh is the cached answer when it is younger than ttl.
func CacheFresh(ctx context.Context, q Querier, key string, ttl time.Duration, now time.Time) ([]byte, bool, error) {
	body, fetched, ok, err := CacheGet(ctx, q, key)
	if err != nil || !ok || now.Sub(fetched) >= ttl {
		return nil, false, err
	}
	return body, true, nil
}

// CacheStale counts the keys the cache holds no answer for, or one at least
// ttl old (CacheFresh's test), in one query.
func CacheStale(ctx context.Context, q Querier, keys []string, ttl time.Duration, now time.Time) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	var n int
	err := q.QueryRow(ctx, "SELECT count(*) FROM unnest($1::text[]) AS k(key) LEFT JOIN "+cacheTable+
		" c USING (key) WHERE c.key IS NULL OR c.fetched_at <= $2", keys, now.Add(-ttl)).Scan(&n)
	return n, err
}

// CachePut stores an answer.
func CachePut(ctx context.Context, q Querier, key string, body []byte, fetched time.Time) error {
	_, err := q.Exec(ctx, "INSERT INTO "+cacheTable+" (key, fetched_at, body) VALUES ($1, $2, $3) "+
		"ON CONFLICT (key) DO UPDATE SET fetched_at = EXCLUDED.fetched_at, body = EXCLUDED.body", key, fetched, body)
	return err
}

// CacheList is every entry's key under prefix and when it was fetched.
func CacheList(ctx context.Context, q Querier, prefix string) (map[string]time.Time, error) {
	rows, err := q.Query(ctx, "SELECT key, fetched_at FROM "+cacheTable+" WHERE key LIKE $1", prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var k string
		var at time.Time
		if err := rows.Scan(&k, &at); err != nil {
			return nil, err
		}
		out[k] = at
	}
	return out, rows.Err()
}

// CachePrune drops entries fetched before cutoff: grids of days gone.
func CachePrune(ctx context.Context, q Querier, prefix string, cutoff time.Time) error {
	_, err := q.Exec(ctx, "DELETE FROM "+cacheTable+" WHERE key LIKE $1 AND fetched_at < $2", prefix+"%", cutoff)
	return err
}
