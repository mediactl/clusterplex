package collections

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/plex/api"
)

// refetchAfter is how long an uploaded image's bytes are trusted before
// its URL is fetched again.
const refetchAfter = 24 * time.Hour

// Plex's collection mode and order numbers, by the spec's words.
var (
	modeNumbers  = map[string]int{"default": -1, "hideCollection": 0, "hideItems": 1, "showCollectionAndItems": 2}
	orderNumbers = map[string]int{"releaseDate": 0, "alphabetical": 1}
)

// presentation is what step 6 did: the names of what it changed, and the
// images it could not set, which do not stop the rest of the sync.
type presentation struct {
	changed  []string
	problems []string
}

// present sets what the spec states of the images, settings and
// visibility, and nothing it leaves out.
func (s *Syncer) present(ctx context.Context, sectionKey string, col *api.Collection, spec Spec, prev Previous, out *Outcome) (presentation, error) {
	var p presentation
	// Settings and visibility first: an image Plex cannot take must hold
	// neither back.
	changed, err := s.syncSettings(ctx, col, spec)
	if err != nil {
		return p, err
	}
	p.changed = append(p.changed, changed...)
	if spec.Visibility != nil {
		set, err := s.syncVisibility(ctx, sectionKey, col.RatingKey, *spec.Visibility)
		if err != nil {
			return p, err
		}
		if set {
			p.changed = append(p.changed, "visibility")
		}
	}
	for _, img := range []struct {
		kind, name string
		want       *Image
		prev       *ImageState
		dst        **ImageState
	}{
		{"posters", "poster", spec.Poster, prev.Poster, &out.Poster},
		{"arts", "background", spec.Background, prev.Background, &out.Background},
	} {
		if img.want == nil {
			*img.dst = nil
			continue
		}
		st, uploaded, problem, err := s.syncImage(ctx, col.RatingKey, img.kind, img.want.URL, img.prev)
		if err != nil {
			return p, err
		}
		if problem != "" {
			p.problems = append(p.problems, img.name+": "+problem)
			*img.dst = img.prev
			continue
		}
		*img.dst = st
		if uploaded {
			p.changed = append(p.changed, img.name)
		}
	}
	return p, nil
}

// syncImage keeps the image of url selected on the collection: its bytes
// are fetched when the URL is new or a day old, and uploaded when Plex's
// selected image is not the upload of those bytes. A fetch that fails, is
// too large or is not an image is a problem, not an error: the image is
// left as it is.
func (s *Syncer) syncImage(ctx context.Context, ratingKey, kind, url string, prev *ImageState) (*ImageState, bool, string, error) {
	now := s.now()
	var body []byte
	var ctype string
	st := ImageState{URL: url}
	if prev != nil && prev.URL == url && prev.SHA1 != "" && now.Sub(prev.FetchedAt) < refetchAfter {
		st = *prev
	} else {
		var problem string
		if body, ctype, problem = s.fetchImage(ctx, url); problem != "" {
			return nil, false, problem, nil
		}
		st.SHA1, st.FetchedAt = sha1Hex(body), now
	}
	shown, err := s.PMS.Posters(ctx, ratingKey, kind)
	if err != nil {
		return nil, false, "", err
	}
	for _, p := range shown {
		if p.Selected && p.SHA1 == st.SHA1 {
			return &st, false, "", nil
		}
	}
	if body == nil {
		var problem string
		if body, ctype, problem = s.fetchImage(ctx, url); problem != "" {
			return nil, false, problem, nil
		}
		st.SHA1, st.FetchedAt = sha1Hex(body), now
	}
	// Plex has just listed the item's images, so a failed upload is Plex
	// refusing these bytes: the image's problem, not Plex being away.
	if err := s.PMS.UploadImage(ctx, ratingKey, kind, body, ctype); err != nil {
		return nil, false, fmt.Sprintf("Plex refused the upload: %v", err), nil
	}
	return &st, true, "", nil
}

