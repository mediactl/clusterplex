package plexdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool adapts a pgx connection pool to Querier. It exists so the rest of the
// package, and its tests, need no driver.
type Pool struct {
	pool *pgxpool.Pool
}

// Open connects to the library database. The connection is lazy, so this
// succeeds even if the database is not up yet and the first query is what
// reports a problem.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", cfg, err)
	}
	return &Pool{pool: pool}, nil
}

// QueryRow implements Querier, translating pgx's own no-rows error into ours.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return row{p.pool.QueryRow(ctx, sql, args...)}
}

// Ping checks the database is actually reachable.
func (p *Pool) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Close releases the pool.
func (p *Pool) Close() { p.pool.Close() }

type row struct{ inner pgx.Row }

func (r row) Scan(dest ...any) error {
	err := r.inner.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoRows
	}
	return err
}

// Exec runs a statement for its effect. The schema dump is sent through here
// as one string, which pgx allows because it uses the simple protocol for a
// query with no parameters.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.pool.Exec(ctx, sql, args...)
	return err
}

// Count runs a query returning a single integer.
func (p *Pool) Count(ctx context.Context, sql string, args ...any) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Strings runs a query returning one text column.
func (p *Pool) Strings(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
