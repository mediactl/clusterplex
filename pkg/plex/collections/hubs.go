package collections

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/mediactl/clusterplex/pkg/plex/api"
)

// HubLayoutGVR is clustarr's PlexHubLayout, one per library: the order
// and visibility of the library's hubs, Plex Web's "Manage
// Recommendations" (docs/superpowers/specs/2026-11-16-plex-hub-layout-design.md).
// Read as unstructured, like PlexCollection.
var HubLayoutGVR = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "plexhublayouts"}

const (
	// ResetAnnotation asks for the library's hub customization to be
	// reset once: its value is a nonce (an RFC 3339 time), honoured when
	// it differs from status.resetHandled.
	ResetAnnotation = "catalog.clustarr.io/hubs-reset"

	// collectionHubPrefix starts a collection's hub identifier:
	// custom.collection.<sectionKey>.<ratingKey>.
	collectionHubPrefix = "custom.collection."

	// MaxStatusHubs caps status.hubs, and maxHubTitle a hub's title in it.
	MaxStatusHubs = 200
	maxHubTitle   = 256
)

// The Ready condition's reasons on a PlexHubLayout beside ReasonSynced,
// ReasonLibraryNotFound, ReasonConflict and ReasonPlexUnavailable. Only
// Synced reads True.
const (
	// ReasonCollectionNotReady: a collection entry's PlexCollection does
	// not exist or holds no Plex collection (status.ratingKey) yet.
	ReasonCollectionNotReady = "CollectionNotReady"
	// ReasonCollectionNotFound and ReasonCollectionAmbiguous: a
	// collectionTitle entry names no collection of the library, or more
	// than one.
	ReasonCollectionNotFound  = "CollectionNotFound"
	ReasonCollectionAmbiguous = "CollectionAmbiguous"
	// ReasonUnknownIdentifier: an identifier entry names no hub Plex
	// lists for the library.
	ReasonUnknownIdentifier = "UnknownIdentifier"
	// ReasonInvalidEntry: an entry states none or several of identifier,
	// collection and collectionTitle, repeats a hub, or names a
	// PlexCollection of another library.
	ReasonInvalidEntry = "InvalidEntry"
)

// HubLayoutSpec mirrors PlexHubLayout's spec.
type HubLayoutSpec struct {
	Library string     `json:"library"`
	Hubs    []HubEntry `json:"hubs,omitempty"`
}

// HubEntry is one hub of the wanted order: exactly one of a built-in
// hub's Identifier, a PlexCollection's name (Collection) or a Plex
// collection's title (CollectionTitle), and where the hub is promoted.
type HubEntry struct {
	Identifier         string `json:"identifier,omitempty"`
	Collection         string `json:"collection,omitempty"`
	CollectionTitle    string `json:"collectionTitle,omitempty"`
	LibraryRecommended bool   `json:"libraryRecommended"`
	Home               bool   `json:"home"`
	FriendsHome        bool   `json:"friendsHome"`
}

// HubStatus is one hub as Plex has it, an element of status.hubs.
type HubStatus struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title,omitempty"`
	// Kind is "builtin" or "collection".
	Kind string `json:"kind"`
	// Collection is the PlexCollection holding a collection hub's
	// collection, when one does.
	Collection         string `json:"collection,omitempty"`
	RatingKey          string `json:"ratingKey,omitempty"`
	LibraryRecommended bool   `json:"libraryRecommended"`
	Home               bool   `json:"home"`
	FriendsHome        bool   `json:"friendsHome"`
}

// HubLayoutSpecOf reads a PlexHubLayout's spec.
func HubLayoutSpecOf(u *unstructured.Unstructured) (HubLayoutSpec, error) {
	raw, ok := u.Object["spec"].(map[string]any)
	if !ok {
		return HubLayoutSpec{}, fmt.Errorf("PlexHubLayout %s/%s: no spec", u.GetNamespace(), u.GetName())
	}
	var s HubLayoutSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &s); err != nil {
		return HubLayoutSpec{}, fmt.Errorf("PlexHubLayout %s/%s: spec: %w", u.GetNamespace(), u.GetName(), err)
	}
	return s, nil
}

// SectionFinder narrows Plex's sections to those a library title names.
// provision.Config.SectionsTitled is one that also takes a library's
// former names; nil is the sections of exactly that title.
type SectionFinder func(all []api.Section, title string) []api.Section

func (f SectionFinder) find(all []api.Section, title string) []api.Section {
	if f != nil {
		return f(all, title)
	}
	var out []api.Section
	for _, s := range all {
		if s.Title == title {
			out = append(out, s)
		}
	}
	return out
}