// fetchImage reads an image through the Syncer's fetcher, holding any
// fetcher to the size and type rules; the string says what failed.
func (s *Syncer) fetchImage(ctx context.Context, url string) ([]byte, string, string) {
	if s.Fetch == nil {
		return nil, "", "no image fetcher is configured"
	}
	body, ctype, err := s.Fetch(ctx, url)
	switch {
	case err != nil:
		return nil, "", err.Error()
	case len(body) > api.MaxImageBytes:
		return nil, "", fmt.Sprintf("%s is over %d bytes", url, api.MaxImageBytes)
	case !strings.HasPrefix(ctype, imageTypeStart):
		return nil, "", fmt.Sprintf("%s is %q, not an image", url, ctype)
	}
	// The bytes decide, not the header: an SVG or a page sent as an image
	// is no poster Plex can show.
	sniffed := http.DetectContentType(body)
	if !uploadTypes[sniffed] {
		return nil, "", fmt.Sprintf("%s is %s, not a JPEG, PNG, GIF, WebP or BMP image", url, sniffed)
	}
	return body, sniffed, ""
}

// uploadTypes are the image types, as http.DetectContentType names them,
// that a poster or background is uploaded as.
var uploadTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true}

// syncSettings sets the stated mode, and a normal collection's stated
// order, where Plex's differ.
func (s *Syncer) syncSettings(ctx context.Context, col *api.Collection, spec Spec) ([]string, error) {
	if spec.Settings == nil {
		return nil, nil
	}
	var mode, order *int
	if w := spec.Settings.Mode; w != "" {
		n, ok := modeNumbers[w]
		if !ok {
			return nil, refuse(ReasonInvalidRule, "settings.mode: %q is not a collection mode", w)
		}
		mode = &n
	}
	if w := spec.Settings.Order; w != "" && spec.Filter == nil {
		n, ok := orderNumbers[w]
		if !ok {
			return nil, refuse(ReasonInvalidRule, "settings.order: %q is not a collection order", w)
		}
		order = &n
	}
	if mode == nil && order == nil {
		return nil, nil
	}
	have, err := s.PMS.CollectionPrefs(ctx, col.RatingKey)
	if err != nil {
		return nil, err
	}
	var changed []string
	if mode != nil && *mode == have.Mode {
		mode = nil
	} else if mode != nil {
		changed = append(changed, "mode")
	}
	if order != nil && *order == have.Order {
		order = nil
	} else if order != nil {
		changed = append(changed, "order")
	}
	if len(changed) == 0 {
		return nil, nil
	}
	return changed, s.PMS.SetCollectionPrefs(ctx, col.RatingKey, mode, order)
}

// syncVisibility makes the collection's managed hub promoted where v
// says. A missing hub reads as promoted nowhere, so all three false with
// no hub writes nothing.
func (s *Syncer) syncVisibility(ctx context.Context, sectionKey, ratingKey string, v Visibility) (bool, error) {
	hubs, err := s.PMS.ManagedHubs(ctx, sectionKey)
	if err != nil {
		return false, fmt.Errorf("reading the library's hubs (a server must be claimed to manage them): %w", err)
	}
	want := api.ManagedHub{
		Identifier:  "custom.collection." + sectionKey + "." + ratingKey,
		Recommended: v.LibraryRecommended, OwnHome: v.Home, SharedHome: v.FriendsHome,
	}
	for _, h := range hubs {
		if h.Identifier != want.Identifier {
			continue
		}
		if h == want {
			return false, nil
		}
		return true, s.PMS.UpdateManagedHub(ctx, sectionKey, want)
	}
	if !want.Recommended && !want.OwnHome && !want.SharedHome {
		return false, nil
	}
	return true, s.PMS.CreateManagedHub(ctx, sectionKey, ratingKey, want)
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func sha1Hex(b []byte) string { return fmt.Sprintf("%x", sha1.Sum(b)) }
