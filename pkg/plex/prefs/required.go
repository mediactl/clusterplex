package prefs

import (
	"maps"
	"slices"
)

// customConnections is the address Plex hands to clients. Plex otherwise
// advertises only what it can see of itself, which here is a link-local
// address inside its own network namespace that no client can reach.
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

// Required returns the settings the architecture fixes, independent of any
// address the proxy is reachable on.
func Required() map[string]string {
	prefs := maps.Clone(required)
	for _, name := range scanScheduling {
		prefs[name] = "0"
	}
	return prefs
}

// Enforced returns everything the manager writes into Preferences.xml itself.
// externalURL is the address clients reach the proxy on; an empty one leaves
// whatever Plex already advertises in place, because clearing it is worse than
// not setting it. transcodeDir is the per-pod directory for transcode chunks,
// and is treated the same way.
func Enforced(externalURL, transcodeDir string) map[string]string {
	prefs := Required()
	maps.Copy(prefs, DisabledButlerTasks())
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
