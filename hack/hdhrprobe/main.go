// Command hdhrprobe is a throwaway HDHomeRun device with N synthetic
// channels and an XMLTV guide for them, for recording PMS's Live TV calls
// (docs/livetv-pms-calls.md). /channels?n= changes N.
//
// With --device it serves several devices, each on its own address with its
// own DeviceID and first channel number, as clustarr's blocks are; with
// --forward it pipes bytes from one address to another, as cluster-plex's
// proxies do. /stream/<n> is endless null MPEG-TS, and each request for it
// is logged, so a tune is visible.
package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, " ") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	addr := flag.String("addr", ":5004", "listen address of the default device")
	id := flag.String("device-id", "CAFE0001", "DeviceID of the default device")
	n := flag.Int("channels", 100, "channel count of every device")
	var devices, forwards list
	flag.Var(&devices, "device", "addr,deviceID,firstNumber: one more device (repeatable); replaces the default")
	flag.Var(&forwards, "forward", "listen=target: a byte-level TCP forward (repeatable)")
	flag.Parse()

	var count atomic.Int64
	count.Store(int64(*n))
	for _, f := range forwards {
		listen, target, ok := strings.Cut(f, "=")
		if !ok {
			log.Fatalf("--forward %q: want listen=target", f)
		}
		go forward(listen, target)
	}
	if len(devices) == 0 {
		devices = list{*addr + "," + *id + ",1000"}
	}
	errs := make(chan error, len(devices))
	for _, d := range devices {
		parts := strings.Split(d, ",")
		if len(parts) != 3 {
			log.Fatalf("--device %q: want addr,deviceID,firstNumber", d)
		}
		first, err := strconv.Atoi(parts[2])
		if err != nil {
			log.Fatalf("--device %q: %v", d, err)
		}
		mux := deviceMux(parts[1], first, &count)
		srv := &http.Server{Addr: parts[0], Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		log.Printf("hdhrprobe device %s on %s, channels from %d (%d)", parts[1], parts[0], first, count.Load())
		go func() { errs <- srv.ListenAndServe() }()
	}
	log.Fatal(<-errs)
}

