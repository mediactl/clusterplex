package plexnet

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemovingAFilterThatWasNeverInstalledIsNotAnError(t *testing.T) {
	// The lease holder has no filter, so the very first Set(nil) it makes asks
	// nftables to delete a chain that does not exist. That is the normal path,
	// not a fault, and nftables answers ENOENT.
	wrapped := fmt.Errorf("conn.Receive: netlink receive: %w", syscall.ENOENT)
	assert.NoError(t, ignoreMissing(wrapped))
}

func TestRemovingAFilterStillReportsARealFailure(t *testing.T) {
	// Permission denied means the rule is not gone and we would not know it: a
	// pod would keep its route to plex.tv while believing it had given it up.
	wrapped := fmt.Errorf("conn.Receive: netlink receive: %w", syscall.EPERM)
	require.Error(t, ignoreMissing(wrapped))
	assert.True(t, errors.Is(ignoreMissing(wrapped), syscall.EPERM))
}

func TestNoErrorStaysNoError(t *testing.T) {
	assert.NoError(t, ignoreMissing(nil))
}

func TestAMissingFileErrorCountsAsAlreadyGone(t *testing.T) {
	assert.NoError(t, ignoreMissing(fmt.Errorf("delete: %w", os.ErrNotExist)))
}
