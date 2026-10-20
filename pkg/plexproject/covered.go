package plexproject

// A file holding several episodes is on each of them, as Plex's own
// scanner puts it: a media row and a part per episode, all pointing at the
// one file. The row on the episode the MediaFile's mediaRef names carries
// the file's UID, as every single-episode file's does; the row on each
// other episode it covers (mediaRef.keys) carries CoveredUID.

// CoveredUID is the cp:mediaFile stamp of the row a multi-episode file has
// on episode, one of its mediaRef.keys other than the episode its mediaRef
// names. A UID holds no '#', and an Episode name cannot.
func CoveredUID(fileUID, episode string) string { return fileUID + "#" + episode }

// CoveredUIDs lists every stamp a MediaFile's rows carry: fileUID for the
// episode its mediaRef names, then CoveredUID for each other key, once.
func CoveredUIDs(fileUID, name string, keys []string) []string {
	out := []string{fileUID}
	seen := map[string]bool{name: true}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, CoveredUID(fileUID, k))
	}
	return out
}
