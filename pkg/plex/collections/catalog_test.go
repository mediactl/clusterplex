package collections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

// The Up Collection as clustarr's movies provider answered it on talos
// (2026-11-17): the collection under its Plex id, TMDB's poster and
// background as the provider hands them to PMS, and the library's movies
// oldest first under the GUIDs the projection keys their Plex items by.
const (
	upPlexID     = "5ec2ebb41ef0460041f94f16"
	upPosterURL  = "https://image.tmdb.org/t/p/w500/aPV1Ecrkl8VXJZrcIXHXUDQeH2H.jpg"
	upArtURL     = "https://image.tmdb.org/t/p/original/ayvfXaNjANcBknVtRuYaIFJqJDN.jpg"
	upSummary    = "The Up Series is a series of documentary films produced by Granada Television."
	upCollection = int64(95051)
)

var upFilms = []collections.CatalogItem{
	{GUID: "plex://movie/5d776890f617c90020155508", Title: "Seven Up!", Year: 1964},
	{GUID: "plex://movie/5d77688b51dd69001fe0de50", Title: "7 Plus Seven", Year: 1970},
	{GUID: "plex://movie/5d776846880197001ec9674d", Title: "21 Up", Year: 1977},
}

// provider is clustarr's Plex metadata provider for one root: the TMDB
// collections it holds, paged pageSize at a time whatever is asked.
type provider struct {
	collections map[int64]collections.CatalogCollection
	pageSize    int
	status      int // non-zero answers every request with it
	gets        []string
}

