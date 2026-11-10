package collections_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/collections"
	"github.com/mediactl/clusterplex/pkg/plexproject"
)

var (
	spirited = collections.Member{Kind: "Movie", Name: "spirited-away-2001"}
	totoro   = collections.Member{Kind: "Movie", Name: "my-neighbor-totoro-1988"}
	kiki     = collections.Member{Kind: "Movie", Name: "kikis-delivery-service-1989"}
)

// guids is a MemberResolver over a fixed map, or a fixed error.
type guids struct {
	m   map[collections.Member]string
	err error
}

func (g *guids) GUIDs(_ context.Context, namespace string, members []collections.Member) (map[collections.Member]string, error) {
	if g.err != nil {
		return nil, g.err
	}
	out := map[collections.Member]string{}
	for _, m := range members {
		if guid, ok := g.m[m]; ok {
			out[m] = guid
		}
	}
	return out, nil
}

func ghibli(members ...collections.Member) collections.Spec {
	return collections.Spec{
		Library: "Clustarr Movies", Title: "Studio Ghibli Essentials", ItemType: "movie",
		Members: &collections.Members{Items: members},
	}
}

func (f *fakePMS) setItems(section string, itemType string, items ...api.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[section+"/"+itemType] = items
}

func memberSyncer(f *fakePMS, g *guids) *collections.Syncer {
	s := syncer(f)
	s.Members = g
	return s
}

var ghibliGUIDs = map[collections.Member]string{spirited: "plex://movie/a", totoro: "plex://movie/b", kiki: "plex://movie/c"}

func TestMembersCreateAndAdd(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"}, api.Item{RatingKey: "3", GUID: "plex://movie/b"}, api.Item{RatingKey: "4", GUID: "plex://movie/c"})
	s := memberSyncer(f, &guids{m: ghibliGUIDs})

	out := s.Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{})
	assert.Equal(t, collections.ReasonCreated, out.Reason, out.Message)
	posts := f.writesTo("POST /library/collections")
	require.Len(t, posts, 1)
	assert.Equal(t, "0", posts[0].Query.Get("smart"))
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"2", "3"}), posts[0].Query.Get("uri"))
	assert.Equal(t, 2, out.ItemCount)

	out = s.Sync(t.Context(), ns, ghibli(spirited, totoro, kiki), collections.Previous{RatingKey: out.RatingKey})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	puts := f.writesTo("PUT /library/collections/" + out.RatingKey + "/items")
	require.Len(t, puts, 1)
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"4"}), puts[0].Query.Get("uri"))
	assert.Empty(t, f.writesTo("DELETE"))
	assert.Equal(t, 3, out.ItemCount)
}

func TestMembersRemoveExtras(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"}, api.Item{RatingKey: "3", GUID: "plex://movie/b"}, api.Item{RatingKey: "5", GUID: "plex://movie/e"})
	rk := f.add(fakeCollection{Section: "1", Title: "Studio Ghibli Essentials", Subtype: "movie", Children: []string{"2", "3", "5"}, ChildCount: 3})

	out := memberSyncer(f, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	dels := f.writesTo("DELETE /library/collections/" + rk + "/items/")
	require.Len(t, dels, 1)
	assert.Equal(t, "/library/collections/"+rk+"/items/5", dels[0].Path)
	assert.Empty(t, f.writesTo("PUT"))
	assert.Contains(t, out.Message, "members")
}

func TestMembersIncludeEditions(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1",
		api.Item{RatingKey: "2", GUID: "plex://movie/a"},
		api.Item{RatingKey: "7", GUID: "plex://movie/a/edition/Director%27s%20Cut"},
		api.Item{RatingKey: "3", GUID: "plex://movie/b"})
	memberSyncer(f, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{})
	posts := f.writesTo("POST /library/collections")
	require.Len(t, posts, 1)
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"2", "7", "3"}), posts[0].Query.Get("uri"))
}

func TestMembersPlexLacksAreReportedAndAddedLater(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"})
	s := memberSyncer(f, &guids{m: ghibliGUIDs})

	out := s.Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{})
	assert.Equal(t, collections.ReasonMembersPending, out.Reason, out.Message)
	assert.Equal(t, 1, out.MissingMembers)
	assert.Equal(t, []collections.Member{totoro}, out.Missing)
	assert.Contains(t, out.Message, "1 of 2")
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"2"}), f.writesTo("POST /library/collections")[0].Query.Get("uri"))

	// Plex scans Totoro in: the next sync adds it, though the section's
	// items were read before.
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"}, api.Item{RatingKey: "3", GUID: "plex://movie/b"})
	out = s.Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{RatingKey: out.RatingKey})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Zero(t, out.MissingMembers)
	assert.Empty(t, out.Missing)
	require.Len(t, f.writesTo("PUT /library/collections/"+out.RatingKey+"/items"), 1)

	// A collection none of whose members Plex holds is never created.
	empty := newFakePMS(t)
	out = memberSyncer(empty, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{})
	assert.Equal(t, collections.ReasonWaiting, out.Reason)
	assert.Equal(t, 2, out.MissingMembers)
	assert.Empty(t, empty.allWrites())
}

