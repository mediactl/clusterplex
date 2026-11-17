package collections_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

// movieHubs are a movie library's hubs as Plex ships them, with one
// collection's hub (ratingKey a24) in fourth place.
func movieHubs(a24 string) []api.ManagedHub {
	return []api.ManagedHub{
		{Identifier: "movie.recentlyadded", Title: "Recently Added Movies", Recommended: true, OwnHome: true, SharedHome: true},
		{Identifier: "movie.recentlyreleased", Title: "Recently Released Movies", Recommended: true},
		{Identifier: "movie.inprogress", Title: "Continue Watching", Recommended: true},
		{Identifier: "custom.collection.1." + a24, Title: "A24", Recommended: true, OwnHome: true, SharedHome: true},
		{Identifier: "movie.genre", Title: "More in Genre", Recommended: true},
	}
}

func hubSyncer(f *fakePMS) *collections.HubSyncer {
	return &collections.HubSyncer{PMS: f.client()}
}

// hubWrites are the writes under a section's managed hubs.
func hubWrites(f *fakePMS) []string {
	var out []string
	for _, w := range f.allWrites() {
		if strings.HasPrefix(w.Path, "/hubs/") {
			line := w.Method + " " + w.Path
			if q := w.Query.Encode(); q != "" {
				line += "?" + q
			}
			out = append(out, line)
		}
	}
	return out
}

func TestHubLayoutPutsListedHubsFirstWithTheFewestMoves(t *testing.T) {
	f := newFakePMS(t)
	a24 := f.add(fakeCollection{Section: "1", Title: "A24", Subtype: "movie"})
	f.setHubs("1", movieHubs(a24)...)
	in := collections.HubInput{
		Spec: collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{
			{Collection: "a24", LibraryRecommended: true, Home: true, FriendsHome: true},
			{Identifier: "movie.recentlyreleased", LibraryRecommended: true},
		}},
		Collections: []collections.HeldCollection{{Name: "a24", Library: "Clustarr Movies", RatingKey: a24}},
	}
	s := hubSyncer(f)

	out := s.Sync(t.Context(), in)
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Equal(t, "1", out.SectionKey)
	assert.Equal(t, []string{
		"custom.collection.1." + a24, "movie.recentlyreleased",
		"movie.recentlyadded", "movie.inprogress", "movie.genre",
	}, f.hubOrder("1"), "the listed first, in the layout's order; the rest as Plex had them")
	writes := hubWrites(f)
	require.Len(t, writes, 2, "two hubs are out of place and two moves put them right: %v", writes)
	assert.Equal(t, "PUT /hubs/sections/1/manage/custom.collection.1."+a24+"/move", writes[0], "first place names no hub to follow")
	for _, w := range writes {
		assert.Contains(t, w, "/move", "no visibility differs, so none is written")
	}
	require.Len(t, out.Hubs, 5)
	assert.Equal(t, collections.HubStatus{
		Identifier: "custom.collection.1." + a24, Title: "A24", Kind: "collection", Collection: "a24", RatingKey: a24,
		LibraryRecommended: true, Home: true, FriendsHome: true,
	}, out.Hubs[0])
	assert.Equal(t, collections.HubStatus{
		Identifier: "movie.recentlyreleased", Title: "Recently Released Movies", Kind: "builtin", LibraryRecommended: true,
	}, out.Hubs[1])

	// An idle layout costs one read of the hubs and no write.
	before, gets := len(f.allWrites()), f.got("/hubs/sections/1/manage")
	out = s.Sync(t.Context(), in)
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Len(t, f.allWrites(), before)
	assert.Equal(t, gets+1, f.got("/hubs/sections/1/manage"))
	assert.Equal(t, 1, f.got("/library/sections/all"), "the sections are read once and kept")
	assert.Len(t, out.Hubs, 5)
}

