package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxLiveTVBody caps a Live TV answer: a DVR or an EPG channel map lists a
// row per channel, and an IPTVProvider has no channel cap.
const maxLiveTVBody = 32 << 20

// Str is a Plex attribute that one PMS answers as a string and another as a
// number; it decodes either.
type Str string

// UnmarshalJSON accepts a JSON string or number.
func (s *Str) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = Str(v)
		return nil
	}
	*s = Str(strings.TrimSpace(string(b)))
	return nil
}

// Device is a tuner device PMS knows (GET /media/grabbers/devices), as
// recorded from PMS 1.43.4 (docs/livetv-pms-calls.md). UUID is its
// identifier, "device://tv.plex.grabbers.hdhomerun/<DeviceID>"; Model is the
// device's FriendlyName; ParentID is its DVR's key once it has one (a number
// on the wire).
type Device struct {
	Key      Str    `json:"key"`
	UUID     string `json:"uuid"`
	URI      string `json:"uri"`
	DeviceID string `json:"deviceId"`
	Model    string `json:"model"`
	State    string `json:"state"`
	ParentID int    `json:"parentID"`
}

// DVR is a PMS DVR (GET /livetv/dvrs): a lineup over one or more devices.
// It has no title; LineupTitle is the lineup's.
type DVR struct {
	Key           Str      `json:"key"`
	UUID          string   `json:"uuid"`
	Language      string   `json:"language"`
	Lineup        string   `json:"lineup"`
	LineupTitle   string   `json:"lineupTitle"`
	EPGIdentifier string   `json:"epgIdentifier"`
	Devices       []Device `json:"Device"`
	// Lineups is the same lineup as a list, which PMS sends as "Lineup"
	// beside "lineup". It must have a field of its own: encoding/json
	// matches keys case-insensitively, and without one the list was decoded
	// into Lineup and failed every DVR listing (recorded 06-dvrs.json).
	Lineups []DVRLineup `json:"Lineup"`
}

// DVRLineup is one entry of a DVR's "Lineup" list.
type DVRLineup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ChannelMapping pairs a device channel with a lineup channel, as PMS's EPG
// matched them (GET /livetv/epg/channelmap) and as a channel-map save sends
// them back. Enabled is "1" for a channel a saved map turned on, as a device
// lists them.
type ChannelMapping struct {
	ChannelKey       string `json:"channelKey"`
	DeviceIdentifier string `json:"deviceIdentifier"`
	LineupIdentifier string `json:"lineupIdentifier"`
	Favorite         string `json:"favorite,omitempty"`
	Enabled          string `json:"enabled,omitempty"`
}

// Activity is one of PMS's running activities (GET /activities). A guide
// load is type ActivityGuideLoad.
type Activity struct {
	UUID     string `json:"uuid"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	Progress int    `json:"progress"`
	// Context is what the activity is about; a grab's names its live
	// session (TranscodeSession, the id in /livetv/sessions/<id>).
	Context ActivityContext `json:"Context"`
}

// ActivityContext is the part of an activity's Context a caller reads.
type ActivityContext struct {
	TranscodeSession Str `json:"transcode_session"`
}

// ActivityGrab is a grabber's recording: a DVR recording, or a Live TV
// session's (subtitle "Live TV - Session …").
const ActivityGrab = "grabber.grab"

// CancelActivity cancels a running activity, as the dashboard's cancel
// does. A grab's cancel stops its recording and ends its live session
// (PMS 1.43.4, talos 2026-10-20): the one way found to end a grab its
// client abandoned, since DELETE /livetv/sessions/<id> is 404 and only the
// client's own stopped timeline ends it otherwise.
func (c *Client) CancelActivity(ctx context.Context, uuid string) error {
	return c.do(ctx, http.MethodDelete, "/activities/"+url.PathEscape(uuid), nil, nil)
}

// LiveSession is one of PMS's Live TV sessions (/livetv/sessions).
type LiveSession struct {
	// Key is "/livetv/sessions/<id>"; RatingKey the item a player that
	// watches it plays.
	Key       string `json:"key"`
	RatingKey Str    `json:"ratingKey"`
	Title     string `json:"title"`
}

// LiveSessions lists PMS's Live TV sessions.
func (c *Client) LiveSessions(ctx context.Context) ([]LiveSession, error) {
	var out struct {
		MediaContainer struct {
			Metadata []LiveSession `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodGet, "/livetv/sessions", nil, &out)
	return out.MediaContainer.Metadata, err
}

