// Command hdhrprobe is a throwaway HDHomeRun device with N synthetic
// channels and an XMLTV guide for them, for recording PMS's Live TV calls
// (docs/livetv-pms-calls.md). It serves no stream. /channels?n= changes N.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

const deviceID = "CAFE0001"

func main() {
	addr := flag.String("addr", ":5004", "listen address")
	n := flag.Int("channels", 100, "channel count")
	flag.Parse()
	var count atomic.Int64
	count.Store(int64(*n))
	base := func(r *http.Request) string { return "http://" + r.Host }
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/discover.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"FriendlyName": "clustarr probe", "Manufacturer": "Silicondust", "ModelNumber": "HDTC-2US",
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
			num := strconv.Itoa(1000 + i)
			out = append(out, map[string]string{"GuideNumber": num, "GuideName": "Probe " + strconv.Itoa(i), "URL": base(r) + "/stream/" + num})
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/lineup.post", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/device.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><URLBase>%s</URLBase><device><deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType><friendlyName>clustarr probe</friendlyName><manufacturer>Silicondust</manufacturer><modelName>HDTC-2US</modelName><modelNumber>HDTC-2US</modelNumber><serialNumber>%s</serialNumber><UDN>uuid:%s</UDN></device></root>`, base(r), deviceID, deviceID)
	})
	mux.HandleFunc("/livetv/probe/probe/xmltv.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		start := time.Now().UTC().Truncate(time.Hour)
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<tv generator-info-name="hdhrprobe">`+"\n")
		for i := 0; i < int(count.Load()); i++ {
			fmt.Fprintf(w, `<channel id="%d"><display-name>Probe %d</display-name></channel>`+"\n", 1000+i, i)
		}
		for i := 0; i < int(count.Load()); i++ {
			for h := 0; h < 48; h += 2 {
				s, e := start.Add(time.Duration(h)*time.Hour), start.Add(time.Duration(h+2)*time.Hour)
				fmt.Fprintf(w, `<programme start="%s" stop="%s" channel="%d"><title>Probe %d</title></programme>`+"\n",
					s.Format("20060102150405 -0700"), e.Format("20060102150405 -0700"), 1000+i, i)
			}
		}
		fmt.Fprint(w, "</tv>\n")
	})
	mux.HandleFunc("/channels", func(w http.ResponseWriter, r *http.Request) {
		if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v >= 0 {
			count.Store(int64(v))
		}
		fmt.Fprintln(w, count.Load())
	})
	log.Printf("hdhrprobe on %s with %d channels", *addr, count.Load())
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
