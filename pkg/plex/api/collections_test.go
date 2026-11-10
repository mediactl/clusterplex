package api_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// The fixtures under testdata/ were recorded on 2026-11-10: the
// collections, meta, genre and managed-hub answers from talos's PMS
// 1.43.5 (read-only GETs), the prefs and posters from a stock PMS
// 1.43.5.11029 container after an upload and a prefs change.
func collectionFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}

// served is one request as the test server saw it.
type served struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         []byte
}

// pms answers each "METHOD PATH" from bodies (a missing key answers 404)
// and records every request.
func pms(t *testing.T, bodies map[string]string) (*plexapi.Client, *[]served) {
	t.Helper()
	var got []served
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, served{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
		b, ok := bodies[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL, Token: func() string { return "tok" }}, &got
}

func TestCollectionsDecodesSmartContentAndGUID(t *testing.T) {
	c, _ := pms(t, map[string]string{
		"GET /library/sections/2/collections": collectionFixture(t, "collections-section2.json"),
		"GET /library/sections/1/collections": collectionFixture(t, "collections-section1.json"),
	})
	tv, err := c.Collections(t.Context(), "2")
	require.NoError(t, err)
	byTitle := map[string]plexapi.Collection{}
	for _, col := range tv {
		byTitle[col.Title] = col
	}
	ad, ok := byTitle["Anime Dubbed"]
	require.True(t, ok, "%+v", tv)
	assert.True(t, ad.Smart)
	assert.Equal(t, 259, ad.ChildCount)
	assert.True(t, strings.HasPrefix(ad.Content, "server://"), ad.Content)
	assert.True(t, strings.HasPrefix(ad.GUID, "collection://"), ad.GUID)
	assert.NotEmpty(t, ad.RatingKey)
	assert.Equal(t, "show", ad.Subtype)

	one, _ := pms(t, map[string]string{"GET /library/metadata/9": `{"MediaContainer":{"Metadata":[{"ratingKey":"9","title":"UP","summary":"Every Up film.","titleSort":"Up 00"}]}}`})
	col, err := one.Collection(t.Context(), "9")
	require.NoError(t, err)
	assert.Equal(t, "Every Up film.", col.Summary)
	assert.Equal(t, "Up 00", col.TitleSort)

	movies, err := c.Collections(t.Context(), "1")
	require.NoError(t, err)
	require.NotEmpty(t, movies)
	for _, col := range movies {
		assert.NotEmpty(t, col.RatingKey, col.Title)
	}
}

func TestSmartDecodesEveryShapePlexSends(t *testing.T) {
	for raw, want := range map[string]bool{`"1"`: true, `1`: true, `true`: true, `"0"`: false, `0`: false, `false`: false} {
		c, _ := pms(t, map[string]string{
			"GET /library/metadata/7": `{"MediaContainer":{"Metadata":[{"ratingKey":"7","title":"X","smart":` + raw + `}]}}`,
		})
		col, err := c.Collection(t.Context(), "7")
		require.NoError(t, err, raw)
		assert.Equal(t, want, col.Smart, raw)
	}
	c, _ := pms(t, map[string]string{"GET /library/metadata/7": `{"MediaContainer":{"Metadata":[{"ratingKey":"7","title":"X"}]}}`})
	col, err := c.Collection(t.Context(), "7")
	require.NoError(t, err)
	assert.False(t, col.Smart, "absent")
}

func TestCollectionMapsNotFound(t *testing.T) {
	c, _ := pms(t, nil)
	_, err := c.Collection(t.Context(), "404")
	require.ErrorIs(t, err, plexapi.ErrNotFound)

	c, _ = pms(t, map[string]string{"GET /library/metadata/5": `{"MediaContainer":{"size":0}}`})
	_, err = c.Collection(t.Context(), "5")
	require.ErrorIs(t, err, plexapi.ErrNotFound, "an empty answer is no collection")
}

func TestSectionMetaFieldsAndTagPaths(t *testing.T) {
	c, got := pms(t, map[string]string{
		"GET /library/sections/2/all": collectionFixture(t, "meta-section2.json"),
		"GET /library/sections/1/all": collectionFixture(t, "meta-section1.json"),
	})
	tv, err := c.SectionMeta(t.Context(), "2")
	require.NoError(t, err)
	assert.Equal(t, "tag", tv.Fields["show.genre"])
	assert.Equal(t, "audioLanguage", tv.Fields["episode.audioLanguage"])
	assert.Equal(t, "string", tv.Fields["show.title"])
	assert.Equal(t, "/library/sections/2/genre?type=2", tv.TagPaths["show.genre"])
	assert.Equal(t, "/library/sections/2/network?type=2", tv.TagPaths["show.network"])
	assert.Equal(t, "/library/sections/2/audioLanguage", tv.TagPaths["episode.audioLanguage"])
	assert.Empty(t, tv.TagPaths["show.title"], "a field with no filter has no path")
	assert.Contains(t, tv.Sorts, "titleSort")
	assert.Contains(t, tv.Sorts, "episode.addedAt")

	movies, err := c.SectionMeta(t.Context(), "1")
	require.NoError(t, err)
	assert.Equal(t, "tag", movies.Fields["genre"])
	assert.Equal(t, "/library/sections/1/genre", movies.TagPaths["genre"])

	req := (*got)[0]
	assert.Equal(t, "1", req.Query.Get("includeMeta"))
	assert.Equal(t, "0", req.Header.Get("X-Plex-Container-Start"))
	assert.Equal(t, "0", req.Header.Get("X-Plex-Container-Size"))
}

func TestTagValues(t *testing.T) {
	c, got := pms(t, map[string]string{"GET /library/sections/2/genre": collectionFixture(t, "genre-section2.json")})
	vals, err := c.TagValues(t.Context(), "/library/sections/2/genre?type=2")
	require.NoError(t, err)
	require.Len(t, vals, 64)
	assert.Equal(t, plexapi.TagValue{ID: "5377", Title: "Action"}, vals[0])
	assert.Equal(t, "2", (*got)[0].Query.Get("type"), "the path's own query is kept")
}

func TestCreateCollectionSendsTheURIOnce(t *testing.T) {
	const uri = "server://abc/com.plexapp.plugins.library/library/sections/2/all?type=2&sort=titleSort&push=1&show.genre=63543&or=1&show.genre=5377&pop=1"
	c, got := pms(t, map[string]string{
		"POST /library/collections": `{"MediaContainer":{"Metadata":[{"ratingKey":"901","title":"Anime Dubbed","subtype":"show","smart":"1","guid":"collection://x","content":"` + uri + `"}]}}`,
	})
	col, err := c.CreateCollection(t.Context(), "2", 2, "Anime Dubbed", uri, true)
	require.NoError(t, err)
	assert.Equal(t, "901", col.RatingKey)
	assert.True(t, col.Smart)

	q := (*got)[0].Query
	assert.Equal(t, []string{uri}, q["uri"])
	assert.Equal(t, "1", q.Get("smart"))
	assert.Equal(t, "2", q.Get("type"))
	assert.Equal(t, "2", q.Get("sectionId"))
	assert.Equal(t, "Anime Dubbed", q.Get("title"))

	_, err = c.CreateCollection(t.Context(), "1", 1, "Ghibli", "server://abc/com.plexapp.plugins.library/library/metadata/5,6", false)
	require.NoError(t, err)
	assert.Equal(t, "0", (*got)[1].Query.Get("smart"))
}

func TestCollectionItemCalls(t *testing.T) {
	const uri = "server://abc/com.plexapp.plugins.library/library/sections/1/all?type=1&title=Up"
	c, got := pms(t, map[string]string{
		"PUT /library/collections/30/items":       ``,
		"DELETE /library/collections/30/items/77": ``,
		"GET /library/collections/30/children":    `{"MediaContainer":{"totalSize":2,"Metadata":[{"ratingKey":"77","guid":"plex://movie/a"},{"ratingKey":"78","guid":"plex://movie/b"}]}}`,
	})
	require.NoError(t, c.SetCollectionFilter(t.Context(), "30", uri))
	require.NoError(t, c.AddCollectionItems(t.Context(), "30", "server://abc/com.plexapp.plugins.library/library/metadata/77,78"))
	require.NoError(t, c.RemoveCollectionItem(t.Context(), "30", "77"))
	items, err := c.CollectionChildren(t.Context(), "30")
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Item{{RatingKey: "77", GUID: "plex://movie/a"}, {RatingKey: "78", GUID: "plex://movie/b"}}, items)

	assert.Equal(t, []string{uri}, (*got)[0].Query["uri"])
	assert.Equal(t, http.MethodPut, (*got)[1].Method)
	assert.Equal(t, http.MethodDelete, (*got)[2].Method)
}

