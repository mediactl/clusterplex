package collections_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

const posterURL = "https://image.tmdb.org/t/p/original/poster.jpg"

var posterBytes = []byte("\x89PNG\r\n\x1a\nthe UP poster")

// images is an ImageFetcher answering from a map, counting fetches.
type images struct {
	bodies map[string][]byte
	types  map[string]string
	errs   map[string]error
	calls  int
}

func (i *images) fetch(_ context.Context, url string) ([]byte, string, error) {
	i.calls++
	if err := i.errs[url]; err != nil {
		return nil, "", err
	}
	ct := i.types[url]
	if ct == "" {
		ct = "image/png"
	}
	return i.bodies[url], ct, nil
}

func presentSyncer(f *fakePMS, img *images, now *time.Time) *collections.Syncer {
	s := syncer(f)
	s.Fetch = img.fetch
	s.Now = func() time.Time { return *now }
	return s
}

func upWithPoster() collections.Spec {
	s := up()
	s.Poster = &collections.Image{URL: posterURL}
	return s
}

func TestImageUploadedOnceThenLeft(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	img := &images{bodies: map[string][]byte{posterURL: posterBytes}}
	now := syncNow
	s := presentSyncer(f, img, &now)

	out := s.Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk})
	assert.True(t, collections.Healthy(out.Reason), out.Reason+": "+out.Message)
	ups := f.writesTo("POST /library/metadata/" + rk + "/posters")
	require.Len(t, ups, 1)
	assert.Equal(t, posterBytes, ups[0].Body)
	assert.Equal(t, "image/png", ups[0].ContentType)
	want := &collections.ImageState{URL: posterURL, SHA1: fmt.Sprintf("%x", sha1.Sum(posterBytes)), FetchedAt: syncNow}
	assert.Equal(t, want, out.Poster)
	assert.Equal(t, 1, img.calls)

	now = syncNow.Add(time.Hour)
	out = s.Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk, Poster: out.Poster})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Equal(t, 1, img.calls, "no fetch within 24 hours")
	assert.Len(t, f.writesTo("POST /library/metadata/"+rk+"/posters"), 1, "no upload while Plex shows it")
	assert.Equal(t, want, out.Poster)

	// A day on, it is fetched again; the same bytes upload nothing.
	now = syncNow.Add(25 * time.Hour)
	out = s.Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk, Poster: out.Poster})
	assert.Equal(t, 2, img.calls)
	assert.Len(t, f.writesTo("POST /library/metadata/"+rk+"/posters"), 1)
	assert.Equal(t, now, out.Poster.FetchedAt)

	// The spec dropping its poster leaves Plex's and records none.
	out = s.Sync(t.Context(), ns, up(), collections.Previous{RatingKey: rk, Poster: out.Poster})
	assert.Nil(t, out.Poster)
	assert.Len(t, f.writesTo("POST /library/metadata/"+rk+"/posters"), 1)
}

func TestImagePutBackWhenAnotherIsSelected(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	img := &images{bodies: map[string][]byte{posterURL: posterBytes}}
	now := syncNow
	s := presentSyncer(f, img, &now)
	out := s.Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk})

	// Someone picks another poster in Plex Web.
	f.mu.Lock()
	for i := range f.cols[rk].Posters {
		f.cols[rk].Posters[i].Selected = false
	}
	f.cols[rk].Posters = append(f.cols[rk].Posters, api.Poster{Key: "composite", Selected: true})
	f.mu.Unlock()

	out = s.Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk, Poster: out.Poster})
	assert.Equal(t, collections.ReasonSynced, out.Reason)
	assert.Contains(t, out.Message, "poster")
	assert.Equal(t, 2, img.calls, "the bytes are fetched again to upload")
	ups := f.writesTo("POST /library/metadata/" + rk + "/posters")
	require.Len(t, ups, 2)
	assert.Equal(t, posterBytes, ups[1].Body)
}

func TestImageFailureLeavesTheImageAndSyncsTheRest(t *testing.T) {
	big := bytes.Repeat([]byte{0}, 12<<20)
	for name, img := range map[string]*images{
		"a fetch error":  {errs: map[string]error{posterURL: errors.New("dial tcp: no route to host")}},
		"a 12 MiB body":  {bodies: map[string][]byte{posterURL: big}},
		"a page, no art": {bodies: map[string][]byte{posterURL: []byte("<html>")}, types: map[string]string{posterURL: "text/html"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePMS(t)
			rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "Documentaries")})
			kept := &collections.ImageState{URL: "https://example.org/old.jpg", SHA1: "abc", FetchedAt: syncNow.Add(-time.Hour)}
			now := syncNow
			out := presentSyncer(f, img, &now).Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk, Poster: kept})
			assert.Equal(t, collections.ReasonImageUnavailable, out.Reason)
			assert.Contains(t, out.Message, "poster")
			assert.Equal(t, kept, out.Poster, "the recorded image stands")
			assert.Empty(t, f.writesTo("POST /library/metadata/"+rk+"/posters"))
			require.Len(t, f.writesTo("PUT /library/collections/"+rk+"/items"), 1, "the drifted filter is still put back")
		})
	}
}

