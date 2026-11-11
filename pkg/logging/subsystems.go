package logging

// Subsystems is every subsystem a cluster-plex process names (the 2026-11-10
// structured-logging design §3, §5), sorted; test/guards holds every
// SubsystemLogger and Subsystem call to a literal in it, and every entry to
// a call. A name is dotted and lowercase, the unit that wrote the line:
// a package's (livetv, remux), a unit below it (livetv.guide.cloud,
// remux.relay), a third party bridged onto the stream (client-go), or Plex's
// own files once the tailer reads them (pms, pms.plugin).
var Subsystems = []string{
	"activity",
	"client-go",
	"clustarr.watch",
	"codecs",
	"collections",
	"db",
	"livetv",
	"livetv.grabs",
	"livetv.guide",
	"livetv.guide.build",
	"livetv.guide.cloud",
	"livetv.guide.xmltv",
	"livetv.restart",
	"logging",
	"maintenance",
	"net",
	"net.egress",
	"pms",
	"pms.dlna",
	"pms.plugin",
	"pms.scanner",
	"pms.stderr",
	"pms.stdout",
	"pms.transcoder",
	"pms.tuner",
	"projection",
	"provision",
	"proxy",
	"proxy.router",
	"remoteexec.dispatcher",
	"remoteexec.executor",
	"remux",
	"remux.worker",
	"scanner",
	"shim",
	"stdlog",
	"supervisor",
	"telemetry",
	"token",
	"video",
	"video.lease",
	"video.worker",
}