func TestSectionItemsPagesThroughTheSection(t *testing.T) {
	const total = 1001
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Size"))
		starts = append(starts, r.Header.Get("X-Plex-Container-Start"))
		assert.Equal(t, "1", r.URL.Query().Get("type"))
		var items []string
		for i := start; i < start+size && i < total; i++ {
			items = append(items, fmt.Sprintf(`{"ratingKey":"%d","guid":"plex://movie/%d"}`, i, i))
		}
		fmt.Fprintf(w, `{"MediaContainer":{"totalSize":%d,"offset":%d,"Metadata":[%s]}}`, total, start, strings.Join(items, ","))
	}))
	t.Cleanup(srv.Close)
	c := &plexapi.Client{BaseURL: srv.URL}

	items, err := c.SectionItems(t.Context(), "1", 1)
	require.NoError(t, err)
	require.Len(t, items, total)
	assert.Equal(t, plexapi.Item{RatingKey: "1000", GUID: "plex://movie/1000"}, items[1000])
	assert.Equal(t, []string{"0", "500", "1000"}, starts)
}

func TestEditCollectionLocksTheField(t *testing.T) {
	c, got := pms(t, map[string]string{"PUT /library/sections/2/all": ``})
	require.NoError(t, c.EditCollection(t.Context(), "2", "901", "summary", "Every dub."))
	q := (*got)[0].Query
	assert.Equal(t, "18", q.Get("type"))
	assert.Equal(t, "901", q.Get("id"))
	assert.Equal(t, "Every dub.", q.Get("summary.value"))
	assert.Equal(t, "1", q.Get("summary.locked"))

	require.Error(t, c.EditCollection(t.Context(), "2", "901", "thumb", "x"), "only title, summary and titleSort")
	assert.Len(t, *got, 1)
}

