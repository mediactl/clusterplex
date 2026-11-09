package prefs

import (
	"maps"
	"slices"
)

// customConnections is the address Plex hands to clients, or several as a
// comma-separated list, which plex.tv passes on whole and a client tries in
// order. Plex otherwise advertises only what it can see of itself, which
// here is a link-local address inside its own network namespace that no
// client can reach.
const customConnections = "customConnections"

// transcoderTempDirectory is where Plex writes the chunks it transcodes into.
// Every pod runs Plex and they share one volume for the identity and the
// metadata, so this has to be steered onto per-pod storage: the chunks are
// written and read back by the single pod serving that session, which the
// proxy pins. Left alone Plex puts them under Cache, which is shared.
const transcoderTempDirectory = "TranscoderTempDirectory"

// required are the settings this architecture depends on, as opposed to the
// ones an operator chooses. They are written on every start and refused as
// configuration, because each of them has exactly one correct value here and a
// wrong one fails in a way that looks like something else.
var required = map[string]string{
	// Remote access publishing. Every pod runs under one server identity, so
	// each would publish its own address to plex.tv and the last to check in
	// would win. Clients would then be handed a pod that only sometimes
	// answers. The proxy is what clients reach, advertised through
	// customConnections; plex.tv still hands that out with publishing off.
	"PublishServerOnPlexOnlineKey": "0",
	// Manual port mapping, which is to say no UPnP and no NAT-PMP. Plex would
	// otherwise ask the router to forward a port straight to a pod address,
	// routing around the proxy and the load balancer together.
	"ManualPortMappingMode": "1",
	// Local discovery, off. GDM broadcasts a pod's own address on the LAN, so
	// every pod would announce itself separately under one server identity and
	// a client on the same network would reach a pod directly rather than the
	// proxy — which is what pins its session. Clients find the server through
	// plex.tv and customConnections instead.
	"GdmEnabled": "0",
	// Marker detection: clustarr detects intros and credits itself and
	// seeds them (ADR 0006; clustarr's spec 2026-10-01 segment detection),
	// so Plex's pods spend no CPU on it. Intro and credits generation are
	// off outright. MarkerSource alone was not enough: it governs credits
	// only, and with the two behaviours at their "asap" default every pod
	// re-ran intro detection over 3,938 items at start and plex-2 ran its
	// credits detector (live logs, 2026-10-01). MarkerSource stays "cloud"
	// ("only online (no local detection)") so that credits generation,
	// were it ever turned back on, could not run locally. Names and options
	// read from a live /:/prefs, 2026-10-01. These, not the libraries'
	// own intro and credits switches, keep detection off: the provisioner
	// leaves those switches on, since Plex serves a library's markers only
	// while they are (talos, 2026-10-20).
	"GenerateIntroMarkerBehavior":   "never",
	"GenerateCreditsMarkerBehavior": "never",
	"MarkerSource":                  "cloud",
	// Commercial (ad) detection, likewise local analysis; no DVR here.
	"GenerateAdMarkerBehavior": "never",
	// Every other analysis Plex runs on its own. clustarr probes and
	// detects, so Plex is a UI and API. A behaviour at "scheduled" runs in
	// the maintenance window whatever the ButlerTask switches say: plex-2
	// ran loudness analysis over 11,922 audio tracks and sonic analysis at
	// 04:37 with every one of them off (live logs, 2026-10-01). Preview
	// thumbnails and voice activity were "never" only by default.
	"LoudnessAnalysisBehavior":     "never",
	"MusicAnalysisBehavior":        "never",
	"GenerateChapterThumbBehavior": "never",
	"GenerateBIFBehavior":          "never",
	"GenerateVADBehavior":          "never",
	// Plex may convert video (owner, 2026-10-26). From 2026-10-01 this was
	// "1", Plex never re-encoding video, since squasharr made the files
	// playable. But Plex for iOS asks Live TV for a video transcode
	// (directPlay=0, directStream=0) even on the LAN at original quality,
	// and every such tune was refused with decision 4010, "the server has
	// reached its configured limit for maximum simultaneous video
	// transcodes". A conversion runs on the CPU of the pod's node, which on
	// talos is a 4-core arm64 one, so any client that cannot copy a stream
	// now costs a real-time transcode, library playback included.
	"TranscoderCanOnlyRemuxVideo": "0",
	// A relayed connection is capped, so Plex would have to transcode the
	// video down to it on the pod's CPU; the relay stays off and a client
	// that cannot reach the server fails to connect rather than stream a
	// transcode through plex.tv's capped relay.
	"RelayEnabled": "0",
	// clustarr owns the files. A Plex user deleting one goes behind its
	// back; an automatic trash empty after a scan that met a stalled NFS
	// mount deletes the items whose files only looked missing. The
	// empty-trash maintenance CronJob still empties it deliberately.
	"allowMediaDeletion": "0",
	"autoEmptyTrash":     "0",
}

// scanScheduling are the settings that start a library scan without anyone
// asking for one. Required turns every one of them off.
//
// The Butler list covers analysis, thumbnails and metadata refresh, but none
// of those discovers files: ButlerTaskRefreshLocalMedia refreshes items Plex
// already knows about. Scanning has its own switches, and they were the hole
// left in that list.
//
// Every pod mounts the same media on the same paths, so each would watch the
// same tree, wake on the same event and scan it into the same PostgreSQL
// library concurrently — and Plex's scanner assumes it is the only one
// running. All of these default off, so turning them off closes a door rather
// than changing today's behaviour; a pod re-enabling its own scanner in the
// web interface would show up as database load rather than as an error.
//
// Scanning still happens. The refresh maintenance task is fanned out to one
// pod per library, the same way the Butler work is.
var scanScheduling = []string{
	"FSEventLibraryUpdatesEnabled",
	"FSEventLibraryPartialScanEnabled",
	"ScheduledLibraryUpdatesEnabled",
}