func (p *provider) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	type md = map[string]any
	write := func(w http.ResponseWriter, total int, items []md) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(md{"MediaContainer": md{"totalSize": total, "size": len(items), "Metadata": items}})
	}
	find := func(key string) (collections.CatalogCollection, bool) {
		for id, c := range p.collections {
			if key == "tmdb-collection-"+strconv.FormatInt(id, 10) || key == upPlexID {
				return c, true
			}
		}
		return collections.CatalogCollection{}, false
	}
	mux.HandleFunc("GET /plex/movies/library/collections/{key}", func(w http.ResponseWriter, r *http.Request) {
		p.gets = append(p.gets, r.URL.Path)
		c, ok := find(r.PathValue("key"))
		if p.status != 0 {
			http.Error(w, "down", p.status)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		write(w, 1, []md{{
			"ratingKey": upPlexID, "key": "/library/collections/" + upPlexID + "/children", "guid": "plex://collection/" + upPlexID,
			"type": "collection", "title": c.Title, "summary": c.Summary, "thumb": c.Poster, "art": c.Background,
		}})
	})
	mux.HandleFunc("GET /plex/movies/library/collections/{key}/children", func(w http.ResponseWriter, r *http.Request) {
		p.gets = append(p.gets, r.URL.Path+"@"+r.Header.Get("X-Plex-Container-Start"))
		c, ok := find(r.PathValue("key"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		start, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Start"))
		end := min(len(c.Items), start+p.pageSize)
		var items []md
		for _, it := range c.Items[min(start, len(c.Items)):end] {
			items = append(items, md{
				"guid": it.GUID, "Guid": []md{{"id": "imdb://tt0058578"}, {"id": "tmdb://51863"}},
				"title": it.Title, "year": it.Year, "type": "movie",
			})
		}
		write(w, len(c.Items), items)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func upProvider() *provider {
	return &provider{pageSize: 200, collections: map[int64]collections.CatalogCollection{
		upCollection: {Title: "The Up Collection", Summary: upSummary, Poster: upPosterURL, Background: upArtURL, Items: upFilms},
	}}
}

func TestTheProviderCatalogReadsACollectionAndItsMovies(t *testing.T) {
	p := upProvider()
	srv := p.server(t)
	// The TV provider comes first and says 404, as clustarr's does.
	cat := collections.ProviderCatalog{Providers: []string{srv.URL + "/plex/tv", srv.URL + "/plex/movies/"}}

	got, err := cat.TMDBCollection(t.Context(), upCollection)
	require.NoError(t, err)
	assert.Equal(t, collections.CatalogCollection{
		Title: "The Up Collection", Summary: upSummary, Poster: upPosterURL, Background: upArtURL, Items: upFilms,
	}, got)
	assert.Contains(t, p.gets, "/plex/movies/library/collections/"+upPlexID+"/children@0",
		"the movies are listed at the key the collection was answered with, its Plex id")
}

func TestTheProviderCatalogReadsEveryPage(t *testing.T) {
	p := upProvider()
	p.pageSize = 2
	got, err := collections.ProviderCatalog{Providers: []string{p.server(t).URL + "/plex/movies"}}.TMDBCollection(t.Context(), upCollection)
	require.NoError(t, err)
	assert.Equal(t, upFilms, got.Items)
}

func TestTheProviderCatalogTellsNoSuchCollectionFromAProviderThatIsDown(t *testing.T) {
	p := upProvider()
	srv := p.server(t)
	cat := collections.ProviderCatalog{Providers: []string{srv.URL + "/plex/movies"}}

	_, err := cat.TMDBCollection(t.Context(), 4438)
	assert.ErrorIs(t, err, collections.ErrNoSuchCollection, "404 from every provider")

	p.status = http.StatusServiceUnavailable
	_, err = cat.TMDBCollection(t.Context(), upCollection)
	require.Error(t, err)
	assert.NotErrorIs(t, err, collections.ErrNoSuchCollection, "a 503 is no answer")

	_, err = collections.ProviderCatalog{}.TMDBCollection(t.Context(), upCollection)
	require.Error(t, err)
	assert.NotErrorIs(t, err, collections.ErrNoSuchCollection, "no provider configured is no answer either")
}

// catalog is a Catalog over a fixed map, or a fixed error.
type catalog struct {
	m     map[int64]collections.CatalogCollection
	err   error
	calls int
}

func (c *catalog) TMDBCollection(_ context.Context, id int64) (collections.CatalogCollection, error) {
	c.calls++
	if c.err != nil {
		return collections.CatalogCollection{}, c.err
	}
	col, ok := c.m[id]
	if !ok {
		return collections.CatalogCollection{}, collections.ErrNoSuchCollection
	}
	return col, nil
}

func upCatalog(films ...collections.CatalogItem) *catalog {
	return &catalog{m: map[int64]collections.CatalogCollection{
		upCollection: {Title: "The Up Collection", Summary: upSummary, Poster: upPosterURL, Background: upArtURL, Items: films},
	}}
}

// tmdbUp is the resource: the collection by id, everything else TMDB's.
func tmdbUp() collections.Spec {
	return collections.Spec{
		Library: "Clustarr Movies", ItemType: "movie",
		TMDBCollection: &collections.TMDBCollection{ID: upCollection},
		Settings:       &collections.Settings{Mode: "hideItems"},
	}
}

func tmdbSyncer(f *fakePMS, c *catalog, img *images) *collections.Syncer {
	s := syncer(f)
	if c != nil {
		s.Catalog = c
	}
	if img != nil {
		s.Fetch = img.fetch
	}
	return s
}

func upImages() *images {
	return &images{bodies: map[string][]byte{upPosterURL: posterBytes, upArtURL: []byte("\x89PNG\r\n\x1a\nthe UP background")}}
}

func TestATMDBCollectionIsANormalCollectionOfTheLibrarysMovies(t *testing.T) {
	f := newFakePMS(t)
	// Plex holds two of the three films; 21 Up is not scanned in yet.
	f.setItems("1", "1",
		api.Item{RatingKey: "737", GUID: upFilms[0].GUID},
		api.Item{RatingKey: "18", GUID: upFilms[1].GUID},
		api.Item{RatingKey: "99", GUID: "plex://movie/unrelated"})
	img := upImages()
	s := tmdbSyncer(f, upCatalog(upFilms...), img)

	out := s.Sync(t.Context(), ns, tmdbUp(), collections.Previous{})
	require.Equal(t, collections.ReasonMembersPending, out.Reason, out.Message)
	posts := f.writesTo("POST /library/collections")
	require.Len(t, posts, 1)
	assert.Equal(t, "0", posts[0].Query.Get("smart"), "a normal collection, whose items Plex can hide")
	assert.Equal(t, "The Up Collection", posts[0].Query.Get("title"), "TMDB's title, the resource stating none")
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"737", "18"}), posts[0].Query.Get("uri"))
	assert.Equal(t, 1, out.MissingMembers)
	assert.Equal(t, []collections.Member{{Kind: "Movie", Name: "21 Up"}}, out.Missing, "a film Plex lacks is named by its title")

	col := f.col(out.RatingKey)
	require.NotNil(t, col)
	assert.Equal(t, upSummary, col.Summary, "TMDB's summary")
	require.NotNil(t, out.Poster)
	assert.Equal(t, upPosterURL, out.Poster.URL, "TMDB's poster is the collection's")
	require.NotNil(t, out.Background)
	assert.Equal(t, upArtURL, out.Background.URL)
	assert.Len(t, f.writesTo("POST /library/metadata/"+out.RatingKey+"/posters"), 1)
	assert.Len(t, f.writesTo("POST /library/metadata/"+out.RatingKey+"/arts"), 1)

	// 21 Up is scanned in; Seven Up! leaves the library, and with it clustarr's list.
	f.setItems("1", "1",
		api.Item{RatingKey: "737", GUID: upFilms[0].GUID},
		api.Item{RatingKey: "18", GUID: upFilms[1].GUID},
		api.Item{RatingKey: "8", GUID: upFilms[2].GUID})
	s.Catalog = upCatalog(upFilms[1], upFilms[2])
	s.Forget()
	out = s.Sync(t.Context(), ns, tmdbUp(), collections.Previous{RatingKey: out.RatingKey, Poster: out.Poster, Background: out.Background})
	assert.Equal(t, collections.ReasonSynced, out.Reason, out.Message)
	assert.Zero(t, out.MissingMembers)
	puts := f.writesTo("PUT /library/collections/" + out.RatingKey + "/items")
	require.Len(t, puts, 1)
	assert.Equal(t, collections.ItemsURI(talosMachine, []string{"8"}), puts[0].Query.Get("uri"))
	dels := f.writesTo("DELETE /library/collections/" + out.RatingKey + "/items/")
	require.Len(t, dels, 1)
	assert.Equal(t, "/library/collections/"+out.RatingKey+"/items/737", dels[0].Path)
	assert.Equal(t, 2, img.calls, "the images were uploaded once and are left")
}

func TestATMDBCollectionKeepsWhatTheResourceStates(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "737", GUID: upFilms[0].GUID})
	spec := tmdbUp()
	spec.Title = "Up"
	spec.Summary = new(string)
	*spec.Summary = "Every seven years."
	spec.Poster = &collections.Image{URL: posterURL}
	img := upImages()
	img.bodies[posterURL] = posterBytes

	out := tmdbSyncer(f, upCatalog(upFilms...), img).Sync(t.Context(), ns, spec, collections.Previous{})
	require.Equal(t, collections.ReasonMembersPending, out.Reason, out.Message)
	col := f.col(out.RatingKey)
	require.NotNil(t, col)
	assert.Equal(t, "Up", col.Title)
	assert.Equal(t, "Every seven years.", col.Summary)
	assert.Equal(t, posterURL, out.Poster.URL, "the stated poster, not TMDB's")
	assert.Equal(t, upArtURL, out.Background.URL, "the background left out is TMDB's")
}