func TestCollectionPrefsReadsModeAndOrder(t *testing.T) {
	c, _ := pms(t, map[string]string{
		"GET /library/metadata/30": collectionFixture(t, "collection-prefs.json"),
		"GET /library/metadata/1":  `{"MediaContainer":{"Metadata":[{"ratingKey":"1","Preferences":{"Setting":[{"id":"collectionMode","value":"-1"}]}}]}}`,
	})
	p, err := c.CollectionPrefs(t.Context(), "30")
	require.NoError(t, err)
	assert.Equal(t, plexapi.CollectionPrefs{Mode: 0, Order: 1}, p)

	p, err = c.CollectionPrefs(t.Context(), "1")
	require.NoError(t, err)
	assert.Equal(t, plexapi.CollectionPrefs{Mode: -1, Order: -1}, p, "a smart collection has no order")
}

func TestSetCollectionPrefsSendsOnlyWhatIsGiven(t *testing.T) {
	c, got := pms(t, map[string]string{"PUT /library/metadata/30/prefs": ``})
	mode, order := 1, 0
	require.NoError(t, c.SetCollectionPrefs(t.Context(), "30", &mode, nil))
	require.NoError(t, c.SetCollectionPrefs(t.Context(), "30", &mode, &order))
	require.NoError(t, c.SetCollectionPrefs(t.Context(), "30", nil, nil))
	require.Len(t, *got, 2, "nothing to set sends nothing")
	assert.Equal(t, url.Values{"collectionMode": {"1"}}, (*got)[0].Query)
	assert.Equal(t, url.Values{"collectionMode": {"1"}, "collectionSort": {"0"}}, (*got)[1].Query)
}