func isScanScheduling(name string) bool { return slices.Contains(scanScheduling, name) }

// refused are settings an operator may not declare and the manager does not
// write either. They differ from required in that this architecture has no
// correct value for them — the setting simply cannot mean here what it means
// on a server Plex was designed for, so the honest answer is to refuse it
// rather than to pick a value.
var refused = map[string]string{
	// allowedNetworks grants access without authentication to clients whose
	// source address falls in the list. Plex never sees a client's source
	// address here: it runs in its own network namespace (ADR-0003) behind the
	// in-pod L4 proxy, which dials upstream with an ordinary Dialer, so every
	// request arrives from the pod end of the veth.
	//
	// That makes it all-or-nothing. A range covering the link subnet drops
	// authentication for everyone who reaches the proxy, including from the
	// internet; any other range matches nothing and silently does nothing.
	// Neither is what an operator writing a LAN range intends.
	"allowedNetworks": "Plex sees every request arriving from the pod end of the veth, " +
		"so this would drop authentication for all clients or for none; restrict access at the proxy instead",
}

// defaults are the settings cluster-plex chooses for every install and an
// operator may still declare otherwise. They are written on every start
// under the declared preferences, so a value set by hand in the Plex web
// interface does not linger past a restart, and refused nowhere, since each
// has a defensible other value on some install -- which is what separates
// them from required.
var defaults = map[string]string{
	// Crash reports to Plex, off (owner, 2026-11-04; PMS defaults it on). A
	// crash here is in a server whose SQLite is a shim over PostgreSQL and
	// whose helper binaries are wrappers, so a report would describe
	// cluster-plex's bugs to Plex. The uploader is a no-op binary in this
	// image and the bootstrap empties Crash Reports before every start
	// (pkg/plex/bootstrap), so the switch only decides whether PMS spawns
	// it; off, it does not.
	"sendCrashReports": "0",
	// PMS's incremental guide loader, on (owner, 2026-11-04). Hidden, off by
	// default and documented nowhere by Plex. Without it PMS 1.43.5 loads
	// every mapped channel's 14 days whole at each load -- a saved channel
	// map's and the lease holder's daily reload of a cloud guide alike,
	// about 30 min for 233 channels, each copied into PostgreSQL at the
	// load's rename -- so a guide refresh costs a whole load whatever moved.
	// What the incremental loader does to that path is read off the first
	// load under it; until then it was false and untested.
	"DvrIncrementalEpgLoader": "1",
}

// Defaults returns the settings cluster-plex chooses but an operator may
// declare otherwise.
func Defaults() map[string]string { return maps.Clone(defaults) }

// Required returns the settings the architecture fixes, independent of any
// address the proxy is reachable on.
func Required() map[string]string {
	prefs := maps.Clone(required)
	for _, name := range scanScheduling {
		prefs[name] = "0"
	}
	return prefs
}

// GuideRefresh is the one butler task Live TV needs on: RefreshEpgGuides,
// PMS's nightly guide refresh, which extends each DVR's guide
// incrementally (two-day chunks of a 14-day window, about ten minutes
// for 248 channels once the shim stopped timing its whole-guide read out,
// 2026-11-04). Nothing else refreshes a guide: a channel-map save or
// reloadGuide rebuilds it whole, half an hour at a core, and the task
// cannot be run by hand (POST /butler/RefreshEpgGuides answers and does
// nothing outside its schedule). It runs in the ButlerStartHour-EndHour
// window, in the pod's time, UTC; the operator sets those. PMS also
// refreshes a guide on its own: at its start when the guide reads stale,
// and on a timer 24 hours after its last refresh, which this setting does
// not govern. That is why Live TV holds the deployment to one Plex pod (the
// chart and the manager refuse a second): on the kind rig, 2026-11-09, a
// second pod's PMS refreshed the one shared guide at its boot.
func GuideRefresh() map[string]string { return map[string]string{"ButlerTaskRefreshEpgGuides": "1"} }

// Enforced returns everything the manager writes into Preferences.xml itself.
// liveTV keeps the guide refresh task on (GuideRefresh).
// externalURL is the address clients reach the proxy on, or several
// comma-separated in the order clients should prefer them, written as Plex
// reads them; an empty one leaves whatever Plex already advertises in place,
// because clearing it is worse than not setting it. transcodeDir is the
// per-pod directory for transcode chunks, and is treated the same way.
func Enforced(externalURL, transcodeDir string, liveTV bool) map[string]string {
	prefs := Required()
	maps.Copy(prefs, DisabledButlerTasks())
	if liveTV {
		maps.Copy(prefs, GuideRefresh())
	}
	if externalURL != "" {
		prefs[customConnections] = externalURL
	}
	if transcodeDir != "" {
		prefs[transcoderTempDirectory] = transcodeDir
	}
	return prefs
}

// forcedBy explains why a setting cannot be declared, or returns "" if it can.
// The message names what to set instead, since every one of these is reachable
// some other way.
func forcedBy(name string) string {
	switch {
	case name == customConnections:
		return "it must match the address the proxy actually serves; set plex.external-url"
	case name == transcoderTempDirectory:
		return "it must match the per-pod volume the pod actually mounts; set plex.transcode-dir"
	case IsButlerTask(name):
		return "each pod would run its own copy of the scheduler; maintenance is scheduled as CronJobs"
	case isScanScheduling(name):
		return "every pod would scan the same media into the same library at once; " +
			"schedule the refresh maintenance task as a CronJob instead"
	default:
		if why, ok := refused[name]; ok {
			return why
		}
		if _, ok := required[name]; ok {
			return "this architecture depends on its value"
		}
		return ""
	}
}