func TestMembersNotInClustarrAreMissing(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"})
	gone := collections.Member{Kind: "Movie", Name: "no-such-movie"}
	out := memberSyncer(f, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(spirited, gone), collections.Previous{})
	assert.Equal(t, collections.ReasonMembersPending, out.Reason)
	assert.Equal(t, []collections.Member{gone}, out.Missing)
}

func TestMembersMissingIsCapped(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"})
	members := []collections.Member{spirited}
	for i := range 25 {
		members = append(members, collections.Member{Kind: "Movie", Name: "absent-" + string(rune('a'+i))})
	}
	out := memberSyncer(f, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(members...), collections.Previous{})
	assert.Equal(t, 25, out.MissingMembers)
	assert.Len(t, out.Missing, 20)
}

func TestMembersNeverEmptyACollection(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/x"}, api.Item{RatingKey: "3", GUID: "plex://movie/y"})
	rk := f.add(fakeCollection{Section: "1", Title: "Studio Ghibli Essentials", Subtype: "movie", Children: []string{"2", "3"}, ChildCount: 2})

	out := memberSyncer(f, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(spirited, totoro), collections.Previous{})
	assert.Equal(t, rk, out.RatingKey)
	assert.Equal(t, collections.ReasonMembersPending, out.Reason, out.Message)
	assert.Empty(t, f.writesTo("DELETE"))
	assert.Equal(t, []string{"2", "3"}, f.col(rk).Children)
}

func TestMembersResolverFailureChangesNothing(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"}, api.Item{RatingKey: "5", GUID: "plex://movie/e"})
	rk := f.add(fakeCollection{Section: "1", Title: "Studio Ghibli Essentials", Subtype: "movie", Children: []string{"5"}, ChildCount: 1})

	out := memberSyncer(f, &guids{err: errors.New("the clustarr cache is not synced")}).Sync(t.Context(), ns, ghibli(spirited), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonPlexUnavailable, out.Reason)
	assert.Contains(t, out.Message, "the clustarr cache is not synced")
	assert.Empty(t, f.allWrites())
}

func TestMembersRemovePlexWebAdditions(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"}, api.Item{RatingKey: "9", GUID: "plex://movie/z"})
	rk := f.add(fakeCollection{Section: "1", Title: "Studio Ghibli Essentials", Subtype: "movie", Children: []string{"2", "9"}, ChildCount: 2})

	out := memberSyncer(f, &guids{m: ghibliGUIDs}).Sync(t.Context(), ns, ghibli(spirited), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonSynced, out.Reason)
	require.Len(t, f.writesTo("DELETE /library/collections/"+rk+"/items/9"), 1)
	assert.Equal(t, []string{"2"}, f.col(rk).Children)
}

func TestMembersOfAShowCollectionAreSeries(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("2", "2", api.Item{RatingKey: "40", GUID: "plex://show/s"})
	show := collections.Member{Kind: "Series", Name: "cowboy-bebop"}
	spec := collections.Spec{Library: "Clustarr TV", Title: "Space", ItemType: "show", Members: &collections.Members{Items: []collections.Member{show}}}
	out := memberSyncer(f, &guids{m: map[collections.Member]string{show: "plex://show/s"}}).Sync(t.Context(), ns, spec, collections.Previous{})
	assert.Equal(t, collections.ReasonCreated, out.Reason, out.Message)
	posts := f.writesTo("POST /library/collections")
	require.Len(t, posts, 1)
	assert.Equal(t, "2", posts[0].Query.Get("type"))
}

func TestBaseGUIDMatchesTheProjection(t *testing.T) {
	for _, g := range []string{"plex://movie/a", "plex://movie/a/edition/Director%27s%20Cut", "tv.plex.agents.custom.clustarr.movies://movie/9", ""} {
		assert.Equal(t, plexproject.BaseGUID(g), collections.BaseGUID(g), g)
	}
}

func TestMembersIndexIsReadAgainOnceStale(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"})
	now := syncNow
	s := memberSyncer(f, &guids{m: ghibliGUIDs})
	s.Now = func() time.Time { return now }
	out := s.Sync(t.Context(), ns, ghibli(spirited), collections.Previous{})
	require.Equal(t, collections.ReasonCreated, out.Reason, out.Message)
	rk := out.RatingKey

	// The movie is deleted and scanned in again: same GUID, new ratingKey.
	f.setItems("1", "1", api.Item{RatingKey: "20", GUID: "plex://movie/a"})
	now = syncNow.Add(11 * time.Minute)
	out = s.Sync(t.Context(), ns, ghibli(spirited), collections.Previous{RatingKey: rk})
	puts := f.writesTo("PUT /library/collections/" + rk + "/items")
	require.Len(t, puts, 1, "the stale index is read again")
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"20"}), puts[0].Query.Get("uri"))
	assert.Equal(t, []string{"20"}, f.col(rk).Children)
}
