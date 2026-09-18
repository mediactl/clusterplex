// Package remoteexec runs the Plex helper binaries that the shim intercepts,
// either on the local node or on a worker pod, and streams their output back.
package remoteexec

import "strings"

// DefaultPMSPort is the port Plex Media Server always listens on; it has no
// setting to change it.
const DefaultPMSPort = "32400"

// loopbackPMS lists the spellings Plex uses when it tells a child process how
// to reach the server that spawned it. On a remote worker they must point back
// at the leader instead.
var loopbackPMS = []string{"127.0.0.1:" + DefaultPMSPort, "localhost:" + DefaultPMSPort}

// RewriteArgs returns a copy of args with every loopback reference to Plex
// Media Server replaced by pmsAddr (host:port). The input is not modified.
func RewriteArgs(args []string, pmsAddr string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		for _, lb := range loopbackPMS {
			a = strings.ReplaceAll(a, lb, pmsAddr)
		}
		out[i] = a
	}
	return out
}
