package plexproject

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A file is missing only when the disk says "no such file"; a root that is
// absent or lists empty -- an NFS mount that is not there -- is not to be
// trusted, and a file under it is never judged.
func TestDiskFilesCallsAFileMissingOnlyWhenTheDiskSaysSo(t *testing.T) {
	root := t.TempDir()
	present := filepath.Join(root, "Heat (1995)", "Heat.mkv")
	require.NoError(t, os.MkdirAll(filepath.Dir(present), 0o755))
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o644))
	d := &DiskFiles{}
	ctx := context.Background()

	require.NoError(t, d.Root(ctx, root))
	missing, err := d.Missing(ctx, present)
	require.NoError(t, err)
	require.False(t, missing)
	missing, err = d.Missing(ctx, filepath.Join(root, "Heat (1995)", "Heat-old.mkv"))
	require.NoError(t, err)
	require.True(t, missing)
	missing, err = d.Missing(ctx, filepath.Join(root, "Gone (2001)", "Gone.mkv"))
	require.NoError(t, err)
	require.True(t, missing, "a file whose folder is gone too is gone")

	require.Error(t, d.Root(ctx, t.TempDir()), "an empty root")
	require.Error(t, d.Root(ctx, filepath.Join(root, "nope")), "an absent root")
}

// A stat that does not answer -- a hung NFS server -- is an error within
// the timeout, never an answer.
func TestDiskFilesGivesUpOnAStatThatHangs(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	d := &DiskFiles{Timeout: 20 * time.Millisecond, Stat: func(string) (fs.FileInfo, error) {
		<-block
		return nil, nil
	}}
	start := time.Now()
	_, err := d.Missing(context.Background(), "/media/movies/x.mkv")
	require.ErrorIs(t, err, ErrStalled)
	require.Less(t, time.Since(start), time.Second)
}
