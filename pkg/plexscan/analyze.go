package plexscan

import "strconv"

// ParseAnalyze reads a scanner argv (without argv[0]). Plex 1.43 runs the
// binary only as "--analyze --log-file-suffix Analysis --no-thumbs --item N"
// (the capture doc); anything that is not an analyze of one item is left to
// the binary.
func ParseAnalyze(args []string) (int64, bool) {
	analyze, item := false, int64(0)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--analyze", "-a":
			analyze = true
		case "--item", "-i":
			if i+1 >= len(args) {
				return 0, false
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return 0, false
			}
			item = n
			i++
		case "--scan", "-s", "--tree", "-t":
			return 0, false
		}
	}
	return item, analyze && item != 0
}