func TestHubLayoutSetsOnlyAListedHubsVisibilityWhereItDiffers(t *testing.T) {
	f := newFakePMS(t)
	a24 := f.add(fakeCollection{Section: "1", Title: "A24", Subtype: "movie"})
	f.setHubs("1", movieHubs(a24)...)
	in := collections.HubInput{Spec: collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{
		{Identifier: "movie.recentlyadded", LibraryRecommended: true, Home: true, FriendsHome: true},
		{Identifier: "movie.recentlyreleased", Home: true},
	}}}

	out := hubSyncer(f).Sync(t.Context(), in)
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Equal(t, []string{
		"PUT /hubs/sections/1/manage/movie.recentlyreleased?promotedToOwnHome=1&promotedToRecommended=0&promotedToSharedHome=0",
	}, hubWrites(f), "the order already holds, and one hub's visibility differs")
	assert.Equal(t, [3]bool{false, true, false}, f.hub("1", "movie.recentlyreleased").Promoted())
	assert.Equal(t, [3]bool{true, true, true}, f.hub("1", "custom.collection.1."+a24).Promoted(), "a hub the layout does not list is not touched")
	assert.True(t, out.Hubs[1].Home)
	assert.False(t, out.Hubs[1].LibraryRecommended)
}

func TestHubLayoutMakesACollectionsHubAndPlacesIt(t *testing.T) {
	f := newFakePMS(t)
	a24 := f.add(fakeCollection{Section: "1", Title: "A24", Subtype: "movie"})
	ghibli := f.add(fakeCollection{Section: "1", Title: "Studio Ghibli", Subtype: "movie"})
	up := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie"})
	f.setHubs("1", movieHubs(a24)...)
	in := collections.HubInput{
		Spec: collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{
			{Identifier: "movie.recentlyadded", LibraryRecommended: true, Home: true, FriendsHome: true},
			{CollectionTitle: "studio ghibli", LibraryRecommended: true},
			{Collection: "up"},
		}},
		Collections: []collections.HeldCollection{{Name: "up", Library: "Clustarr Movies", RatingKey: up}},
	}

	out := hubSyncer(f).Sync(t.Context(), in)
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	posts := f.writesTo("POST /hubs/sections/1/manage")
	require.Len(t, posts, 2)
	assert.Equal(t, ghibli, posts[0].Query.Get("metadataItemId"))
	assert.Equal(t, "1", posts[0].Query.Get("promotedToRecommended"))
	assert.Equal(t, up, posts[1].Query.Get("metadataItemId"))
	assert.Equal(t, "0", posts[1].Query.Get("promotedToRecommended"), "a hub promoted nowhere is still made, to be ordered")
	assert.Equal(t, []string{
		"movie.recentlyadded", "custom.collection.1." + ghibli, "custom.collection.1." + up,
		"movie.recentlyreleased", "movie.inprogress", "custom.collection.1." + a24, "movie.genre",
	}, f.hubOrder("1"))
	assert.Len(t, f.writesTo("PUT /hubs/sections/1/manage/"), 2, "the two new hubs are moved up from the end")
	assert.Equal(t, collections.HubStatus{
		Identifier: "custom.collection.1." + ghibli, Title: "Studio Ghibli", Kind: "collection", RatingKey: ghibli, LibraryRecommended: true,
	}, out.Hubs[1], "a collection no resource holds names none")
	assert.Equal(t, "up", out.Hubs[2].Collection)
}

func TestHubLayoutNamesAnUntitledCollectionHubFromItsCollection(t *testing.T) {
	f := newFakePMS(t)
	f.untitledHubs = true
	up := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie"})
	f.setHubs("1", movieHubs("77")[:2]...)
	out := hubSyncer(f).Sync(t.Context(), collections.HubInput{
		Spec:        collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{{Collection: "up", Home: true}}},
		Collections: []collections.HeldCollection{{Name: "up", Library: "Clustarr Movies", RatingKey: up}},
	})
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Equal(t, "UP", out.Hubs[0].Title)
}

