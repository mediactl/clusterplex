package collections_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

const ns = "clustarr-system"

var syncNow = time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)

func syncer(f *fakePMS) *collections.Syncer {
	return &collections.Syncer{PMS: f.client(), MachineID: talosMachine, Now: func() time.Time { return syncNow }}
}

// upContent is UP's filter as a collection holds it, with a carried sort.
func upContent(t *testing.T, s collections.Spec, carry collections.Carry) string {
	t.Helper()
	q, err := collections.Compile(*s.Filter, s.Sort, s.Limit, s.ItemType, recordedMeta(t, "1"), talosTags, carry)
	require.NoError(t, err)
	return collections.SmartURI(talosMachine, "1", q)
}

func TestSyncAdoptsByTitleAndKeepsRatingKey(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, ChildCount: 8, Content: storedContent(t, "1", "UP")})

	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, collections.ReasonAdopted, out.Reason, out.Message)
	assert.True(t, out.Adopted)
	assert.Equal(t, rk, out.RatingKey)
	assert.Equal(t, 8, out.ItemCount)
	assert.Equal(t, "library/sections/1/all?type=1&push=1&push=1&title==Seven Up!&pop=1&or=1&push=1&genre=5836&and=1&title=Up&pop=1&pop=1", out.Content)
	assert.Empty(t, f.allWrites())

	// The next sync finds it by its ratingKey and keeps the recorded flag.
	out = syncer(f).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk, Adopted: true})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.True(t, out.Adopted)
	assert.Empty(t, f.allWrites())
}

func TestSyncCreatesWhenAbsent(t *testing.T) {
	f := newFakePMS(t)
	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, collections.ReasonCreated, out.Reason, out.Message)
	assert.False(t, out.Adopted)
	posts := f.writesTo("POST /library/collections")
	require.Len(t, posts, 1)
	assert.Equal(t, "1", posts[0].Query.Get("smart"))
	assert.Equal(t, "1", posts[0].Query.Get("type"))
	assert.Equal(t, "1", posts[0].Query.Get("sectionId"))
	assert.Equal(t, "UP", posts[0].Query.Get("title"))
	assert.Equal(t, storedContent(t, "1", "UP"), posts[0].Query.Get("uri"))
	require.NotEmpty(t, out.RatingKey)
	assert.NotNil(t, f.col(out.RatingKey))
	assert.Len(t, f.allWrites(), 1, "a new collection needs no filter put or title edit")
}

func TestSyncPutsBackADriftedFilter(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "Documentaries")})

	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonSynced, out.Reason)
	assert.Contains(t, out.Message, "filter")
	puts := f.writesTo("PUT /library/collections/" + rk + "/items")
	require.Len(t, puts, 1)
	// Documentaries held sort=random:desc, which UP's spec leaves to Plex.
	assert.Equal(t, upContent(t, up(), collections.Carry{Sort: "random:desc"}), puts[0].Query.Get("uri"))
	assert.Equal(t, puts[0].Query.Get("uri"), f.col(rk).Content)
}

func TestSyncKeepsPlexSort(t *testing.T) {
	f := newFakePMS(t)
	held := upContent(t, up(), collections.Carry{Sort: "rating:desc"})
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: held})

	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Empty(t, f.allWrites(), "a sort set in Plex is no drift")

	changed := up()
	changed.Filter.Rules[0].Value = "7 Up!"
	syncer(f).Sync(t.Context(), ns, changed, collections.Previous{RatingKey: rk})
	puts := f.writesTo("PUT /library/collections/" + rk + "/items")
	require.Len(t, puts, 1)
	assert.Contains(t, puts[0].Query.Get("uri"), "sort=rating%3Adesc")
	assert.Contains(t, puts[0].Query.Get("uri"), "7%20Up!")
}

func TestSyncNeverAdoptsTheOtherKind(t *testing.T) {
	f := newFakePMS(t)
	f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: false})
	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, collections.ReasonNotSmart, out.Reason)
	assert.Empty(t, out.RatingKey)
	assert.Empty(t, f.allWrites())
}

func TestSyncNeverAdoptsAMetadataCollection(t *testing.T) {
	f := newFakePMS(t)
	f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", GUID: "tmdb://collection/1234"})
	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, collections.ReasonNotAdoptable, out.Reason)
	assert.Empty(t, f.allWrites())
}

func TestSyncRefusesTwoCandidates(t *testing.T) {
	f := newFakePMS(t)
	f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, collections.ReasonConflict, out.Reason)
	assert.Empty(t, f.allWrites())
}