func deviceMux(deviceID string, first int, count *atomic.Int64) *http.ServeMux {
	base := func(r *http.Request) string { return "http://" + r.Host }
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/discover.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"FriendlyName": "clustarr probe " + strconv.Itoa(first) + "+", "Manufacturer": "Silicondust", "ModelNumber": "HDTC-2US",
			"FirmwareName": "hdhomeruntc_atsc", "FirmwareVersion": "20150826", "DeviceID": deviceID,
			"DeviceAuth": "clustarr", "TunerCount": 2, "BaseURL": base(r), "LineupURL": base(r) + "/lineup.json",
		})
	})
	mux.HandleFunc("/lineup_status.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ScanInProgress": 0, "ScanPossible": 1, "Source": "Cable", "SourceList": []string{"Cable"}})
	})
	mux.HandleFunc("/lineup.json", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]string, 0, count.Load())
		for i := 0; i < int(count.Load()); i++ {
			num := strconv.Itoa(first + i)
			out = append(out, map[string]string{"GuideNumber": num, "GuideName": "Probe " + num, "URL": base(r) + "/stream/" + num})
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/lineup.post", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/device.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><URLBase>%s</URLBase><device><deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType><friendlyName>clustarr probe</friendlyName><manufacturer>Silicondust</manufacturer><modelName>HDTC-2US</modelName><modelNumber>HDTC-2US</modelNumber><serialNumber>%s</serialNumber><UDN>uuid:%s</UDN></device></root>`, base(r), deviceID, deviceID)
	})
	guide := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		// The guide is the same for every request on one UTC day, so two loads
		// of it (one copied in, one made in PostgreSQL) can be diffed.
		start := time.Now().UTC().Truncate(24 * time.Hour)
		_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<tv generator-info-name="hdhrprobe">`+"\n")
		for i := 0; i < int(count.Load()); i++ {
			_, _ = fmt.Fprintf(w, `<channel id="%d"><display-name>Probe %d</display-name></channel>`+"\n", first+i, first+i)
		}
		for i := 0; i < int(count.Load()); i++ {
			for slot := 0; slot < 24; slot++ {
				s, e := start.Add(time.Duration(2*slot)*time.Hour), start.Add(time.Duration(2*slot+2)*time.Hour)
				t := titles[(i*7+slot)%len(titles)]
				_, _ = fmt.Fprintf(w, `<programme start="%s" stop="%s" channel="%d"><title>%s</title>`,
					s.Format("20060102150405 -0700"), e.Format("20060102150405 -0700"), first+i, esc(t.title))
				if t.sub != "" {
					// A show's episodes recur across channels under the same
					// sub-title, so the loader's dupe check merges them.
					_, _ = fmt.Fprintf(w, `<sub-title>%s %d</sub-title><episode-num system="xmltv_ns">0.%d.</episode-num>`, esc(t.sub), slot%4, slot%4)
				}
				_, _ = fmt.Fprintf(w, `<desc>%s</desc><category>%s</category></programme>`+"\n", esc(t.desc), esc(t.category))
			}
		}
		_, _ = fmt.Fprint(w, "</tv>\n")
	}
	mux.HandleFunc("/livetv/probe/probe/xmltv.xml", guide)
	mux.HandleFunc("/xmltv.xml", guide)
	mux.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("stream %s for %s via Host %s", strings.TrimPrefix(r.URL.Path, "/stream/"), r.RemoteAddr, r.Host)
		w.Header().Set("Content-Type", "video/mp2t")
		pkt := make([]byte, 188*64)
		for i := 0; i < len(pkt); i += 188 {
			pkt[i], pkt[i+1], pkt[i+2], pkt[i+3] = 0x47, 0x1F, 0xFF, 0x10
			for j := 4; j < 188; j++ {
				pkt[i+j] = 0xFF
			}
		}
		sent := 0
		for r.Context().Err() == nil {
			if _, err := w.Write(pkt); err != nil {
				break
			}
			sent += len(pkt)
			time.Sleep(10 * time.Millisecond)
		}
		log.Printf("stream %s ended after %d bytes", strings.TrimPrefix(r.URL.Path, "/stream/"), sent)
	})
	mux.HandleFunc("/channels", func(w http.ResponseWriter, r *http.Request) {
		if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v >= 0 {
			count.Store(int64(v))
		}
		_, _ = fmt.Fprintln(w, count.Load())
	})
	return mux
}

// forward pipes every connection on listen to target, byte for byte, as
// cluster-plex's proxy.TCP does.
func forward(listen, target string) {
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalf("forward %s: %v", listen, err)
	}
	log.Printf("forward %s -> %s", listen, target)
	for {
		c, err := lis.Accept()
		if err != nil {
			log.Fatalf("forward %s: %v", listen, err)
		}
		go func() {
			defer func() { _ = c.Close() }()
			u, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				log.Printf("forward %s: %v", target, err)
				return
			}
			defer func() { _ = u.Close() }()
			go func() { _, _ = io.Copy(u, c) }()
			_, _ = io.Copy(c, u)
		}()
	}
}

// titles are the guide's programmes: the shapes the loader inlines as SQL
// literals (apostrophes, doubled quotes, double quotes, backslashes,
// non-ASCII, XML specials), so a load made in PostgreSQL is tried on them.
var titles = []struct{ title, sub, desc, category string }{
	{"America's Funniest Home Videos", "Viewers' Clips", "Clips sent in by viewers.", "Comedy"},
	{`The "Quoted" Show`, "Pilot", `A title with "double quotes" and a backslash \ in it.`, "Drama"},
	{"Café Müller: Übung", "Ouvertüre", "Non-ASCII: ñ, ü, 日本語, 😀.", "Arts"},
	{"Bob's Burgers", "Human Flesh", "Bob's restaurant.", "Animation"},
	{`C:\Path\To\Show`, `Back\slash`, "Backslashes in the title.", "Documentary"},
	{"It's Always Sunny in Philadelphia", "The Gang Gets Quarantined", "The Gang.", "Comedy"},
	{"Paid Programming", "", "Paid programming.", "Shopping"},
	{"The Godfather", "", "A film.", "Movie"},
	{"Who's the Boss? & <Friends>", "Pilot", "Question mark, ampersand and <angle brackets>.", "Sitcom"},
	{"O'Brien's ''Doubled'' Quotes", "Part", "Two single quotes in a row: ''.", "Talk"},
	{"Probe News", "Evening Edition", "The news.", "News"},
	{"100% Wrestling", "Night", "A percent sign and a semicolon; here.", "Sports"},
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