// PlayingItem is one item a player is playing (/status/sessions), by the
// two ways it names it.
type PlayingItem struct {
	Key       string `json:"key"`
	RatingKey Str    `json:"ratingKey"`
}

// Playing lists what PMS's players are playing, paused ones included.
func (c *Client) Playing(ctx context.Context) ([]PlayingItem, error) {
	var out struct {
		MediaContainer struct {
			Metadata []PlayingItem `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodGet, "/status/sessions", nil, &out)
	return out.MediaContainer.Metadata, err
}

// ActivityGuideLoad is the activity of a DVR's guide being rebuilt. A DVR
// deleted, or its map saved again, while one runs deadlocked PMS (recorded):
// callers wait for it to end.
const ActivityGuideLoad = "provider.epg.load"

// MaxChannelMapBytes keeps a channel-map query under PMS's 32 KiB request
// line, with room for the path (recorded: 454 four-digit channels are a
// 32,738-byte path and query, 455 a 400).
const MaxChannelMapBytes = 30000

// ErrChannelMapTooLarge is a channel map PMS would refuse; nothing is sent.
var ErrChannelMapTooLarge = errors.New("plexapi: channel map over 30,000 bytes")

// ErrEmptyChannelMap is a save with no channel: one cleared a device's whole
// map in the recording, so it is never sent.
var ErrEmptyChannelMap = errors.New("plexapi: empty channel map")

// XMLTVLineup is the lineup id of an XMLTV guide at guideURL, titled title.
// url.Values encodes it once; it must not be encoded beforehand.
func XMLTVLineup(guideURL, title string) string {
	return "lineup://tv.plex.providers.epg.xmltv/" + guideURL + "#" + title
}

// Devices lists every tuner device PMS knows.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out struct {
		MediaContainer struct {
			Device []Device `json:"Device"`
		} `json:"MediaContainer"`
	}
	err := c.doLimit(ctx, http.MethodGet, "/media/grabbers/devices", nil, &out, maxLiveTVBody)
	return out.MediaContainer.Device, err
}

// AddDevice registers the device at uri.
func (c *Client) AddDevice(ctx context.Context, uri string) error {
	return c.do(ctx, http.MethodPost, "/media/grabbers/devices", url.Values{"uri": {uri}}, nil)
}

// DeleteDevice removes a device.
func (c *Client) DeleteDevice(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/media/grabbers/devices/"+url.PathEscape(key), nil, nil)
}

// DVRs lists every DVR.
func (c *Client) DVRs(ctx context.Context) ([]DVR, error) {
	var out struct {
		MediaContainer struct {
			Dvr []DVR `json:"Dvr"`
		} `json:"MediaContainer"`
	}
	err := c.doLimit(ctx, http.MethodGet, "/livetv/dvrs", nil, &out, maxLiveTVBody)
	return out.MediaContainer.Dvr, err
}

// CreateDVR creates a DVR over the device deviceUUID with lineup.
func (c *Client) CreateDVR(ctx context.Context, language, deviceUUID, lineup string) (DVR, error) {
	var out struct {
		MediaContainer struct {
			Dvr []DVR `json:"Dvr"`
		} `json:"MediaContainer"`
	}
	q := url.Values{"language": {language}, "device": {deviceUUID}, "lineup": {lineup}}
	if err := c.doLimit(ctx, http.MethodPost, "/livetv/dvrs", q, &out, maxLiveTVBody); err != nil {
		return DVR{}, err
	}
	if len(out.MediaContainer.Dvr) == 0 {
		return DVR{}, nil
	}
	return out.MediaContainer.Dvr[0], nil
}

// DeleteDVR removes a DVR, and with it its recording rules.
func (c *Client) DeleteDVR(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/livetv/dvrs/"+url.PathEscape(key), nil, nil)
}

// EPGChannelMap is PMS's pairing of the device's channels with lineup's.
func (c *Client) EPGChannelMap(ctx context.Context, deviceUUID, lineup string) ([]ChannelMapping, error) {
	var out struct {
		MediaContainer struct {
			ChannelMapping []ChannelMapping `json:"ChannelMapping"`
		} `json:"MediaContainer"`
	}
	q := url.Values{"device": {deviceUUID}, "lineup": {lineup}}
	err := c.doLimit(ctx, http.MethodGet, "/livetv/epg/channelmap", q, &out, maxLiveTVBody)
	return out.MediaContainer.ChannelMapping, err
}

// channelMapQuery is SaveChannelMap's query for m.
func channelMapQuery(m []ChannelMapping) url.Values {
	q := url.Values{}
	enabled := make([]string, 0, len(m))
	for _, cm := range m {
		enabled = append(enabled, cm.DeviceIdentifier)
		q.Set("channelMappingByKey["+cm.DeviceIdentifier+"]", cm.ChannelKey)
		q.Set("channelMapping["+cm.DeviceIdentifier+"]", cm.LineupIdentifier)
	}
	q.Set("channelsEnabled", strings.Join(enabled, ","))
	return q
}

// MapBytes is the length of SaveChannelMap's encoded query for m.
func MapBytes(m []ChannelMapping) int { return len(channelMapQuery(m).Encode()) }

// SaveChannelMap enables every mapping's channel on the device, in one
// request: PMS replaces the whole map on each save, so a batch would leave
// only its own channels (iptvtunerr's finding). An empty map, or one over
// MaxChannelMapBytes, is refused before anything is sent.
func (c *Client) SaveChannelMap(ctx context.Context, deviceKey string, m []ChannelMapping) error {
	if len(m) == 0 {
		return ErrEmptyChannelMap
	}
	q := channelMapQuery(m)
	if n := len(q.Encode()); n > MaxChannelMapBytes {
		return fmt.Errorf("%w: %d bytes for %d channels", ErrChannelMapTooLarge, n, len(m))
	}
	return c.do(ctx, http.MethodPut, "/media/grabbers/devices/"+url.PathEscape(deviceKey)+"/channelmap", q, nil)
}

// Activities lists PMS's running activities.
func (c *Client) Activities(ctx context.Context) ([]Activity, error) {
	var out struct {
		MediaContainer struct {
			Activity []Activity `json:"Activity"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodGet, "/activities", nil, &out)
	return out.MediaContainer.Activity, err
}

// Identity is the server's machineIdentifier (GET /identity): the same on
// every pod of one installation, so it names the installation that holds a
// Live TV block's Lease.
func (c *Client) Identity(ctx context.Context) (string, error) {
	var out struct {
		MediaContainer struct {
			MachineIdentifier string `json:"machineIdentifier"`
		} `json:"MediaContainer"`
	}
	if err := c.do(ctx, http.MethodGet, "/identity", nil, &out); err != nil {
		return "", err
	}
	if out.MediaContainer.MachineIdentifier == "" {
		return "", errors.New("plexapi: /identity names no machineIdentifier")
	}
	return out.MediaContainer.MachineIdentifier, nil
}

// ReloadGuide asks PMS to fetch a DVR's guide again.
func (c *Client) ReloadGuide(ctx context.Context, dvrKey string) error {
	return c.do(ctx, http.MethodPost, "/livetv/dvrs/"+url.PathEscape(dvrKey)+"/reloadGuide", nil, nil)
}
