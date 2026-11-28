package plexposters

import (
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics counts the poster requests the proxy saw.
type Metrics struct {
	requests *prometheus.CounterVec
}

// NewMetrics builds the counter and registers it with reg (nil registers
// nothing).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{requests: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "clusterplex_poster_requests_total",
		Help: "Requests to Plex's photo transcoder the proxy saw, by the client's platform, the width asked for (the bucket's upper bound, in pixels) and what the request would have met",
	}, []string{"platform", "width", "request"})}
	if reg != nil {
		reg.MustRegister(m.requests)
	}
	return m
}

func (m *Metrics) count(r Request, request string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(platformLabel(r.Platform), widthLabel(r.Width), request).Inc()
}

// platforms are the X-Plex-Platform values kept as a label; any other is
// "other", so a client cannot grow the series by naming itself.
var platforms = map[string]string{}

func init() {
	for _, p := range []string{
		"iOS", "Android", "tvOS", "Roku", "Chrome", "Safari", "Firefox", "Microsoft Edge", "Opera",
		"macOS", "Windows", "Linux", "webOS", "Tizen", "Xbox", "PlayStation", "Chromecast", "Kodi",
		"Plex Home Theater", "Konvergo", "Alexa", "Sonos", "Vizio", "Netcast",
	} {
		platforms[strings.ToLower(p)] = p
	}
}

func platformLabel(p string) string {
	if p == "" {
		return "none"
	}
	if known, ok := platforms[strings.ToLower(p)]; ok {
		return known
	}
	return "other"
}

// widthBuckets are the upper bounds, in pixels asked for, a request's width
// is counted under.
var widthBuckets = []int{100, 150, 200, 250, 300, 400, 450, 500, 600, 800, 1000}

func widthLabel(w int) string {
	if w <= 0 {
		return "none"
	}
	for _, b := range widthBuckets {
		if w <= b {
			return strconv.Itoa(b)
		}
	}
	return "+Inf"
}