func TestSettingsOnlyWhenStated(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "2", GUID: "plex://movie/a"})
	rk := f.add(fakeCollection{Section: "1", Title: "Studio Ghibli Essentials", Subtype: "movie", Children: []string{"2"}, ChildCount: 1, Mode: -1, Order: 0})
	s := memberSyncer(f, &guids{m: ghibliGUIDs})

	s.Sync(t.Context(), ns, ghibli(spirited), collections.Previous{RatingKey: rk})
	assert.Empty(t, f.writesTo("PUT /library/metadata/"+rk+"/prefs"))

	spec := ghibli(spirited)
	spec.Settings = &collections.Settings{Mode: "hideCollection"}
	out := s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	prefs := f.writesTo("PUT /library/metadata/" + rk + "/prefs")
	require.Len(t, prefs, 1)
	assert.Equal(t, "0", prefs[0].Query.Get("collectionMode"))
	assert.False(t, prefs[0].Query.Has("collectionSort"))
	assert.Contains(t, out.Message, "mode")

	s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	assert.Len(t, f.writesTo("PUT /library/metadata/"+rk+"/prefs"), 1, "a matching value sends nothing")

	spec.Settings = &collections.Settings{Mode: "showCollectionAndItems", Order: "alphabetical"}
	s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	prefs = f.writesTo("PUT /library/metadata/" + rk + "/prefs")
	require.Len(t, prefs, 2)
	assert.Equal(t, "2", prefs[1].Query.Get("collectionMode"))
	assert.Equal(t, "1", prefs[1].Query.Get("collectionSort"))
}

func TestSmartCollectionOrderIsNeverSent(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP"), Mode: -1, Order: 0})
	spec := up()
	spec.Settings = &collections.Settings{Order: "alphabetical"}
	syncer(f).Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	assert.Empty(t, f.writesTo("PUT /library/metadata/"+rk+"/prefs"))
}

func TestVisibilityCreatesThenUpdatesTheHub(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "2", Title: "Netflix", Subtype: "show", Smart: true, Content: storedContent(t, "2", "Netflix")})
	s := syncer(f)
	hubPath := "/hubs/sections/2/manage"

	// None stated: the hubs are never read.
	s.Sync(t.Context(), ns, netflix(), collections.Previous{RatingKey: rk})
	assert.Zero(t, f.gets["GET "+hubPath])

	// All false and no hub: nothing to write.
	spec := netflix()
	spec.Visibility = &collections.Visibility{}
	s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	assert.Empty(t, f.writesTo("POST "+hubPath))
	assert.Empty(t, f.writesTo("PUT "+hubPath))

	spec.Visibility = &collections.Visibility{LibraryRecommended: true, Home: true}
	out := s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	posts := f.writesTo("POST " + hubPath)
	require.Len(t, posts, 1)
	assert.Equal(t, rk, posts[0].Query.Get("metadataItemId"))
	assert.Equal(t, "1", posts[0].Query.Get("promotedToRecommended"))
	assert.Equal(t, "1", posts[0].Query.Get("promotedToOwnHome"))
	assert.Equal(t, "0", posts[0].Query.Get("promotedToSharedHome"))
	assert.Contains(t, out.Message, "visibility")

	s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	assert.Empty(t, f.writesTo("PUT "+hubPath), "in step")

	spec.Visibility = &collections.Visibility{FriendsHome: true}
	s.Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
	puts := f.writesTo("PUT " + hubPath + "/custom.collection.2." + rk)
	require.Len(t, puts, 1)
	assert.Equal(t, "0", puts[0].Query.Get("promotedToRecommended"))
	assert.Equal(t, "1", puts[0].Query.Get("promotedToSharedHome"))
	assert.Len(t, f.writesTo("POST "+hubPath), 1)
}

func TestARefusedImageHoldsBackNothingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		body []byte
		ct   string
		fail bool
	}{
		"Plex refuses the upload":  {body: posterBytes, ct: "image/png", fail: true},
		"an SVG sent as image/png": {body: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), ct: "image/png"},
		"bytes that are no image":  {body: []byte("not an image at all"), ct: "image/jpeg"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePMS(t)
			rk := f.add(fakeCollection{Section: "2", Title: "Netflix", Subtype: "show", Smart: true, Content: storedContent(t, "2", "Netflix"), Mode: -1})
			if tc.fail {
				f.failNext("POST /library/metadata/"+rk+"/posters", http.StatusBadRequest)
			}
			img := &images{bodies: map[string][]byte{posterURL: tc.body}, types: map[string]string{posterURL: tc.ct}}
			now := syncNow
			spec := netflix()
			spec.Poster = &collections.Image{URL: posterURL}
			spec.Settings = &collections.Settings{Mode: "hideCollection"}
			spec.Visibility = &collections.Visibility{Home: true}
			out := presentSyncer(f, img, &now).Sync(t.Context(), ns, spec, collections.Previous{RatingKey: rk})
			assert.Equal(t, collections.ReasonImageUnavailable, out.Reason, out.Message)
			assert.Contains(t, out.Message, "poster")
			assert.Len(t, f.writesTo("PUT /library/metadata/"+rk+"/prefs"), 1, "the mode is still set")
			assert.Len(t, f.writesTo("POST /hubs/sections/2/manage"), 1, "the visibility is still set")
			if !tc.fail {
				assert.Empty(t, f.writesTo("POST /library/metadata/"+rk+"/posters"), "nothing Plex cannot show is uploaded")
			}
		})
	}
}

func TestTheUploadCarriesTheSniffedType(t *testing.T) {
	f := newFakePMS(t)
	rk := f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	img := &images{bodies: map[string][]byte{posterURL: posterBytes}, types: map[string]string{posterURL: "image/jpeg"}}
	now := syncNow
	presentSyncer(f, img, &now).Sync(t.Context(), ns, upWithPoster(), collections.Previous{RatingKey: rk})
	ups := f.writesTo("POST /library/metadata/" + rk + "/posters")
	require.Len(t, ups, 1)
	assert.Equal(t, "image/png", ups[0].ContentType, "the bytes are a PNG whatever the header said")
}