func TestHubLayoutSkipsWhatItCannotResolveAndAppliesTheRest(t *testing.T) {
	f := newFakePMS(t)
	a24 := f.add(fakeCollection{Section: "1", Title: "A24", Subtype: "movie"})
	f.add(fakeCollection{Section: "1", Title: "Twins", Subtype: "movie"})
	f.add(fakeCollection{Section: "1", Title: "twins", Subtype: "movie"})
	tv := f.add(fakeCollection{Section: "2", Title: "Anime", Subtype: "show"})
	f.setHubs("1", movieHubs(a24)...)
	held := []collections.HeldCollection{
		{Name: "pending", Library: "Clustarr Movies"},
		{Name: "anime", Library: "Clustarr TV", RatingKey: tv},
	}
	for name, tc := range map[string]struct {
		entry  collections.HubEntry
		reason string
		says   string
	}{
		"unknown identifier":     {collections.HubEntry{Identifier: "movie.nope"}, collections.ReasonUnknownIdentifier, `"movie.nope"`},
		"no ratingKey yet":       {collections.HubEntry{Collection: "pending"}, collections.ReasonCollectionNotReady, `"pending"`},
		"no such resource":       {collections.HubEntry{Collection: "ghost"}, collections.ReasonCollectionNotReady, `"ghost"`},
		"another library's":      {collections.HubEntry{Collection: "anime"}, collections.ReasonInvalidEntry, `"anime"`},
		"title not found":        {collections.HubEntry{CollectionTitle: "Nope"}, collections.ReasonCollectionNotFound, `"Nope"`},
		"title ambiguous":        {collections.HubEntry{CollectionTitle: "Twins"}, collections.ReasonCollectionAmbiguous, `"Twins"`},
		"two kinds in one entry": {collections.HubEntry{Identifier: "movie.genre", Collection: "a24"}, collections.ReasonInvalidEntry, "hubs[0]"},
		"no kind at all":         {collections.HubEntry{Home: true}, collections.ReasonInvalidEntry, "hubs[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			f.setHubs("1", movieHubs(a24)...)
			out := hubSyncer(f).Sync(t.Context(), collections.HubInput{
				Spec: collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{
					tc.entry, {Identifier: "movie.genre", LibraryRecommended: true},
				}},
				Collections: held,
			})
			assert.Equal(t, tc.reason, out.Reason, out.Message)
			assert.Contains(t, out.Message, tc.says)
			assert.Equal(t, "movie.genre", f.hubOrder("1")[0], "the entry that resolves is still placed")
			assert.Len(t, out.Hubs, 5, "and Plex's hubs are still reported")
		})
	}

	t.Run("the same hub twice", func(t *testing.T) {
		out := hubSyncer(f).Sync(t.Context(), collections.HubInput{Spec: collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{
			{Identifier: "movie.genre", LibraryRecommended: true}, {Identifier: "movie.genre"},
		}}})
		assert.Equal(t, collections.ReasonInvalidEntry, out.Reason, out.Message)
		assert.True(t, f.hub("1", "movie.genre").Recommended, "the first mention stands")
	})
}

func TestHubLayoutLibraryNotFoundAndPlexAway(t *testing.T) {
	f := newFakePMS(t)
	f.setHubs("1", movieHubs("77")...)
	out := hubSyncer(f).Sync(t.Context(), collections.HubInput{Spec: collections.HubLayoutSpec{Library: "Music"}})
	assert.Equal(t, collections.ReasonLibraryNotFound, out.Reason, out.Message)
	assert.False(t, out.Known)
	assert.Empty(t, f.allWrites())

	// An unclaimed server lists no hubs to manage.
	f.failNext("GET /hubs/sections/1/manage", http.StatusNotFound)
	out = hubSyncer(f).Sync(t.Context(), collections.HubInput{Spec: collections.HubLayoutSpec{Library: "Clustarr Movies"}})
	assert.Equal(t, collections.ReasonPlexUnavailable, out.Reason, out.Message)
	assert.Contains(t, out.Message, "claimed")
	assert.False(t, out.Known)

	f.failNext("PUT /hubs/sections/1/manage/movie.genre/move", http.StatusInternalServerError)
	out = hubSyncer(f).Sync(t.Context(), collections.HubInput{Spec: collections.HubLayoutSpec{
		Library: "Clustarr Movies", Hubs: []collections.HubEntry{{Identifier: "movie.genre", LibraryRecommended: true}},
	}})
	assert.Equal(t, collections.ReasonPlexUnavailable, out.Reason, out.Message)
}

func TestHubLayoutResetsThenAppliesTheLayout(t *testing.T) {
	f := newFakePMS(t)
	a24 := f.add(fakeCollection{Section: "1", Title: "A24", Subtype: "movie"})
	f.setHubs("1", movieHubs(a24)...)
	// A person's customization, which the reset undoes.
	f.hubs["1"][1].Recommended = false
	f.hubs["1"][0], f.hubs["1"][2] = f.hubs["1"][2], f.hubs["1"][0]
	in := collections.HubInput{
		Reset: true,
		Spec: collections.HubLayoutSpec{Library: "Clustarr Movies", Hubs: []collections.HubEntry{
			{Identifier: "movie.genre", LibraryRecommended: true},
		}},
	}

	out := hubSyncer(f).Sync(t.Context(), in)
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.True(t, out.ResetDone)
	writes := hubWrites(f)
	require.NotEmpty(t, writes)
	assert.Equal(t, "DELETE /hubs/sections/1/manage", writes[0], "the reset comes before anything the layout asks")
	assert.Equal(t, []string{"movie.genre", "movie.recentlyadded", "movie.recentlyreleased", "movie.inprogress"}, f.hubOrder("1"),
		"Plex's own list again, without the collection's hub, then the layout's order")
	assert.True(t, f.hub("1", "movie.recentlyreleased").Recommended)

	// A reset Plex refuses is not done, and nothing else is tried.
	f.failNext("DELETE /hubs/sections/1/manage", http.StatusInternalServerError)
	before := len(f.allWrites())
	out = hubSyncer(f).Sync(t.Context(), in)
	assert.Equal(t, collections.ReasonPlexUnavailable, out.Reason)
	assert.False(t, out.ResetDone)
	assert.Len(t, f.allWrites(), before+1)
}

