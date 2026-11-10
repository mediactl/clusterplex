package epgcloud

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// budgetTable holds one row per budget: the moment its next call may go.
const budgetTable = CacheSchema + ".epg_budget"

// Budget paces every pod's calls with one token at one rate, kept in
// PostgreSQL: each call claims the next free slot from the budget's row, so
// the pods together never pass the rate whatever each believes about the
// others, and a refusal's Retry-After (Hold) holds them all. A pod that
// stops waiting for a slot it claimed leaves it unused, which errs slow.
//
// Q is used only inside Wait and Hold, from the caller's goroutine: a
// *pgx.Conn the caller uses for nothing else meanwhile, or a pool.
type Budget struct {
	Q Querier
	// Key names the budget: one per token.
	Key string
	// Interval is the gap between two calls: one over the rate.
	Interval time.Duration
}

// EnsureBudget makes the budget's table.
func EnsureBudget(ctx context.Context, q Querier) error {
	for _, s := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + CacheSchema,
		"CREATE TABLE IF NOT EXISTS " + budgetTable + " (key text PRIMARY KEY, next_at timestamptz NOT NULL)",
	} {
		if _, err := q.Exec(ctx, s); err != nil {
			return fmt.Errorf("epgcloud: make the budget: %w", err)
		}
	}
	return nil
}

// Wait implements Limiter: it claims the budget's next slot and sleeps
// until it. The slot and the wait are the server's arithmetic, on the
// server's clock, so the pods' clocks need not agree.
func (b *Budget) Wait(ctx context.Context) error {
	if b.Q == nil || b.Key == "" || b.Interval <= 0 {
		return errors.New("epgcloud: a budget needs a querier, a key and an interval")
	}
	var wait time.Duration
	err := b.Q.QueryRow(ctx, `INSERT INTO `+budgetTable+` AS b (key, next_at) VALUES ($1, clock_timestamp() + $2::interval)
		ON CONFLICT (key) DO UPDATE SET next_at = greatest(b.next_at, clock_timestamp()) + $2::interval
		RETURNING greatest(b.next_at - $2::interval - clock_timestamp(), '0'::interval)`, b.Key, b.Interval).Scan(&wait)
	if err != nil {
		return fmt.Errorf("epgcloud: claim a slot of budget %s: %w", b.Key, err)
	}
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Hold holds every pod's calls not yet waiting for d from now; a call
// already waiting for its slot still goes at it.
func (b *Budget) Hold(ctx context.Context, d time.Duration) error {
	_, err := b.Q.Exec(ctx, `INSERT INTO `+budgetTable+` AS b (key, next_at) VALUES ($1, clock_timestamp() + $2::interval)
		ON CONFLICT (key) DO UPDATE SET next_at = greatest(b.next_at, clock_timestamp() + $2::interval)`, b.Key, d)
	if err != nil {
		return fmt.Errorf("epgcloud: hold budget %s: %w", b.Key, err)
	}
	return nil
}
