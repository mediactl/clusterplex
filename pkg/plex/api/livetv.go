package api

import (
	"context"
	"encoding/json"
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

// Device is a tuner device PMS knows (GET /media/grabbers/devices). UUID is
// its identifier, "device://tv.plex.grabbers.hdhomerun/<DeviceID>".
type Device struct {
	Key      Str    `json:"key"`
	UUID     string `json:"uuid"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	DeviceID string `json:"deviceId"`
	State    string `json:"state"`
}

// DVR is a PMS DVR (GET /livetv/dvrs): a lineup over one or more devices.
type DVR struct {
	Key     Str      `json:"key"`
	UUID    string   `json:"uuid"`
	Title   string   `json:"title"`
	Lineup  string   `json:"lineup"`
	Devices []Device `json:"Device"`
}

// ChannelMapping pairs a device channel with a lineup channel, as PMS's EPG
// matched them (GET /livetv/epg/channelmap) and as a channel-map save sends
// them back.
type ChannelMapping struct {
	ChannelKey       string `json:"channelKey"`
	DeviceIdentifier string `json:"deviceIdentifier"`
	LineupIdentifier string `json:"lineupIdentifier"`
}

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

// DiscoverDevice asks PMS to probe uri, returning the device found there.
func (c *Client) DiscoverDevice(ctx context.Context, uri string) ([]Device, error) {
	var out struct {
		MediaContainer struct {
			Device []Device `json:"Device"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodPost, "/media/grabbers/devices/discover", url.Values{"uri": {uri}}, &out)
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

// SaveChannelMap enables every mapping's channel on the device, in one
// request: PMS replaces the whole map on each save, so a batch would leave
// only its own channels (iptvtunerr's finding).
func (c *Client) SaveChannelMap(ctx context.Context, deviceKey string, m []ChannelMapping) error {
	q := url.Values{}
	enabled := make([]string, 0, len(m))
	for _, cm := range m {
		enabled = append(enabled, cm.DeviceIdentifier)
		q.Set("channelMappingByKey["+cm.DeviceIdentifier+"]", cm.ChannelKey)
		q.Set("channelMapping["+cm.DeviceIdentifier+"]", cm.LineupIdentifier)
	}
	q.Set("channelsEnabled", strings.Join(enabled, ","))
	return c.do(ctx, http.MethodPut, "/media/grabbers/devices/"+url.PathEscape(deviceKey)+"/channelmap", q, nil)
}

// ReloadGuide asks PMS to fetch a DVR's guide again.
func (c *Client) ReloadGuide(ctx context.Context, dvrKey string) error {
	return c.do(ctx, http.MethodPost, "/livetv/dvrs/"+url.PathEscape(dvrKey)+"/reloadGuide", nil, nil)
}