func TestPostersParsesUploadSHA1(t *testing.T) {
	c, _ := pms(t, map[string]string{"GET /library/metadata/30/posters": collectionFixture(t, "posters.json")})
	ps, err := c.Posters(t.Context(), "30", "posters")
	require.NoError(t, err)
	require.Len(t, ps, 2)
	assert.True(t, ps[0].Selected)
	assert.Equal(t, "e9dfa0063251cdcf337791147bdfac4e4ea62df8", ps[0].SHA1)
	assert.False(t, ps[1].Selected)
	assert.Empty(t, ps[1].SHA1, "a composite is no upload")

	_, err = c.Posters(t.Context(), "30", "thumbs")
	require.Error(t, err)
}

func TestUploadImageSendsTheBytes(t *testing.T) {
	c, got := pms(t, map[string]string{"POST /library/metadata/30/arts": ``})
	img := []byte("\x89PNG\r\n\x1a\nnot really")
	require.NoError(t, c.UploadImage(t.Context(), "30", "arts", img, "image/png"))
	require.Len(t, *got, 1)
	assert.Equal(t, img, (*got)[0].Body)
	assert.Equal(t, "image/png", (*got)[0].Header.Get("Content-Type"))
	assert.Equal(t, "tok", (*got)[0].Header.Get("X-Plex-Token"))

	require.Error(t, c.UploadImage(t.Context(), "30", "thumbs", img, "image/png"))
	require.Error(t, c.UploadImage(t.Context(), "30", "arts", bytes.Repeat([]byte{0}, plexapi.MaxImageBytes+1), "image/png"))
	assert.Len(t, *got, 1, "a refused upload sends nothing")
}

func TestManagedHubsDecodes(t *testing.T) {
	c, got := pms(t, map[string]string{
		"GET /hubs/sections/2/manage":                         collectionFixture(t, "managed-hubs-section2.json"),
		"POST /hubs/sections/2/manage":                        ``,
		"PUT /hubs/sections/2/manage/custom.collection.2.901": ``,
	})
	hubs, err := c.ManagedHubs(t.Context(), "2")
	require.NoError(t, err)
	var recent *plexapi.ManagedHub
	for i := range hubs {
		if hubs[i].Identifier == "tv.recentlyadded" {
			recent = &hubs[i]
		}
	}
	require.NotNil(t, recent)
	assert.Equal(t, plexapi.ManagedHub{Identifier: "tv.recentlyadded", Recommended: true, OwnHome: true, SharedHome: true}, *recent)

	require.NoError(t, c.CreateManagedHub(t.Context(), "2", "901", plexapi.ManagedHub{Recommended: true, OwnHome: true}))
	q := (*got)[1].Query
	assert.Equal(t, "901", q.Get("metadataItemId"))
	assert.Equal(t, "1", q.Get("promotedToRecommended"))
	assert.Equal(t, "1", q.Get("promotedToOwnHome"))
	assert.Equal(t, "0", q.Get("promotedToSharedHome"))

	require.NoError(t, c.UpdateManagedHub(t.Context(), "2", plexapi.ManagedHub{Identifier: "custom.collection.2.901", SharedHome: true}))
	q = (*got)[2].Query
	assert.Equal(t, http.MethodPut, (*got)[2].Method)
	assert.Equal(t, "0", q.Get("promotedToRecommended"))
	assert.Equal(t, "1", q.Get("promotedToSharedHome"))
}

func TestDeleteCollectionTreats404AsDeleted(t *testing.T) {
	c, got := pms(t, map[string]string{"DELETE /library/collections/30": ``})
	require.NoError(t, c.DeleteCollection(t.Context(), "30"))
	require.NoError(t, c.DeleteCollection(t.Context(), "31"), "gone already")
	assert.Equal(t, http.MethodDelete, (*got)[1].Method)
}
