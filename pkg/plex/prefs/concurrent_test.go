package plexprefs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeveralPodsMergingAtOnceDoNotLoseEachOthersChanges(t *testing.T) {
	// Every pod runs Plex now (ADR-0004), and they all merge into one
	// Preferences.xml on shared storage as they start. Apply is a
	// read-modify-write: the write itself is atomic, so the file never tears,
	// but two pods that read the same original both merge onto that original
	// and the second rename silently discards the first's setting.
	//
	// Starting together is the normal case rather than the rare one — a
	// rolling update does exactly this.
	dir := t.TempDir()
	path := filepath.Join(dir, "Preferences.xml")
	require.NoError(t, os.WriteFile(path, []byte(live), 0o600))

	const pods = 8
	var wg sync.WaitGroup
	errs := make(chan error, pods)
	for i := range pods {
		wg.Go(func() {
			if _, err := Apply(path, map[string]string{
				fmt.Sprintf("FriendlyName%d", i): "set",
			}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	got := attrs(t, path)
	for i := range pods {
		assert.Contains(t, got, fmt.Sprintf("FriendlyName%d", i),
			"a pod's setting was overwritten by one that started at the same time")
	}
	assert.Contains(t, got, "MachineIdentifier", "the identity survives every merge")
}
