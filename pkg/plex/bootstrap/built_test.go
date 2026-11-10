package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildItems is a BuildFunc writing n items (ids 1..n) into staging.
func buildItems(n int) BuildFunc {
	return func(ctx context.Context, tx pgx.Tx, staging string) error {
		_, err := tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{staging, "metadata_items"}.Sanitize()+
			" (id, metadata_type, title) SELECT g, 1, 'item ' || g FROM generate_series(1, $1) g", n)
		return err
	}
}

func count(t *testing.T, conn *pgx.Conn, schema, table string) int {
	t.Helper()
	return one[int](t, conn, "SELECT count(*) FROM "+pgx.Identifier{schema, table}.Sanitize())
}

// TestAStagedBuildIsInstalledOnlyUnderItsStamp: StageBuilt leaves the live
// guide as it is and records the build; SwapStaged installs only the
// stamp it is given, and clears the record.
func TestAStagedBuildIsInstalledOnlyUnderItsStamp(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	g := testGuide
	require.NoError(t, InstallBuilt(ctx, quiet, conn, "plex", g, buildItems(2)))
	require.Equal(t, 2, count(t, conn, g.Schema(), "metadata_items"))
	stamp, _, err := StagedBuild(ctx, conn, g)
	require.NoError(t, err)
	assert.Empty(t, stamp, "an in-process install leaves no record")

	require.NoError(t, StageBuilt(ctx, quiet, conn, "plex", g, "plan-a", buildItems(5)))
	stamp, _, err = StagedBuild(ctx, conn, g)
	require.NoError(t, err)
	assert.Equal(t, "plan-a", stamp)
	assert.Equal(t, 2, count(t, conn, g.Schema(), "metadata_items"), "staging leaves the live guide alone")
	assert.Equal(t, 5, count(t, conn, g.Staging(), "metadata_items"))

	err = SwapStaged(ctx, quiet, conn, "plex", g, "plan-b")
	require.ErrorIs(t, err, ErrNotStaged)
	assert.Equal(t, 2, count(t, conn, g.Schema(), "metadata_items"), "another stamp installs nothing")

	require.NoError(t, SwapStaged(ctx, quiet, conn, "plex", g, "plan-a"))
	assert.Equal(t, 5, count(t, conn, g.Schema(), "metadata_items"))
	stamp, _, err = StagedBuild(ctx, conn, g)
	require.NoError(t, err)
	assert.Empty(t, stamp, "an installed build's record is cleared")
	assert.False(t, one[bool](t, conn, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", g.Staging()))
	require.ErrorIs(t, SwapStaged(ctx, quiet, conn, "plex", g, "plan-a"), ErrNotStaged, "a build installs once")
}

// TestAFailedStageLeavesNoBuildToInstall: a build that fails clears the
// record of the build it replaced and drops its staging schema, so no
// swap installs a half-written guide or an older one.
func TestAFailedStageLeavesNoBuildToInstall(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	g := testGuide
	require.NoError(t, StageBuilt(ctx, quiet, conn, "plex", g, "plan-a", buildItems(3)))
	boom := errors.New("boom")
	err := StageBuilt(ctx, quiet, conn, "plex", g, "plan-b", func(ctx context.Context, tx pgx.Tx, staging string) error {
		if err := buildItems(4)(ctx, tx, staging); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	stamp, _, err := StagedBuild(ctx, conn, g)
	require.NoError(t, err)
	assert.Empty(t, stamp)
	assert.False(t, one[bool](t, conn, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", g.Staging()))
	require.ErrorIs(t, SwapStaged(ctx, quiet, conn, "plex", g, "plan-a"), ErrNotStaged)
}

// TestTheSwapCarriesWhatAPersonDidWhileTheBuildRan: playlists and viewing
// rows are taken from the live guide at the swap, not at the build, so
// one made between the two is in the new guide, numbered past its items.
func TestTheSwapCarriesWhatAPersonDidWhileTheBuildRan(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	g := testGuide
	require.NoError(t, InstallBuilt(ctx, quiet, conn, "plex", g, buildItems(2)))
	require.NoError(t, StageBuilt(ctx, quiet, conn, "plex", g, "plan-a", buildItems(4)))

	live := pgx.Identifier{g.Schema()}.Sanitize()
	_, err := conn.Exec(ctx, "INSERT INTO "+live+".metadata_items (id, metadata_type, title) VALUES (100, 15, 'Watch later')")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "INSERT INTO "+live+".metadata_item_accounts (account_id, metadata_item_id) VALUES (1, 100)")
	require.NoError(t, err)

	require.NoError(t, SwapStaged(ctx, quiet, conn, "plex", g, "plan-a"))
	assert.Equal(t, 5, count(t, conn, g.Schema(), "metadata_items"))
	assert.Equal(t, "Watch later", one[string](t, conn, "SELECT title FROM "+live+".metadata_items WHERE metadata_type = 15 AND id = 5"),
		"the playlist made during the build, past the build's four items")
	assert.Equal(t, 1, one[int](t, conn, "SELECT count(*) FROM "+live+".metadata_item_accounts WHERE metadata_item_id = 5"))
}

// TestAStageWaitsForTheGuidesLock: a build takes the lock ImportGuide and
// the swaps take, so it never fills a staging schema another holds.
func TestAStageWaitsForTheGuidesLock(t *testing.T) {
	ctx := context.Background()
	connect, conn := libraryDB(t)
	other, err := connect(ctx)
	require.NoError(t, err)
	defer func() { _ = other.Close(context.Background()) }()
	unlock, err := lockGuide(ctx, other, testGuide)
	require.NoError(t, err)
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err = StageBuilt(short, quiet, conn, "plex", testGuide, "plan-a", buildItems(1))
	require.Error(t, err, "the stage waits on the lock until its context ends")
	unlock()
	// A context that ends mid-query closes its connection (pgx), which
	// also releases any lock the build held.
	again, err := connect(ctx)
	require.NoError(t, err)
	defer func() { _ = again.Close(context.Background()) }()
	require.NoError(t, StageBuilt(ctx, quiet, again, "plex", testGuide, "plan-a", buildItems(1)))
}

// TestTheStartSweepKeepsABuildsStagingSchema: a PMS start drops every
// staging schema but one a build Job is filling (it holds the guide's lock)
// or has staged for the holder to swap.
func TestTheStartSweepKeepsABuildsStagingSchema(t *testing.T) {
	ctx := context.Background()
	connect, conn := libraryDB(t)
	staged := Guide{Provider: "tv.plex.providers.epg.cloud", DVR: "7f4d37cb-fada-46d8-8fcb-fe1b0cff2d85"}
	building := Guide{Provider: "tv.plex.providers.epg.cloud", DVR: "16cac8a1-0746-4e63-9a32-c9d0fd96a0c1"}
	require.NoError(t, StageBuilt(ctx, quiet, conn, "plex", staged, "plan-a", buildItems(1)))
	for _, s := range []string{staged.Schema(), building.Schema(), building.Staging(), testGuide.Schema(), testGuide.Staging()} {
		_, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+s)
		require.NoError(t, err)
	}
	job, err := connect(ctx)
	require.NoError(t, err)
	defer func() { _ = job.Close(context.Background()) }()
	unlock, err := lockGuide(ctx, job, building)
	require.NoError(t, err)

	require.NoError(t, SweepGuides(ctx, quiet, conn, []Guide{staged, building, testGuide}, t.TempDir()))
	assert.ElementsMatch(t, []string{staged.Schema(), staged.Staging(), building.Schema(), building.Staging(), testGuide.Schema()},
		schemasLike(t, conn, "epg_"), "kept: a staged build and one being built; dropped: an idle staging schema")
	unlock()
	require.NoError(t, SweepGuides(ctx, quiet, conn, []Guide{staged, building, testGuide}, t.TempDir()))
	assert.NotContains(t, schemasLike(t, conn, "epg_"), building.Staging(), "once the build lets go it is an idle staging schema")
	assert.Contains(t, schemasLike(t, conn, "epg_"), staged.Staging(), "a staged build waits for its swap")
}
