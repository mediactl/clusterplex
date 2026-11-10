package collections

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mediactl/clusterplex/pkg/plex/api"
)

// The Synced condition's reasons (the design's Status). Synced, Created,
// Adopted and MembersPending read True; the rest False.
const (
	ReasonSynced           = "Synced"
	ReasonCreated          = "Created"
	ReasonAdopted          = "Adopted"
	ReasonMembersPending   = "MembersPending"
	ReasonWaiting          = "Waiting"
	ReasonInvalidRule      = "InvalidRule"
	ReasonUnknownValue     = "UnknownValue"
	ReasonLibraryNotFound  = "LibraryNotFound"
	ReasonNotSmart         = "NotSmart"
	ReasonNotAdoptable     = "NotAdoptable"
	ReasonConflict         = "Conflict"
	ReasonImageUnavailable = "ImageUnavailable"
	ReasonPlexUnavailable  = "PlexUnavailable"
)

// Healthy says whether a reason reads Synced=True.
func Healthy(reason string) bool {
	switch reason {
	case ReasonSynced, ReasonCreated, ReasonAdopted, ReasonMembersPending:
		return true
	}
	return false
}

// PMS is the Plex Media Server API a Syncer uses; *api.Client is one.
type PMS interface {
	Sections(ctx context.Context) ([]api.Section, error)
	Collections(ctx context.Context, sectionKey string) ([]api.Collection, error)
	Collection(ctx context.Context, ratingKey string) (api.Collection, error)
	CreateCollection(ctx context.Context, sectionKey string, itemType int, title, uri string, smart bool) (api.Collection, error)
	SetCollectionFilter(ctx context.Context, ratingKey, uri string) error
	AddCollectionItems(ctx context.Context, ratingKey, uri string) error
	RemoveCollectionItem(ctx context.Context, ratingKey, itemRatingKey string) error
	CollectionChildren(ctx context.Context, ratingKey string) ([]api.Item, error)
	SectionItems(ctx context.Context, sectionKey string, itemType int) ([]api.Item, error)
	EditCollection(ctx context.Context, sectionKey, ratingKey, field, value string) error
	CollectionPrefs(ctx context.Context, ratingKey string) (api.CollectionPrefs, error)
	SetCollectionPrefs(ctx context.Context, ratingKey string, mode, order *int) error
	Posters(ctx context.Context, ratingKey, kind string) ([]api.Poster, error)
	UploadImage(ctx context.Context, ratingKey, kind string, body []byte, contentType string) error
	ManagedHubs(ctx context.Context, sectionKey string) ([]api.ManagedHub, error)
	CreateManagedHub(ctx context.Context, sectionKey, ratingKey string, v api.ManagedHub) error
	UpdateManagedHub(ctx context.Context, sectionKey string, v api.ManagedHub) error
	DeleteCollection(ctx context.Context, ratingKey string) error
	SectionMeta(ctx context.Context, sectionKey string) (api.SectionMeta, error)
	TagValues(ctx context.Context, path string) ([]api.TagValue, error)
}

var _ PMS = (*api.Client)(nil)

// MemberResolver gives the Plex GUID each member's clustarr Movie or
// Series carries; a member it cannot find is absent from the map.
type MemberResolver interface {
	GUIDs(ctx context.Context, namespace string, members []Member) (map[Member]string, error)
}

// ImageFetcher reads an image from its URL.
type ImageFetcher func(ctx context.Context, url string) (body []byte, contentType string, err error)

// ImageState is an image the resource's status records as uploaded.
type ImageState struct {
	URL, SHA1 string
	FetchedAt time.Time
}

// Previous is what the resource's status recorded at the last sync.
type Previous struct {
	RatingKey  string
	Adopted    bool
	Poster     *ImageState
	Background *ImageState
}

// Outcome is a sync's result, which the resource's status records.
type Outcome struct {
	RatingKey string
	Adopted   bool
	// Content is a smart collection's filter as a person reads it,
	// "library/sections/<n>/all?<decoded query>".
	Content                   string
	ItemCount, MissingMembers int
	Missing                   []Member
	Poster, Background        *ImageState
	Reason, Message           string
}