func TestATMDBCollectionChangesNothingWithoutAnAnswer(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "737", GUID: upFilms[0].GUID}, api.Item{RatingKey: "18", GUID: upFilms[1].GUID})
	rk := f.add(fakeCollection{Section: "1", Title: "The Up Collection", Subtype: "movie", Children: []string{"737", "18"}, ChildCount: 2})
	prev := collections.Previous{RatingKey: rk}
	writes := func() int { return len(f.writesTo("POST")) + len(f.writesTo("PUT")) + len(f.writesTo("DELETE")) }

	out := tmdbSyncer(f, &catalog{err: errors.New("dial tcp: connection refused")}, nil).Sync(t.Context(), ns, tmdbUp(), prev)
	assert.Equal(t, collections.ReasonCatalogUnavailable, out.Reason, out.Message)
	assert.Equal(t, rk, out.RatingKey, "the collection it holds stays recorded")

	out = tmdbSyncer(f, &catalog{}, nil).Sync(t.Context(), ns, tmdbUp(), prev)
	assert.Equal(t, collections.ReasonWaiting, out.Reason, out.Message)
	assert.Contains(t, out.Message, "95051")

	out = tmdbSyncer(f, nil, nil).Sync(t.Context(), ns, tmdbUp(), prev)
	assert.Equal(t, collections.ReasonCatalogUnavailable, out.Reason, out.Message)

	shows := tmdbUp()
	shows.Library, shows.ItemType = "Clustarr TV", "show"
	out = tmdbSyncer(f, upCatalog(upFilms...), nil).Sync(t.Context(), ns, shows, collections.Previous{})
	assert.Equal(t, collections.ReasonInvalidRule, out.Reason, out.Message)

	assert.Zero(t, writes(), "none of them wrote to Plex")

	// A collection clustarr answers with no movie in it is presented, and
	// never emptied: nothing is removed when no member is in Plex.
	out = tmdbSyncer(f, upCatalog(), upImages()).Sync(t.Context(), ns, tmdbUp(), prev)
	assert.True(t, collections.Healthy(out.Reason), out.Message)
	assert.Empty(t, f.writesTo("DELETE"))
	assert.Equal(t, []string{"737", "18"}, f.col(rk).Children)
}

