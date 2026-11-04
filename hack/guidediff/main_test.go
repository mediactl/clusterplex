package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiffTellsWhatIsOnlyOnOneSideFromWhatDiffers(t *testing.T) {
	a := map[string]string{"plex://show/1": "2|Show|x", "plex://show/2": "2|Other|y", "plex://movie/3": "1|Film|z"}
	b := map[string]string{"plex://show/1": "2|Show|x", "plex://show/2": "2|Other|changed", "plex://episode/4": "4|Ep|w"}
	d := Diff(a, b)
	assert.Equal(t, []string{"plex://movie/3"}, d.OnlyA)
	assert.Equal(t, []string{"plex://episode/4"}, d.OnlyB)
	assert.Equal(t, []string{"plex://show/2"}, d.Changed)
	assert.Len(t, d.Examples(a, b, 10), 3)
	assert.Len(t, d.Examples(a, b, 0), 0)
	assert.Empty(t, Diff(a, a).Changed)
}