// HubPMS is the Plex Media Server API a HubSyncer uses; *api.Client is one.
type HubPMS interface {
	Sections(ctx context.Context) ([]api.Section, error)
	Collections(ctx context.Context, sectionKey string) ([]api.Collection, error)
	Collection(ctx context.Context, ratingKey string) (api.Collection, error)
	ManagedHubs(ctx context.Context, sectionKey string) ([]api.ManagedHub, error)
	CreateManagedHub(ctx context.Context, sectionKey, ratingKey string, v api.ManagedHub) error
	UpdateManagedHub(ctx context.Context, sectionKey string, v api.ManagedHub) error
	MoveManagedHub(ctx context.Context, sectionKey, identifier, after string) error
	ResetManagedHubs(ctx context.Context, sectionKey string) error
}

var _ HubPMS = (*api.Client)(nil)

// HeldCollection is a PlexCollection of the layout's namespace: its
// name, the library it names and the Plex collection it holds, if any.
type HeldCollection struct {
	Name, Library, RatingKey string
}

// HubInput is one layout's sync: its spec, whether its reset is asked
// for and not yet honoured, and the namespace's PlexCollections, oldest
// first.
type HubInput struct {
	Spec        HubLayoutSpec
	Reset       bool
	Collections []HeldCollection
}

// HubOutcome is a layout sync's result, which the resource's status
// records.
type HubOutcome struct {
	SectionKey string
	// ResetDone says Plex took the reset this sync asked for.
	ResetDone bool
	// Known says Hubs is Plex's list, read this sync; false leaves the
	// status's last list as it is.
	Known bool
	Hubs  []HubStatus
	// Changed names what this sync wrote to Plex: "reset", "visibility",
	// "hubs" (made) and "order".
	Changed         []string
	Reason, Message string
}

// HubSyncer brings a library's hubs in line with a PlexHubLayout. Plex's
// section list is kept until Forget.
type HubSyncer struct {
	PMS HubPMS
	// Sections finds the library a title names; nil is by exact title.
	Sections SectionFinder

	mu       sync.Mutex
	sections []api.Section
}

// Forget drops the kept section list: on Plex's start, and when a
// section's hubs are not found.
func (s *HubSyncer) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sections = nil
}

