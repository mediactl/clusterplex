package collections

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxMissing caps the members status lists as missing (the CRD's 20).
const maxMissing = 20

// editionMarker begins the edition part of a movie's GUID.
const editionMarker = "/edition/"

// baseGUID is a GUID without its edition, as pkg/plexproject.BaseGUID: a
// member is every edition of its movie.
func baseGUID(guid string) string {
	if i := strings.Index(guid, editionMarker); i >= 0 {
		return guid[:i]
	}
	return guid
}

// memberPlan is a normal collection's members as Plex holds them: the
// rating keys it should hold, in member order, and the members Plex does
// not hold (or clustarr does not have).
type memberPlan struct {
	want    []string
	missing []Member
	total   int
}

// planMembers resolves the members to Plex items by GUID. A resolver
// failure is a refusal: no item is added or removed on a guess.
func (s *Syncer) planMembers(ctx context.Context, namespace, sectionKey string, spec Spec) (memberPlan, error) {
	var members []Member
	if spec.Members != nil {
		members = spec.Members.Items
	}
	p := memberPlan{total: len(members)}
	if s.Members == nil {
		return p, refuse(ReasonPlexUnavailable, "no clustarr reader to resolve the members with")
	}
	guids, err := s.Members.GUIDs(ctx, namespace, members)
	if err != nil {
		return p, refuse(ReasonPlexUnavailable, "reading the members from clustarr: %v", err)
	}
	itemType := ItemTypeNumber(spec.ItemType)
	index, cached, err := s.guidIndex(ctx, sectionKey, itemType, false)
	if err != nil {
		return p, err
	}
	if cached {
		// An item scanned in since the index was read: read it once more.
		for _, m := range members {
			if g, ok := guids[m]; ok && len(index[baseGUID(g)]) == 0 {
				if index, _, err = s.guidIndex(ctx, sectionKey, itemType, true); err != nil {
					return p, err
				}
				break
			}
		}
	}
	seen := map[string]bool{}
	for _, m := range members {
		rks := index[baseGUID(guids[m])]
		if guids[m] == "" || len(rks) == 0 {
			p.missing = append(p.missing, m)
			continue
		}
		for _, rk := range rks {
			if !seen[rk] {
				seen[rk] = true
				p.want = append(p.want, rk)
			}
		}
	}
	return p, nil
}

// guidIndexTTL is how long a section's GUID index is trusted: a movie
// deleted and scanned in again keeps its GUID under a new ratingKey,
// which only a fresh read finds. The controller's passes are 10 minutes
// apart, so each pass reads it once.
const guidIndexTTL = 5 * time.Minute

// guidIndex is a cached index and when it was read.
type guidIndex struct {
	byGUID map[string][]string
	read   time.Time
}

// guidIndex maps a section's items of one type by base GUID to their
// rating keys, cached unless fresh or older than guidIndexTTL; it reports
// whether it was cached.
func (s *Syncer) guidIndex(ctx context.Context, sectionKey string, itemType int, fresh bool) (map[string][]string, bool, error) {
	key := sectionKey + "/" + strconv.Itoa(itemType)
	now := s.now()
	s.mu.Lock()
	cached, ok := s.guids[key]
	s.mu.Unlock()
	if ok && !fresh && now.Sub(cached.read) < guidIndexTTL {
		return cached.byGUID, true, nil
	}
	items, err := s.PMS.SectionItems(ctx, sectionKey, itemType)
	if err != nil {
		return nil, false, err
	}
	index := make(map[string][]string, len(items))
	for _, it := range items {
		if it.GUID != "" {
			b := baseGUID(it.GUID)
			index[b] = append(index[b], it.RatingKey)
		}
	}
	s.mu.Lock()
	if s.guids == nil {
		s.guids = map[string]guidIndex{}
	}
	s.guids[key] = guidIndex{byGUID: index, read: now}
	s.mu.Unlock()
	return index, false, nil
}

// syncMembers adds the items Plex lacks and removes the ones no member
// names, but never empties the collection: with no member in Plex,
// nothing is removed. It reports whether it changed anything.
func (s *Syncer) syncMembers(ctx context.Context, ratingKey string, p memberPlan) (bool, error) {
	children, err := s.PMS.CollectionChildren(ctx, ratingKey)
	if err != nil {
		return false, err
	}
	have := make(map[string]bool, len(children))
	for _, c := range children {
		have[c.RatingKey] = true
	}
	var add []string
	for _, rk := range p.want {
		if !have[rk] {
			add = append(add, rk)
		}
	}
	changed := false
	if len(add) > 0 {
		if err := s.PMS.AddCollectionItems(ctx, ratingKey, ItemsURI(s.MachineID, add)); err != nil {
			return false, err
		}
		changed = true
	}
	if len(p.want) == 0 {
		return changed, nil
	}
	for _, c := range children {
		if slices.Contains(p.want, c.RatingKey) {
			continue
		}
		if err := s.PMS.RemoveCollectionItem(ctx, ratingKey, c.RatingKey); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