func TestHubLayoutCapsTheHubsItReports(t *testing.T) {
	f := newFakePMS(t)
	var hubs []api.ManagedHub
	for i := range 205 {
		hubs = append(hubs, api.ManagedHub{Identifier: "custom.collection.1." + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Title: strings.Repeat("t", 300)})
	}
	f.setHubs("1", hubs...)
	out := hubSyncer(f).Sync(t.Context(), collections.HubInput{Spec: collections.HubLayoutSpec{Library: "Clustarr Movies"}})
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Len(t, out.Hubs, 200)
	assert.Len(t, out.Hubs[0].Title, 256)
	assert.Contains(t, out.Message, "205")
}

func TestHubLayoutFindsALibraryByAFormerName(t *testing.T) {
	f := newFakePMS(t)
	f.sections[0].Title = "Movies"
	f.setHubs("1", movieHubs("77")...)
	s := hubSyncer(f)
	s.Sections = func(all []api.Section, title string) []api.Section {
		if title == "Clustarr Movies" {
			return all[:1]
		}
		return nil
	}
	out := s.Sync(t.Context(), collections.HubInput{Spec: collections.HubLayoutSpec{Library: "Clustarr Movies"}})
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Equal(t, "1", out.SectionKey)
}

func TestPlanMovesIsTheFewest(t *testing.T) {
	for _, tc := range []struct {
		current, target []string
		want            [][2]string
	}{
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}, nil},
		{[]string{"a", "b", "c"}, []string{"c", "a", "b"}, [][2]string{{"c", ""}}},
		{[]string{"a", "b", "c", "d"}, []string{"b", "c", "d", "a"}, [][2]string{{"a", "d"}}},
		{[]string{"a", "b", "c", "d"}, []string{"d", "c", "b", "a"}, [][2]string{{"d", ""}, {"c", "d"}, {"b", "c"}}},
		{[]string{"a", "b", "c", "d", "e"}, []string{"a", "d", "b", "c", "e"}, [][2]string{{"d", "a"}}},
	} {
		got := collections.PlanMoves(tc.current, tc.target)
		assert.Equal(t, tc.want, got, "%v -> %v", tc.current, tc.target)
		// Applying them gives the target.
		order := append([]string(nil), tc.current...)
		for _, m := range got {
			i := indexOf(order, m[0])
			order = append(order[:i], order[i+1:]...)
			to := 0
			if m[1] != "" {
				to = indexOf(order, m[1]) + 1
			}
			order = append(order[:to], append([]string{m[0]}, order[to:]...)...)
		}
		assert.Equal(t, tc.target, order)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestACollectionALayoutListsLeavesItsVisibilityToTheLayout(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	spec := up()
	spec.Visibility = &collections.Visibility{LibraryRecommended: true, Home: true}

	out := syncer(f).Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk, HubLayout: true})
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Empty(t, hubWrites(f), "a PlexHubLayout lists the collection: its hub is the layout's")
	assert.Zero(t, f.got("/hubs/sections/1/manage"))

	out = syncer(f).Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	require.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Len(t, hubWrites(f), 1, "one no layout lists is as before")
}

func TestACollectionFindsItsLibraryByAFormerName(t *testing.T) {
	f := newFakePMS(t)
	f.sections[0].Title = "Movies"
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	s := syncer(f)

	out := s.Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonLibraryNotFound, out.Reason, "without the former names, the old title names nothing")

	s.Sections = func(all []api.Section, title string) []api.Section {
		if title == "Clustarr Movies" {
			return all[:1]
		}
		return nil
	}
	out = s.Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
}