// Syncer brings Plex's collections in line with PlexCollection specs.
// Each section's Meta, tag lists and GUID index are cached until Forget.
type Syncer struct {
	PMS       PMS
	MachineID string
	Members   MemberResolver
	Fetch     ImageFetcher
	Now       func() time.Time

	mu    sync.Mutex
	metas map[string]api.SectionMeta
	tags  map[string]map[string]string   // tag path -> lower-case title -> id
	guids map[string]map[string][]string // "section/type" -> base GUID -> rating keys
}

// Forget drops the cached Meta, tag lists and GUID indexes: on Plex's
// start, and whenever the caller wants them read afresh.
func (s *Syncer) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metas, s.tags, s.guids = nil, nil, nil
}

// refusal is a sync stopped for a reason before Plex was changed.
type refusal struct{ reason, message string }

func (r refusal) Error() string { return r.message }

func refuse(reason, format string, a ...any) error {
	return refusal{reason, fmt.Sprintf(format, a...)}
}

// reasonOf is an error's condition reason: a refusal's own, a compile
// error's, else Plex being unavailable.
func reasonOf(err error) string {
	var r refusal
	switch {
	case errors.As(err, &r):
		return r.reason
	case errors.Is(err, ErrInvalidRule):
		return ReasonInvalidRule
	case errors.Is(err, ErrUnknownValue):
		return ReasonUnknownValue
	}
	return ReasonPlexUnavailable
}

// Sync brings Plex's collection in line with spec: the design's steps.
// It never creates or changes anything before every rule of the filter
// compiles, and never creates when Plex could not say what it holds.
func (s *Syncer) Sync(ctx context.Context, namespace string, spec Spec, prev Previous) Outcome {
	out := Outcome{RatingKey: prev.RatingKey, Adopted: prev.Adopted, Poster: prev.Poster, Background: prev.Background}
	if err := s.sync(ctx, namespace, spec, prev, &out); err != nil {
		out.Reason, out.Message = reasonOf(err), err.Error()
	}
	return out
}

func (s *Syncer) sync(ctx context.Context, namespace string, spec Spec, prev Previous, out *Outcome) error {
	// 1. The library.
	section, err := s.section(ctx, spec)
	if err != nil {
		return err
	}
	smart := spec.Filter != nil

	// 2. Compile a smart collection's filter -- every rule checked and
	// every tag resolved before Plex is touched -- or resolve a normal
	// one's members to Plex items.
	var meta api.SectionMeta
	var members memberPlan
	if smart {
		if meta, err = s.meta(ctx, section.Key); err != nil {
			return err
		}
		if _, err := s.compile(ctx, spec, meta, Carry{}); err != nil {
			return err
		}
	} else {
		if members, err = s.planMembers(ctx, namespace, section.Key, spec); err != nil {
			return err
		}
		out.MissingMembers = len(members.missing)
		out.Missing = members.missing[:min(len(members.missing), maxMissing)]
	}

	// 3. Find, adopt or create.
	col, adopted, err := s.find(ctx, section.Key, spec, prev.RatingKey)
	if err != nil {
		return err
	}
	var notes []string
	wrote := false
	switch {
	case col == nil:
		uri, err := s.contentURI(ctx, section.Key, spec, meta, members)
		if err != nil {
			return err
		}
		c, err := s.PMS.CreateCollection(ctx, section.Key, ItemTypeNumber(spec.ItemType), spec.Title, uri, smart)
		if err != nil {
			return err
		}
		col, out.Adopted, out.Reason = &c, false, ReasonCreated
		out.Message = fmt.Sprintf("created in Plex (ratingKey %s)", c.RatingKey)
	case adopted:
		out.Adopted, out.Reason = true, ReasonAdopted
		out.Message = fmt.Sprintf("adopted Plex's collection %q (ratingKey %s)", col.Title, col.RatingKey)
	default:
		out.Reason = ReasonSynced
	}
	out.RatingKey = col.RatingKey
	created := out.Reason == ReasonCreated

	// 4. The content: the resource wins. A smart collection's sort or
	// limit the spec leaves out is Plex's, carried.
	if !created && !smart {
		changed, err := s.syncMembers(ctx, col.RatingKey, members)
		if err != nil {
			return err
		}
		if changed {
			notes, wrote = append(notes, "members"), true
		}
	}
	if !created && smart {
		current, carry := ParseContent(col.Content)
		q, err := s.compile(ctx, spec, meta, carry)
		if err != nil {
			return err
		}
		if q != current {
			if err := s.PMS.SetCollectionFilter(ctx, col.RatingKey, SmartURI(s.MachineID, section.Key, q)); err != nil {
				return err
			}
			notes, wrote = append(notes, "filter"), true
		}
	}

	// 5. Title, summary and sort title, each only when stated.
	for _, f := range []struct {
		field string
		want  *string
		have  string
	}{{"title", &spec.Title, col.Title}, {"summary", spec.Summary, col.Summary}, {"titleSort", spec.SortTitle, col.TitleSort}} {
		if f.want == nil || *f.want == f.have {
			continue
		}
		if err := s.PMS.EditCollection(ctx, section.Key, col.RatingKey, f.field, *f.want); err != nil {
			return err
		}
		wrote = true
		if !created {
			notes = append(notes, f.field)
		}
	}

	// 7. What Plex holds now.
	if wrote {
		if c, err := s.PMS.Collection(ctx, col.RatingKey); err == nil {
			col = &c
		}
	}
	out.ItemCount = col.ChildCount
	if q, _ := ParseContent(col.Content); q != "" {
		out.Content = "library/sections/" + section.Key + "/all?" + Decode(q)
	}
	if len(notes) > 0 {
		drift := fmt.Sprintf("Plex's %s differed from the resource and %s put back", strings.Join(notes, ", "), map[bool]string{true: "was", false: "were"}[len(notes) == 1])
		if out.Message != "" {
			out.Message += "; " + drift
		} else {
			out.Message = drift
		}
	}
	if out.Message == "" {
		out.Message = "Plex matches the resource"
	}
	if out.MissingMembers > 0 {
		out.Reason = ReasonMembersPending
		out.Message = fmt.Sprintf("%d of %d members are not in Plex yet (not scanned or matched, or not in clustarr); %s", out.MissingMembers, members.total, out.Message)
	}
	return nil
}

