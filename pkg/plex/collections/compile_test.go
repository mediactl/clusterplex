package collections_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

func strp(s string) *string { return &s }
func int32p(n int32) *int32 { return &n }

// talosMachine is talos's PMS machine identifier, as its stored filters
// carry it (../api/testdata, recorded 2026-11-10).
const talosMachine = "ae5887ec46d6bfac69927015acffa8941a904368"

// recordedMeta is a section's Meta as talos's PMS 1.43.5 answered it, read
// through the api client.
func recordedMeta(t *testing.T, section string) api.SectionMeta {
	t.Helper()
	b, err := os.ReadFile("../api/testdata/meta-section" + section + ".json")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) }))
	t.Cleanup(srv.Close)
	m, err := (&api.Client{BaseURL: srv.URL}).SectionMeta(t.Context(), section)
	require.NoError(t, err)
	return m
}

// storedContent is the filter a talos collection holds, by title.
func storedContent(t *testing.T, section, title string) string {
	t.Helper()
	b, err := os.ReadFile("../api/testdata/collections-section" + section + ".json")
	require.NoError(t, err)
	var doc struct {
		MediaContainer struct {
			Metadata []struct{ Title, Content string }
		}
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	for _, m := range doc.MediaContainer.Metadata {
		if m.Title == title {
			return m.Content
		}
	}
	t.Fatalf("no collection %q in section %s", title, section)
	return ""
}

// talosTags is the resolver with talos's tag ids.
func talosTags(field, name string) (string, error) {
	return map[string]string{"show.genre/Anime": "6437", "show.network/Netflix": "6787", "genre/Documentary": "5836"}[field+"/"+name], nil
}

func animeDubbed() collections.Spec {
	return collections.Spec{
		Library: "Clustarr TV", Title: "Anime Dubbed", ItemType: "show",
		Filter: &collections.Filter{Match: "all", Rules: []collections.Rule{
			{Field: "episode.audioLanguage", Operator: "is", Value: "en"},
			{Field: "show.genre", Operator: "is", Value: "Anime"},
		}},
		Sort: &collections.Sort{Field: "titleSort"},
	}
}

func netflix() collections.Spec {
	return collections.Spec{
		Library: "Clustarr TV", Title: "Netflix", ItemType: "show",
		Filter: &collections.Filter{Match: "all", Rules: []collections.Rule{{Field: "show.network", Operator: "is", Value: "Netflix"}}},
	}
}

func up() collections.Spec {
	return collections.Spec{
		Library: "Clustarr Movies", Title: "UP", ItemType: "movie",
		Filter: &collections.Filter{Match: "any", Rules: []collections.Rule{
			{Field: "title", Operator: "is", Value: "Seven Up!"},
			{Match: "all", Rules: []collections.Condition{
				{Field: "genre", Operator: "is", Value: "Documentary"},
				{Field: "title", Operator: "contains", Value: "Up"},
			}},
		}},
	}
}

func compile(t *testing.T, s collections.Spec, section string, carry collections.Carry) (string, error) {
	t.Helper()
	return collections.Compile(*s.Filter, s.Sort, s.Limit, s.ItemType, recordedMeta(t, section), talosTags, carry)
}

func TestCompileReferences(t *testing.T) {
	for _, tc := range []struct {
		spec          collections.Spec
		section, want string
	}{
		{animeDubbed(), "2", "type=2&sort=titleSort&episode.audioLanguage=en&and=1&show.genre=6437"},
		{netflix(), "2", "type=2&show.network=6787"},
		{up(), "1", "type=1&push=1&push=1&title==Seven Up!&pop=1&or=1&push=1&genre=5836&and=1&title=Up&pop=1&pop=1"},
	} {
		q, err := compile(t, tc.spec, tc.section, collections.Carry{})
		require.NoError(t, err, tc.spec.Title)
		assert.Equal(t, tc.want, collections.Decode(q), tc.spec.Title)
		// Byte for byte what Plex Web stored for the same collection.
		assert.Equal(t, storedContent(t, tc.section, tc.spec.Title), collections.SmartURI(talosMachine, tc.section, q), tc.spec.Title)
	}
}

func TestCompileCarriesPlexSortWhenSpecHasNone(t *testing.T) {
	q, err := compile(t, up(), "1", collections.Carry{Sort: "addedAt:desc", Limit: "25"})
	require.NoError(t, err)
	d := collections.Decode(q)
	// Plex Web joins {type, sort, group, having, limit} and then the
	// filter (its DirectoryListAdvancedFilters, 4.162.1).
	assert.True(t, strings.HasPrefix(d, "type=1&sort=addedAt:desc&limit=25&push=1&"), d)
	assert.True(t, strings.HasSuffix(d, "&pop=1"), d)

	// The spec's own sort and limit win.
	s := up()
	s.Sort, s.Limit = &collections.Sort{Field: "rating", Descending: true}, int32p(10)
	q, err = compile(t, s, "1", collections.Carry{Sort: "addedAt:desc", Limit: "25"})
	require.NoError(t, err)
	d = collections.Decode(q)
	assert.True(t, strings.HasPrefix(d, "type=1&sort=rating:desc&limit=10&push=1&"), d)
}

func TestCompileEncodesSpecialCharacters(t *testing.T) {
	s := collections.Spec{ItemType: "movie", Filter: &collections.Filter{Match: "any", Rules: []collections.Rule{
		{Field: "title", Operator: "contains", Value: "Tom & Jerry"},
		{Field: "title", Operator: "is", Value: "Pokémon"},
		{Field: "title", Operator: "beginsWith", Value: "a=b%c+d"},
	}}}
	q, err := compile(t, s, "1", collections.Carry{})
	require.NoError(t, err)
	uri := collections.SmartURI("abc", "1", q)
	_, rawQuery, ok := strings.Cut(uri, "?")
	require.True(t, ok, uri)
	for _, part := range strings.Split(rawQuery, "&") {
		assert.Equal(t, 1, strings.Count(part, "="), "a bare & or = inside a value: %q in %s", part, uri)
	}
	back, carry := collections.ParseContent(uri)
	assert.Equal(t, q, back)
	assert.Equal(t, collections.Carry{}, carry)
	d := collections.Decode(back)
	for _, want := range []string{"title=Tom & Jerry", "title==Pokémon", "title<=a=b%c+d"} {
		assert.Contains(t, d, want)
	}
}

func TestCompileTypedOperators(t *testing.T) {
	s := collections.Spec{ItemType: "movie", Filter: &collections.Filter{Match: "all", Rules: []collections.Rule{
		{Field: "hdr", Operator: "isTrue"},
		{Field: "dovi", Operator: "isFalse"},
		{Field: "year", Operator: "greaterThan", Value: "1999"},
		{Field: "addedAt", Operator: "after", Value: "-6w"},
		{Field: "title", Operator: "isNot", Value: "Up"},
		{Field: "audioLanguage", Operator: "isNot", Value: "ja"},
	}}}
	q, err := compile(t, s, "1", collections.Carry{})
	require.NoError(t, err)
	assert.Equal(t, "type=1&hdr=1&and=1&dovi!=1&and=1&year>>=1999&and=1&addedAt>>=-6w&and=1&title!==Up&and=1&audioLanguage!=ja", collections.Decode(q))
}

func TestCompileRejects(t *testing.T) {
	m := recordedMeta(t, "1")
	one := func(r collections.Rule) collections.Filter {
		return collections.Filter{Match: "all", Rules: []collections.Rule{r}}
	}
	for name, f := range map[string]collections.Filter{
		"an unknown field":           one(collections.Rule{Field: "nonsense", Operator: "is", Value: "x"}),
		"an operator its type lacks": one(collections.Rule{Field: "genre", Operator: "contains", Value: "Doc"}),
		"a TV field in a movie list": one(collections.Rule{Field: "show.network", Operator: "is", Value: "Netflix"}),
		"a condition with no value":  one(collections.Rule{Field: "title", Operator: "is"}),
		"a group in a group's place": {Match: "all", Rules: []collections.Rule{{Match: "all"}}},
		"no rules":                   {Match: "all"},
		"a rule both kinds":          one(collections.Rule{Field: "title", Operator: "is", Value: "x", Match: "any"}),
		"a match that is neither":    {Match: "some", Rules: []collections.Rule{{Field: "title", Operator: "is", Value: "x"}}},
	} {
		_, err := collections.Compile(f, nil, nil, "movie", m, talosTags, collections.Carry{})
		assert.ErrorIs(t, err, collections.ErrInvalidRule, name)
	}
	_, err := collections.Compile(one(collections.Rule{Field: "title", Operator: "is", Value: "x"}), nil, nil, "album", m, talosTags, collections.Carry{})
	assert.ErrorIs(t, err, collections.ErrInvalidRule, "an unknown item type")

	_, err = collections.Compile(one(collections.Rule{Field: "genre", Operator: "is", Value: "Westerns"}), nil, nil, "movie", m, talosTags, collections.Carry{})
	require.ErrorIs(t, err, collections.ErrUnknownValue)
	assert.Contains(t, err.Error(), "Westerns")

	down := errors.New("plex: unavailable")
	_, err = collections.Compile(one(collections.Rule{Field: "genre", Operator: "is", Value: "Documentary"}), nil, nil, "movie", m,
		func(string, string) (string, error) { return "", down }, collections.Carry{})
	require.ErrorIs(t, err, down)
	assert.NotErrorIs(t, err, collections.ErrUnknownValue)
}

func TestParseContentCarriesSortAndLimit(t *testing.T) {
	q, carry := collections.ParseContent(storedContent(t, "1", "A24"))
	assert.Equal(t, collections.Carry{Sort: "rating:desc"}, carry)
	assert.Equal(t, "type=1&sort=rating:desc&studio=A24", collections.Decode(q))

	_, carry = collections.ParseContent("server://abc/com.plexapp.plugins.library/library/sections/1/all?type=1&genre=5836&limit=20")
	assert.Equal(t, collections.Carry{Limit: "20"}, carry)

	q, carry = collections.ParseContent("")
	assert.Empty(t, q)
	assert.Equal(t, collections.Carry{}, carry)
}

func TestItemsURI(t *testing.T) {
	assert.Equal(t, "server://abc/com.plexapp.plugins.library/library/metadata/2,3", collections.ItemsURI("abc", []string{"2", "3"}))
}

func TestItemTypeNumber(t *testing.T) {
	for in, want := range map[string]int{"movie": 1, "show": 2, "season": 3, "episode": 4, "album": 0} {
		assert.Equal(t, want, collections.ItemTypeNumber(in), in)
	}
}

func TestSpecOfReadsTheResource(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "PlexCollection",
		"metadata": map[string]any{"name": "ghibli", "namespace": "clustarr-system"},
		"spec": map[string]any{
			"library": "Clustarr Movies", "title": "Studio Ghibli Essentials", "itemType": "movie",
			"members":    map[string]any{"items": []any{map[string]any{"kind": "Movie", "name": "spirited-away-2001"}}},
			"summary":    "Every Ghibli film.",
			"poster":     map[string]any{"url": "https://image.tmdb.org/t/p/original/p.jpg"},
			"settings":   map[string]any{"mode": "hideItems", "order": "releaseDate"},
			"visibility": map[string]any{"libraryRecommended": true, "home": false, "friendsHome": false},
			"limit":      int64(5),
		},
	}}
	s, err := collections.SpecOf(u)
	require.NoError(t, err)
	assert.Equal(t, collections.Spec{
		Library: "Clustarr Movies", Title: "Studio Ghibli Essentials", ItemType: "movie",
		Members:    &collections.Members{Items: []collections.Member{{Kind: "Movie", Name: "spirited-away-2001"}}},
		Summary:    strp("Every Ghibli film."),
		Poster:     &collections.Image{URL: "https://image.tmdb.org/t/p/original/p.jpg"},
		Settings:   &collections.Settings{Mode: "hideItems", Order: "releaseDate"},
		Visibility: &collections.Visibility{LibraryRecommended: true},
		Limit:      int32p(5),
	}, s)

	_, err = collections.SpecOf(&unstructured.Unstructured{Object: map[string]any{"spec": "nonsense"}})
	assert.Error(t, err)
}