func TestTwoFilmsOfOneTitleAreToldApartInTheStatus(t *testing.T) {
	f := newFakePMS(t)
	f.setItems("1", "1", api.Item{RatingKey: "1", GUID: "plex://movie/x"})
	films := []collections.CatalogItem{
		{GUID: "plex://movie/x", Title: "Dune", Year: 2021},
		{GUID: "plex://movie/a", Title: "Solaris", Year: 1972},
		{GUID: "plex://movie/b", Title: "Solaris", Year: 2002},
	}
	out := tmdbSyncer(f, upCatalog(films...), upImages()).Sync(t.Context(), ns, tmdbUp(), collections.Previous{})
	assert.Equal(t, []collections.Member{{Kind: "Movie", Name: "Solaris"}, {Kind: "Movie", Name: fmt.Sprintf("Solaris (%d)", 2002)}}, out.Missing)
}

// Two TMDB collections that take TMDB's title are the same collection only
// when they name the same one: an empty title is no title to share.
func TestUntitledTMDBCollectionsShareOnlyTheirID(t *testing.T) {
	res := func(name, title string, id int64) *unstructured.Unstructured {
		spec := map[string]any{"library": "Clustarr Movies", "itemType": "movie"}
		if title != "" {
			spec["title"] = title
		}
		if id != 0 {
			spec["tmdbCollection"] = map[string]any{"id": id}
		}
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}, "spec": spec}}
	}
	holder := func(u *unstructured.Unstructured, all ...*unstructured.Unstructured) string {
		spec, err := collections.SpecOf(u)
		require.NoError(t, err)
		return collections.FirstHolder(u, spec, append(all, u))
	}
	up, alien := res("up", "", 95051), res("alien", "", 8091)
	assert.Empty(t, holder(alien, up), "two collections, neither titled")
	assert.Equal(t, "up", holder(res("up-again", "", 95051), up), "the same collection twice")
	assert.Empty(t, holder(res("titled", "The Up Collection", 95051), up), "a title of its own against none: the sync finds out what Plex holds")
	assert.Empty(t, holder(up, res("smart", "UP", 0)), "a smart collection of another title")
	assert.Equal(t, "smart", holder(res("normal", "up", 0), res("smart", "UP", 0)), "titles still match in any case")
}

// clustarr's own answers for The Up Collection, recorded on talos
// (2026-11-17; testdata/provider, the host renamed): a movie carries
// "guid" and "Guid" both, which the first cut read as one field and
// refused the whole collection for.
func TestTheProviderCatalogReadsClustarrsRecordedAnswers(t *testing.T) {
	mux := http.NewServeMux()
	serve := func(pattern, file string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			raw, err := os.ReadFile("testdata/provider/" + file)
			require.NoError(t, err)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		})
	}
	serve("GET /plex/movies/library/collections/tmdb-collection-95051", "up-collection.json")
	serve("GET /plex/movies/library/collections/"+upPlexID+"/children", "up-children.json")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got, err := collections.ProviderCatalog{Providers: []string{srv.URL + "/plex/tv", srv.URL + "/plex/movies"}}.TMDBCollection(t.Context(), upCollection)
	require.NoError(t, err)
	assert.Equal(t, "The Up Collection", got.Title)
	assert.Equal(t, upPosterURL, got.Poster, "TMDB's own poster for the collection")
	assert.Equal(t, upArtURL, got.Background)
	assert.Contains(t, got.Summary, "Granada Television")
	require.Len(t, got.Items, 9)
	assert.Equal(t, collections.CatalogItem{GUID: "plex://movie/5d776890f617c90020155508", Title: "Seven Up!", Year: 1964}, got.Items[0])
	assert.Equal(t, collections.CatalogItem{GUID: "plex://movie/5eb8f2743f398b00402fc2dc", Title: "63 Up", Year: 2019}, got.Items[8])
}