// contentURI is what a new collection is created with: a smart one's
// compiled filter, or a normal one's items. A normal collection none of
// whose members Plex holds is not created.
func (s *Syncer) contentURI(ctx context.Context, sectionKey string, spec Spec, meta api.SectionMeta, members memberPlan) (string, error) {
	if spec.Filter != nil {
		q, err := s.compile(ctx, spec, meta, Carry{})
		if err != nil {
			return "", err
		}
		return SmartURI(s.MachineID, sectionKey, q), nil
	}
	if len(members.want) == 0 {
		return "", refuse(ReasonWaiting, "none of its %d members is in Plex yet; the collection is created once one is", members.total)
	}
	return ItemsURI(s.MachineID, members.want), nil
}

// section is the library the spec names, by title, holding its item type.
func (s *Syncer) section(ctx context.Context, spec Spec) (api.Section, error) {
	all, err := s.PMS.Sections(ctx)
	if err != nil {
		return api.Section{}, err
	}
	var found []api.Section
	for _, sec := range all {
		if sec.Title == spec.Library {
			found = append(found, sec)
		}
	}
	switch len(found) {
	case 0:
		return api.Section{}, refuse(ReasonLibraryNotFound, "Plex has no library titled %q", spec.Library)
	case 1:
	default:
		return api.Section{}, refuse(ReasonLibraryNotFound, "Plex has %d libraries titled %q", len(found), spec.Library)
	}
	want := "show"
	if spec.ItemType == "movie" {
		want = "movie"
	}
	if found[0].Type != want {
		return api.Section{}, refuse(ReasonInvalidRule, "library %q holds %ss, not %ss", spec.Library, found[0].Type, spec.ItemType)
	}
	return found[0], nil
}

