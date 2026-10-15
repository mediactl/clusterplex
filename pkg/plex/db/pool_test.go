package db

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// Query returns every row of a statement, for the reads one row at a time
// cannot serve: the projection's reconcile lists a whole section.
func TestQueryReturnsEveryRow(t *testing.T) {
	dsn := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	p, err := Open(context.Background(), Config{
		Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database,
		User: cfg.User, Password: cfg.Password, Schema: "public",
	})
	require.NoError(t, err)
	t.Cleanup(p.Close)

	rows, err := p.Query(context.Background(), "SELECT n FROM generate_series(1, 3) AS g(n) ORDER BY n")
	require.NoError(t, err)
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		require.NoError(t, rows.Scan(&n))
		got = append(got, n)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []int{1, 2, 3}, got)
}
