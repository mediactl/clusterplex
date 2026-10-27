// Package plexseed: the MP4 layout Plex's streaming flag reports.
package plexseed

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrNotMP4 is FastStart's answer for a file that does not open with an
// ISO base media file box.
var ErrNotMP4 = errors.New("plexseed: not an ISO base media file")

// fastStartMaxBoxes bounds the walk: a progressive file has ftyp and moov
// in its first two or three boxes, and an mdat is the answer either way.
const fastStartMaxBoxes = 16

// FastStart reports whether an ISO base media file (MP4, M4V, MOV) is
// "optimized for streaming" as Plex's analysis records it: its moov atom
// comes before its mdat, so a player has the index before the samples.
// It reads the file's leading box headers only, stepping over ftyp, free,
// skip, wide and uuid, and answers at the first moov (true) or mdat
// (false). A file that opens with no box, or whose first boxes name
// neither, is an error, never a guess.
func FastStart(r io.ReaderAt) (bool, error) {
	var off int64
	for i := 0; i < fastStartMaxBoxes; i++ {
		var hdr [16]byte
		n, err := r.ReadAt(hdr[:8], off)
		if err != nil && !(errors.Is(err, io.EOF) && n == 8) {
			if i == 0 {
				return false, fmt.Errorf("%w: %v", ErrNotMP4, err)
			}
			return false, fmt.Errorf("plexseed: box at %d: %w", off, err)
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		if i == 0 && typ != "ftyp" {
			return false, fmt.Errorf("%w: first box is %q", ErrNotMP4, typ)
		}
		switch size {
		case 1:
			if n, err := r.ReadAt(hdr[8:16], off+8); err != nil && !(errors.Is(err, io.EOF) && n == 8) {
				return false, fmt.Errorf("plexseed: box at %d: %w", off, err)
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
		case 0:
			// The box runs to the end of the file: only an mdat does that.
			size = -1
		}
		switch typ {
		case "moov":
			return true, nil
		case "mdat":
			return false, nil
		case "ftyp", "free", "skip", "wide", "uuid", "pdin", "styp", "sidx", "prft":
		default:
			return false, fmt.Errorf("plexseed: unexpected box %q at %d before moov or mdat", typ, off)
		}
		if size < 8 {
			return false, fmt.Errorf("plexseed: box %q at %d has size %d", typ, off, size)
		}
		off += size
	}
	return false, fmt.Errorf("plexseed: no moov or mdat in the first %d boxes", fastStartMaxBoxes)
}
