package prefs

import "slices"

// ButlerTasks are Plex's internal background maintenance jobs.
//
// Each Plex process runs its own copy of this scheduler, on its own timer, with
// no awareness of any other. With one server that is what you want. With
// several serving one shared library it is not: every pod wakes up and analyses
// the same media, regenerates the same thumbnails and hammers the same metadata
// providers at once, multiplying the load on the database and on rate-limited
// APIs by the number of pods.
//
// So the internal scheduler is turned off and the work is scheduled once,
// centrally, and handed to a single pod per item.
var ButlerTasks = []string{
	"ButlerTaskAnalyzeMedia",
	"ButlerTaskBackupDatabase",
	"ButlerTaskCleanOldBundles",
	"ButlerTaskCleanOldCacheFiles",
	"ButlerTaskDeepMediaAnalysis",
	"ButlerTaskGenerateAutoTags",
	"ButlerTaskGenerateChapterThumbs",
	"ButlerTaskGenerateMediaIndexFiles",
	"ButlerTaskRefreshLocalMedia",
	"ButlerTaskRefreshPeriodicMetadata",
	"ButlerTaskUpgradeMediaAnalysis",
}

// DisabledButlerTasks returns every Butler task set to off.
//
// These are applied on every start, so switching one back on in the Plex user
// interface does not survive a restart. That is deliberate: a single pod
// quietly re-enabling its own scheduler is the failure this prevents, and it
// would show up as unexplained load rather than as an error.
func DisabledButlerTasks() map[string]string {
	out := make(map[string]string, len(ButlerTasks))
	for _, task := range ButlerTasks {
		out[task] = "0"
	}
	return out
}

// IsButlerTask reports whether name is one of Plex's background tasks.
func IsButlerTask(name string) bool {
	return slices.Contains(ButlerTasks, name)
}
