package epgcloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// budgetDB is two connections to a fresh database: two pods. It skips
// without CLUSTERPLEX_TEST_POSTGRES_DSN (pkg/plex/bootstrap's
// postgres_test.go says how to run a server).
func budgetDB(t *testing.T) (*pgx.Conn, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "budget_" + hex.EncodeToString(b[:])
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.Database = name
	var conns []*pgx.Conn
	for range 2 {
		c, err := pgx.ConnectConfig(ctx, cfg)
		require.NoError(t, err)
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close(context.Background())
		}
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	require.NoError(t, EnsureBudget(ctx, conns[0]))
	return conns[0], conns[1]
}

// TestEveryPodsCallsShareOneRate: two pods calling at once go no faster
// together than the budget's rate, whatever each would do alone.
func TestEveryPodsCallsShareOneRate(t *testing.T) {
	a, b := budgetDB(t)
	const interval = 100 * time.Millisecond
	ctx := context.Background()
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var at []time.Duration
	for _, conn := range []*pgx.Conn{a, b} {
		wg.Go(func() {
			bud := &Budget{Q: conn, Key: "token", Interval: interval}
			for range 3 {
				if !assert.NoError(t, bud.Wait(ctx)) {
					return
				}
				mu.Lock()
				at = append(at, time.Since(start))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	require.Len(t, at, 6)
	// Six calls at 10 a second: the sixth goes no sooner than 500 ms in.
	assert.GreaterOrEqual(t, time.Since(start), 5*interval-20*time.Millisecond)
}

// TestARefusalHoldsEveryPod: one pod's Hold, after the service refused it,
// holds the other pod's next call too.
func TestARefusalHoldsEveryPod(t *testing.T) {
	a, b := budgetDB(t)
	ctx := context.Background()
	require.NoError(t, (&Budget{Q: a, Key: "token", Interval: time.Millisecond}).Hold(ctx, 400*time.Millisecond))
	start := time.Now()
	require.NoError(t, (&Budget{Q: b, Key: "token", Interval: time.Millisecond}).Wait(ctx))
	assert.GreaterOrEqual(t, time.Since(start), 350*time.Millisecond)

	start = time.Now()
	require.NoError(t, (&Budget{Q: b, Key: "another token", Interval: time.Millisecond}).Wait(ctx))
	assert.Less(t, time.Since(start), 200*time.Millisecond, "a budget holds only its own key")
}

// TestABudgetWaitEndsWithItsContext.
func TestABudgetWaitEndsWithItsContext(t *testing.T) {
	a, _ := budgetDB(t)
	ctx := context.Background()
	bud := &Budget{Q: a, Key: "token", Interval: time.Millisecond}
	require.NoError(t, bud.Hold(ctx, time.Hour))
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, bud.Wait(short), context.DeadlineExceeded)
	require.Error(t, (&Budget{Key: "token", Interval: time.Second}).Wait(ctx), "a budget without a querier")
}