// find is the collection the resource holds: its recorded ratingKey's,
// else one of its title and item type to adopt; nil when there is none.
// Only a 404 for the recorded ratingKey lets it look further, and a
// candidate it may not adopt stops it, so nothing is ever created beside
// a collection Plex already has.
func (s *Syncer) find(ctx context.Context, sectionKey string, spec Spec, ratingKey string) (*api.Collection, bool, error) {
	smart := spec.Filter != nil
	if ratingKey != "" {
		c, err := s.PMS.Collection(ctx, ratingKey)
		switch {
		case err == nil && c.Subtype == spec.ItemType && c.Smart == smart:
			return &c, false, nil
		case err == nil, errors.Is(err, api.ErrNotFound):
			// Deleted in Plex, or the key names something else now.
		default:
			return nil, false, err
		}
	}
	list, err := s.PMS.Collections(ctx, sectionKey)
	if err != nil {
		return nil, false, err
	}
	var adoptable []api.Collection
	var notByPerson, otherKind *api.Collection
	for i, c := range list {
		if c.Title != spec.Title || c.Subtype != spec.ItemType {
			continue
		}
		switch {
		case !c.MadeByPerson():
			notByPerson = &list[i]
		case c.Smart != smart:
			otherKind = &list[i]
		default:
			adoptable = append(adoptable, c)
		}
	}
	switch {
	case len(adoptable) == 1:
		return &adoptable[0], true, nil
	case len(adoptable) > 1:
		return nil, false, refuse(ReasonConflict, "Plex has %d collections titled %q; delete or rename all but one", len(adoptable), spec.Title)
	case notByPerson != nil:
		return nil, false, refuse(ReasonNotAdoptable, "Plex's collection %q was made by a metadata agent (%s), not a person, and is never adopted; retitle the resource", spec.Title, notByPerson.GUID)
	case otherKind != nil:
		kinds := map[bool]string{true: "smart", false: "normal"}
		return nil, false, refuse(ReasonNotSmart, "Plex's collection %q is a %s collection and the resource a %s one", spec.Title, kinds[otherKind.Smart], kinds[smart])
	}
	return nil, false, nil
}

// meta is a section's Meta, cached.
func (s *Syncer) meta(ctx context.Context, sectionKey string) (api.SectionMeta, error) {
	s.mu.Lock()
	m, ok := s.metas[sectionKey]
	s.mu.Unlock()
	if ok {
		return m, nil
	}
	m, err := s.PMS.SectionMeta(ctx, sectionKey)
	if err != nil {
		return api.SectionMeta{}, err
	}
	s.mu.Lock()
	if s.metas == nil {
		s.metas = map[string]api.SectionMeta{}
	}
	s.metas[sectionKey] = m
	s.mu.Unlock()
	return m, nil
}

func (s *Syncer) compile(ctx context.Context, spec Spec, meta api.SectionMeta, carry Carry) (string, error) {
	resolve := func(field, name string) (string, error) {
		path := meta.TagPaths[field]
		if path == "" {
			return "", fmt.Errorf("%w: %s lists no values", ErrInvalidRule, field)
		}
		id, cached, err := s.tagID(ctx, path, name, false)
		if err == nil && id == "" && cached {
			// A tag newer than the cached list: read it once more.
			id, _, err = s.tagID(ctx, path, name, true)
		}
		return id, err
	}
	return Compile(*spec.Filter, spec.Sort, spec.Limit, spec.ItemType, meta, resolve, carry)
}

// tagID is a tag's id by name, case-insensitively, from the path's list:
// the cached one unless fresh, reporting whether the answer was cached.
func (s *Syncer) tagID(ctx context.Context, path, name string, fresh bool) (string, bool, error) {
	key := strings.ToLower(name)
	s.mu.Lock()
	ids, ok := s.tags[path]
	s.mu.Unlock()
	if ok && !fresh {
		return ids[key], true, nil
	}
	vals, err := s.PMS.TagValues(ctx, path)
	if err != nil {
		return "", false, err
	}
	ids = make(map[string]string, len(vals))
	for _, v := range vals {
		if _, dup := ids[strings.ToLower(v.Title)]; !dup {
			ids[strings.ToLower(v.Title)] = v.ID
		}
	}
	s.mu.Lock()
	if s.tags == nil {
		s.tags = map[string]map[string]string{}
	}
	s.tags[path] = ids
	s.mu.Unlock()
	return ids[key], false, nil
}

// Delete deletes the collection a resource recorded; without a recorded
// ratingKey it deletes nothing.
func (s *Syncer) Delete(ctx context.Context, ratingKey string) error {
	if ratingKey == "" {
		return nil
	}
	return s.PMS.DeleteCollection(ctx, ratingKey)
}
