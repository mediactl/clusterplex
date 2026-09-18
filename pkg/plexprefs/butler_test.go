package plexprefs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisabledButlerTasksTurnsEveryBackgroundJobOff(t *testing.T) {
	got := DisabledButlerTasks()

	require.Len(t, got, len(ButlerTasks))
	for _, task := range ButlerTasks {
		assert.Equal(t, "0", got[task], task)
	}
}

func TestDisabledButlerTasksNamesAreValidPreferences(t *testing.T) {
	// A typo here would be rejected at startup rather than silently leaving a
	// scheduler running on every pod.
	require.NoError(t, Validate(DisabledButlerTasks()))
}

func TestApplyTurnsOffASchedulerAPodHadEnabled(t *testing.T) {
	// The case that matters: someone switched a task back on in the Plex user
	// interface, and the next start has to undo it.
	path := writeFile(t, `<?xml version="1.0" encoding="utf-8"?>
<Preferences MachineIdentifier="9c67996e-8b08-44b9-9c83-a6d317322a2d" ButlerTaskAnalyzeMedia="1" FriendlyName="Cluster Plex"/>
`)

	changed, err := Apply(path, DisabledButlerTasks())
	require.NoError(t, err)

	got := attrs(t, path)
	assert.Equal(t, "0", got["ButlerTaskAnalyzeMedia"])
	assert.Contains(t, changed, "ButlerTaskAnalyzeMedia")
	assert.Equal(t, "Cluster Plex", got["FriendlyName"], "unrelated settings survive")
	assert.Equal(t, "9c67996e-8b08-44b9-9c83-a6d317322a2d", got["MachineIdentifier"])
}

func TestApplyIsQuietWhenEverySchedulerIsAlreadyOff(t *testing.T) {
	path := writeFile(t, `<?xml version="1.0" encoding="utf-8"?>
<Preferences ButlerTaskAnalyzeMedia="0"/>
`)
	changed, err := Apply(path, map[string]string{"ButlerTaskAnalyzeMedia": "0"})
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestIsButlerTaskRecognisesTheScheduledJobs(t *testing.T) {
	assert.True(t, IsButlerTask("ButlerTaskAnalyzeMedia"))
	assert.False(t, IsButlerTask("FriendlyName"))
	assert.False(t, IsButlerTask(""))
}
