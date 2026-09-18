package hashring

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocateIsStableForTheSameKeyAndMembers(t *testing.T) {
	a := New("plex-0", "plex-1", "plex-2")
	b := New("plex-2", "plex-0", "plex-1")

	// Every pod builds its own ring, so the order members arrive in must not
	// change the answer or two pods would disagree about who owns a key.
	for _, key := range []string{"1", "2", "library-42", "abc"} {
		assert.Equal(t, a.Locate(key), b.Locate(key), "key %q", key)
	}
}

func TestLocateReturnsAMember(t *testing.T) {
	r := New("plex-0", "plex-1")
	assert.Contains(t, []string{"plex-0", "plex-1"}, r.Locate("anything"))
}

func TestLocateSpreadsKeysAcrossMembers(t *testing.T) {
	r := New("plex-0", "plex-1", "plex-2")
	counts := map[string]int{}
	for i := range 600 {
		counts[r.Locate(fmt.Sprintf("library-%d", i))]++
	}

	require.Len(t, counts, 3, "every member should own some keys")
	for member, n := range counts {
		// Consistent hashing is not perfectly even; this only catches a ring
		// that collapses most keys onto one member.
		assert.Greater(t, n, 60, "member %s owns too few keys", member)
	}
}

func TestRemovingAMemberMovesOnlyItsKeys(t *testing.T) {
	// The property that makes this worth using over a modulo: losing a pod
	// must not reshuffle work that was not on it.
	before := New("plex-0", "plex-1", "plex-2")
	after := New("plex-0", "plex-1")

	var moved, kept int
	for i := range 600 {
		key := fmt.Sprintf("library-%d", i)
		owner := before.Locate(key)
		if owner == "plex-2" {
			continue
		}
		if after.Locate(key) == owner {
			kept++
		} else {
			moved++
		}
	}
	assert.Zero(t, moved, "keys not owned by the departed member must not move")
	assert.Positive(t, kept)
}

func TestAnEmptyRingLocatesNothing(t *testing.T) {
	assert.Empty(t, New().Locate("key"))
}

func TestASingleMemberOwnsEverything(t *testing.T) {
	r := New("plex-0")
	assert.Equal(t, "plex-0", r.Locate("a"))
	assert.Equal(t, "plex-0", r.Locate("b"))
}

func TestMembersAreReportedSortedAndDeduplicated(t *testing.T) {
	r := New("plex-2", "plex-0", "plex-2", "plex-1")
	assert.Equal(t, []string{"plex-0", "plex-1", "plex-2"}, r.Members())
}

func TestAnEmptyMemberNameIsIgnored(t *testing.T) {
	r := New("plex-0", "")
	assert.Equal(t, []string{"plex-0"}, r.Members())
}