func TestSyncKeepsTheLastGoodFilterOnUnknownValue(t *testing.T) {
	f := newFakePMS(t)
	good := storedContent(t, "1", "UP")
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: good})
	bad := up()
	bad.Filter.Rules[1].Rules[0].Value = "Nope"

	out := syncer(f).Sync(t.Context(), ns, bad, collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonUnknownValue, out.Reason)
	assert.Contains(t, out.Message, "Nope")
	assert.Equal(t, rk, out.RatingKey, "the recorded collection is kept")
	assert.Empty(t, f.allWrites())
	assert.Equal(t, good, f.col(rk).Content)
}

func TestSyncReportsAnInvalidRuleAndAMissingLibrary(t *testing.T) {
	f := newFakePMS(t)
	bad := up()
	bad.Filter.Rules[0].Field = "nonsense"
	assert.Equal(t, collections.ReasonInvalidRule, syncer(f).Sync(t.Context(), ns, bad, collections.Previous{}).Reason)

	lost := up()
	lost.Library = "Nope"
	out := syncer(f).Sync(t.Context(), ns, lost, collections.Previous{})
	assert.Equal(t, collections.ReasonLibraryNotFound, out.Reason)
	assert.Contains(t, out.Message, "Nope")
	assert.Empty(t, f.allWrites())
}

func TestSyncNeverCreatesWhenListingFails(t *testing.T) {
	f := newFakePMS(t)
	f.failNext("GET /library/sections/1/collections", http.StatusServiceUnavailable)
	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, collections.ReasonPlexUnavailable, out.Reason)
	assert.Empty(t, f.writesTo("POST"))

	f.failNext("GET /library/sections/all", http.StatusServiceUnavailable)
	assert.Equal(t, collections.ReasonPlexUnavailable, syncer(f).Sync(t.Context(), ns, up(), collections.Previous{}).Reason)

	f.failNext("GET /library/metadata/77", http.StatusInternalServerError)
	out = syncer(f).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: "77"})
	assert.Equal(t, collections.ReasonPlexUnavailable, out.Reason, "only a 404 means the collection is gone")
	assert.Empty(t, f.writesTo("POST"))
}

func TestSyncRecreatesACollectionDeletedInPlex(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: "4040"})
	assert.Equal(t, collections.ReasonAdopted, out.Reason)
	assert.Equal(t, rk, out.RatingKey)

	empty := newFakePMS(t)
	out = syncer(empty).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: "4040"})
	assert.Equal(t, collections.ReasonCreated, out.Reason)
	assert.NotEqual(t, "4040", out.RatingKey)
	assert.Len(t, empty.writesTo("POST /library/collections"), 1)
}

func TestSyncEditsOnlyStatedTitles(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "Up!", Subtype: "movie", Smart: true, Summary: "Plex's own words.", Content: storedContent(t, "1", "UP")})

	out := syncer(f).Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	edits := f.writesTo("PUT /library/sections/1/all")
	require.Len(t, edits, 1, "the title alone: summary and sort title are not stated")
	assert.Equal(t, "UP", edits[0].Query.Get("title.value"))
	assert.Equal(t, "1", edits[0].Query.Get("title.locked"))
	assert.Equal(t, rk, edits[0].Query.Get("id"))
	assert.Contains(t, out.Message, "title")
	assert.Equal(t, "Plex's own words.", f.col(rk).Summary)

	stated := up()
	summary, sortTitle := "Every Up film.", "Up 00"
	stated.Summary, stated.SortTitle = &summary, &sortTitle
	syncer(f).Sync(t.Context(), ns, stated, collections.Previous{RatingKey: rk})
	edits = f.writesTo("PUT /library/sections/1/all")
	require.Len(t, edits, 3)
	assert.Equal(t, "Every Up film.", f.col(rk).Summary)
	assert.Equal(t, "Up 00", f.col(rk).TitleSort)

	syncer(f).Sync(t.Context(), ns, stated, collections.Previous{RatingKey: rk})
	assert.Len(t, f.writesTo("PUT /library/sections/1/all"), 3, "nothing differs any more")
}

func TestDeleteOnlyWithARatingKey(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true})
	s := syncer(f)
	require.NoError(t, s.Delete(t.Context(), ""))
	assert.Empty(t, f.allWrites())
	require.NoError(t, s.Delete(t.Context(), rk))
	require.Len(t, f.writesTo("DELETE /library/collections/"+rk), 1)
	assert.Nil(t, f.col(rk))
	require.NoError(t, s.Delete(t.Context(), rk), "gone already")
}

func TestSyncCachesTheSectionMeta(t *testing.T) {
	f := newFakePMS(t)
	s := syncer(f)
	s.Sync(t.Context(), ns, up(), collections.Previous{})
	s.Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, 1, f.gets["GET /library/sections/1/all"], "the Meta is read once per section")
	s.Forget()
	s.Sync(t.Context(), ns, up(), collections.Previous{})
	assert.Equal(t, 2, f.gets["GET /library/sections/1/all"])
	assert.True(t, strings.HasPrefix(storedContent(t, "1", "UP"), "server://"))
}
