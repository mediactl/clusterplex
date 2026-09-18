package plexprefs

import "maps"

// customConnections is the address Plex hands to clients. Plex otherwise
// advertises only what it can see of itself, which here is a link-local
// address inside its own network namespace that no client can reach.
const customConnections = "customConnections"

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
}

// Required returns the settings the architecture fixes, independent of any
// address the proxy is reachable on.
func Required() map[string]string { return maps.Clone(required) }

// Enforced returns everything the manager writes into Preferences.xml itself.
// externalURL is the address clients reach the proxy on; an empty one leaves
// whatever Plex already advertises in place, because clearing it is worse than
// not setting it.
func Enforced(externalURL string) map[string]string {
	prefs := maps.Clone(required)
	maps.Copy(prefs, DisabledButlerTasks())
	if externalURL != "" {
		prefs[customConnections] = externalURL
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
	case IsButlerTask(name):
		return "each pod would run its own copy of the scheduler; maintenance is scheduled as CronJobs"
	default:
		if _, ok := required[name]; ok {
			return "this architecture depends on its value"
		}
		return ""
	}
}