func (s *HubSyncer) allSections(ctx context.Context) ([]api.Section, error) {
	s.mu.Lock()
	all := s.sections
	s.mu.Unlock()
	if all != nil {
		return all, nil
	}
	all, err := s.PMS.Sections(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sections = all
	s.mu.Unlock()
	return all, nil
}

// Sync makes the listed hubs the library's first, in the layout's order
// and promoted where each entry says, and reports Plex's list. An entry
// it cannot resolve is skipped and named; the rest are applied.
func (s *HubSyncer) Sync(ctx context.Context, in HubInput) HubOutcome {
	var out HubOutcome
	if err := s.sync(ctx, in, &out); err != nil {
		out.Reason, out.Message = reasonOf(err), err.Error()
	}
	return out
}

// hubTarget is a resolved entry: the hub it names, and the collection
// behind it when the hub may have to be made.
type hubTarget struct {
	id, ratingKey string
	want          api.ManagedHub
}

func (s *HubSyncer) sync(ctx context.Context, in HubInput, out *HubOutcome) error {
	// 1. The library.
	all, err := s.allSections(ctx)
	if err != nil {
		return err
	}
	found := s.Sections.find(all, in.Spec.Library)
	switch len(found) {
	case 0:
		// A library made or renamed since the list was read is found next time.
		s.Forget()
		return refuse(ReasonLibraryNotFound, "Plex has no library titled %q", in.Spec.Library)
	case 1:
	default:
		return refuse(ReasonLibraryNotFound, "Plex has %d libraries titled %q", len(found), in.Spec.Library)
	}
	section := found[0].Key
	out.SectionKey = section

	// 2. The reset, once, before anything the layout asks.
	if in.Reset {
		if err := s.PMS.ResetManagedHubs(ctx, section); err != nil {
			return fmt.Errorf("resetting the library's hubs: %w", err)
		}
		out.ResetDone = true
		out.Changed = append(out.Changed, "reset")
	}

	// 3. Plex's hubs: the one read of an idle layout.
	hubs, err := s.hubs(ctx, section)
	if err != nil {
		return err
	}

	// 4. Each entry's hub. One that cannot be resolved is skipped.
	var cols []api.Collection
	if slices.ContainsFunc(in.Spec.Hubs, func(e HubEntry) bool { return e.CollectionTitle != "" }) {
		if cols, err = s.PMS.Collections(ctx, section); err != nil {
			return err
		}
	}
	var problems []refusal
	var targets []hubTarget
	seen := map[string]int{}
	for i, e := range in.Spec.Hubs {
		t, err := s.resolve(all, section, hubs, cols, in.Collections, e)
		var r refusal
		switch {
		case errors.As(err, &r):
			problems = append(problems, refusal{r.reason, fmt.Sprintf("hubs[%d]: %s", i, r.message)})
			continue
		case err != nil:
			return err
		}
		if j, dup := seen[t.id]; dup {
			problems = append(problems, refusal{ReasonInvalidEntry, fmt.Sprintf("hubs[%d]: names the same hub as hubs[%d] (%s)", i, j, t.id)})
			continue
		}
		seen[t.id] = i
		t.want = api.ManagedHub{Identifier: t.id, Recommended: e.LibraryRecommended, OwnHome: e.Home, SharedHome: e.FriendsHome}
		targets = append(targets, t)
	}

	// 5. Visibility where it differs; a collection's hub made when Plex
	// has none.
	made, wrote := false, false
	for _, t := range targets {
		i := slices.IndexFunc(hubs, func(h api.ManagedHub) bool { return h.Identifier == t.id })
		switch {
		case i < 0:
			if err := s.PMS.CreateManagedHub(ctx, section, t.ratingKey, t.want); err != nil {
				return fmt.Errorf("making the hub %s: %w", t.id, err)
			}
			made = true
		case hubs[i].Promoted() != t.want.Promoted():
			if err := s.PMS.UpdateManagedHub(ctx, section, t.want); err != nil {
				return fmt.Errorf("setting where the hub %s is shown: %w", t.id, err)
			}
			wrote = true
		}
	}
	if wrote {
		out.Changed = append(out.Changed, "visibility")
	}
	if made {
		out.Changed = append(out.Changed, "hubs")
		// Where Plex put the new hubs is Plex's to say.
		if hubs, err = s.hubs(ctx, section); err != nil {
			return err
		}
		wrote = false
	}

	// 6. The order: the listed first, as listed; the rest as Plex has
	// them. Only a hub out of place is moved.
	current := make([]string, 0, len(hubs))
	for _, h := range hubs {
		current = append(current, h.Identifier)
	}
	var target []string
	for _, t := range targets {
		if slices.Contains(current, t.id) {
			target = append(target, t.id)
		} else {
			problems = append(problems, refusal{ReasonPlexUnavailable, fmt.Sprintf("Plex lists no hub %s after one was made for it", t.id)})
		}
	}
	listed := len(target)
	for _, id := range current {
		if !slices.Contains(target[:listed], id) {
			target = append(target, id)
		}
	}
	moves := PlanMoves(current, target)
	for _, m := range moves {
		if err := s.PMS.MoveManagedHub(ctx, section, m[0], m[1]); err != nil {
			return fmt.Errorf("moving the hub %s: %w", m[0], err)
		}
	}
	if len(moves) > 0 {
		out.Changed = append(out.Changed, "order")
		wrote = true
	}

	// 7. What Plex holds now.
	if wrote {
		if hubs, err = s.hubs(ctx, section); err != nil {
			return err
		}
	}
	out.Hubs, out.Known = s.report(ctx, section, hubs, cols, in.Collections), true

	var notes []string
	if len(hubs) > MaxStatusHubs {
		notes = append(notes, fmt.Sprintf("Plex lists %d hubs; the status shows the first %d", len(hubs), MaxStatusHubs))
	}
	if len(problems) > 0 {
		msgs := make([]string, 0, len(problems)+len(notes))
		for _, p := range problems {
			msgs = append(msgs, p.message)
		}
		return refusal{problems[0].reason, strings.Join(append(msgs, notes...), "; ")}
	}
	out.Reason = ReasonSynced
	switch {
	case len(out.Changed) > 0:
		out.Message = "applied to Plex: " + strings.Join(out.Changed, ", ")
	case len(targets) == 0:
		out.Message = "the layout lists no hub; Plex's are as Plex has them"
	default:
		out.Message = "Plex's hubs match the layout"
	}
	if len(notes) > 0 {
		out.Message += "; " + strings.Join(notes, "; ")
	}
	return nil
}

// hubs reads the section's hubs. Only a claimed server lists them.
func (s *HubSyncer) hubs(ctx context.Context, section string) ([]api.ManagedHub, error) {
	hubs, err := s.PMS.ManagedHubs(ctx, section)
	if errors.Is(err, api.ErrNotFound) {
		// The section may be gone, too: its key is looked up again.
		s.Forget()
		return nil, fmt.Errorf("Plex lists no hubs to manage for section %s (a server must be claimed to manage them): %w", section, err)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the library's hubs: %w", err)
	}
	return hubs, nil
}

// resolve is the hub an entry names, or the refusal that skips it.
func (s *HubSyncer) resolve(all []api.Section, section string, hubs []api.ManagedHub, cols []api.Collection, held []HeldCollection, e HubEntry) (hubTarget, error) {
	stated := 0
	for _, v := range []string{e.Identifier, e.Collection, e.CollectionTitle} {
		if v != "" {
			stated++
		}
	}
	switch {
	case stated != 1:
		return hubTarget{}, refuse(ReasonInvalidEntry, "exactly one of identifier, collection and collectionTitle is stated, not %d", stated)

	case e.Identifier != "":
		if !slices.ContainsFunc(hubs, func(h api.ManagedHub) bool { return h.Identifier == e.Identifier }) {
			return hubTarget{}, refuse(ReasonUnknownIdentifier, "Plex lists no hub %q for the library", e.Identifier)
		}
		return hubTarget{id: e.Identifier}, nil

	case e.Collection != "":
		i := slices.IndexFunc(held, func(c HeldCollection) bool { return c.Name == e.Collection })
		switch {
		case i < 0:
			return hubTarget{}, refuse(ReasonCollectionNotReady, "there is no PlexCollection %q in the namespace", e.Collection)
		case held[i].RatingKey == "":
			return hubTarget{}, refuse(ReasonCollectionNotReady, "the PlexCollection %q holds no Plex collection yet (no status.ratingKey)", e.Collection)
		}
		if in := s.Sections.find(all, held[i].Library); len(in) != 1 || in[0].Key != section {
			return hubTarget{}, refuse(ReasonInvalidEntry, "the PlexCollection %q is of the library %q, not this one", e.Collection, held[i].Library)
		}
		return hubTarget{id: collectionHubPrefix + section + "." + held[i].RatingKey, ratingKey: held[i].RatingKey}, nil
	}

	// A Plex collection by its title, in any case, as Plex's are matched.
	var match []api.Collection
	for _, c := range cols {
		if strings.EqualFold(c.Title, e.CollectionTitle) {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		return hubTarget{}, refuse(ReasonCollectionNotFound, "the library has no collection titled %q", e.CollectionTitle)
	case 1:
		return hubTarget{id: collectionHubPrefix + section + "." + match[0].RatingKey, ratingKey: match[0].RatingKey}, nil
	}
	return hubTarget{}, refuse(ReasonCollectionAmbiguous, "the library has %d collections titled %q", len(match), e.CollectionTitle)
}

// report is Plex's list as status.hubs holds it, capped. A collection's
// hub Plex gives no title takes its collection's.
func (s *HubSyncer) report(ctx context.Context, section string, hubs []api.ManagedHub, cols []api.Collection, held []HeldCollection) []HubStatus {
	holder := map[string]string{}
	for _, c := range held {
		if c.RatingKey != "" && holder[c.RatingKey] == "" {
			holder[c.RatingKey] = c.Name
		}
	}
	out := make([]HubStatus, 0, min(len(hubs), MaxStatusHubs))
	for _, h := range hubs[:min(len(hubs), MaxStatusHubs)] {
		st := HubStatus{
			Identifier: h.Identifier, Title: h.Title, Kind: "builtin",
			LibraryRecommended: h.Recommended, Home: h.OwnHome, FriendsHome: h.SharedHome,
		}
		if rk, ok := strings.CutPrefix(h.Identifier, collectionHubPrefix+section+"."); ok {
			st.Kind, st.RatingKey, st.Collection = "collection", rk, holder[rk]
			if st.Title == "" {
				if i := slices.IndexFunc(cols, func(c api.Collection) bool { return c.RatingKey == rk }); i >= 0 {
					st.Title = cols[i].Title
				} else if c, err := s.PMS.Collection(ctx, rk); err == nil {
					st.Title = c.Title
				}
			}
		} else if strings.HasPrefix(h.Identifier, collectionHubPrefix) {
			st.Kind = "collection"
		}
		if len(st.Title) > maxHubTitle {
			st.Title = st.Title[:maxHubTitle]
		}
		out = append(out, st)
	}
	return out
}

// PlanMoves is the fewest moves that turn current into target, a
// reordering of it: each {hub, after}, the hub put right after "after",
// or first when after is "". The hubs already in target's relative order
// -- a longest such run -- stay; every other one is moved once, in
// target's order, behind the hub it follows there.
func PlanMoves(current, target []string) [][2]string {
	want := make(map[string]int, len(target))
	for i, id := range target {
		want[id] = i
	}
	// The longest increasing run of target positions in current.
	n := len(current)
	length, prev := make([]int, n), make([]int, n)
	best := -1
	for i := range current {
		length[i], prev[i] = 1, -1
		for j := range i {
			if want[current[j]] < want[current[i]] && length[j]+1 > length[i] {
				length[i], prev[i] = length[j]+1, j
			}
		}
		if best < 0 || length[i] > length[best] {
			best = i
		}
	}
	keep := map[string]bool{}
	for i := best; i >= 0; i = prev[i] {
		keep[current[i]] = true
	}
	var moves [][2]string
	for i, id := range target {
		if keep[id] {
			continue
		}
		after := ""
		if i > 0 {
			after = target[i-1]
		}
		moves = append(moves, [2]string{id, after})
	}
	return moves
}
