package plexseed_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

func box(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	binary.BigEndian.PutUint32(b, uint32(8+payload))
	copy(b[4:], typ)
	return b
}

func largeBox(typ string, payload int) []byte {
	b := make([]byte, 16+payload)
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:], typ)
	binary.BigEndian.PutUint64(b[8:], uint64(16+payload))
	return b
}

// A faststart MP4 (ftyp, then moov) is optimized for streaming; one whose
// mdat comes first is not, however the mdat is sized; the walk reads the
// headers alone, so a huge mdat costs nothing.
func TestFastStartReadsTheLeadingBoxesAlone(t *testing.T) {
	tests := []struct {
		name string
		file []byte
		want bool
	}{
		{"ftyp then moov", bytes.Join([][]byte{box("ftyp", 24), box("moov", 100), box("mdat", 10)}, nil), true},
		{"ftyp, free, moov", bytes.Join([][]byte{box("ftyp", 24), box("free", 8), box("moov", 100)}, nil), true},
		{"ftyp then mdat", bytes.Join([][]byte{box("ftyp", 24), box("mdat", 10), box("moov", 100)}, nil), false},
		{"ftyp, free, large mdat", bytes.Join([][]byte{box("ftyp", 24), box("free", 0), largeBox("mdat", 10), box("moov", 100)}, nil), false},
		{"ftyp, mdat to the end", bytes.Join([][]byte{box("ftyp", 24), append([]byte{0, 0, 0, 0}, []byte("mdat")...), make([]byte, 40)}, nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := plexseed.FastStart(bytes.NewReader(tt.file))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Anything that is not an ISO base media file, or opens with a box the
// walk does not know, is an error: the flag then stays unset.
func TestFastStartRefusesWhatItCannotRead(t *testing.T) {
	for name, file := range map[string][]byte{
		"matroska":  []byte("\x1a\x45\xdf\xa3" + "matroska....."),
		"empty":     {},
		"short":     box("ftyp", 24)[:12],
		"odd box":   bytes.Join([][]byte{box("ftyp", 24), box("junk", 4), box("moov", 8)}, nil),
		"no answer": box("ftyp", 24),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := plexseed.FastStart(bytes.NewReader(file))
			assert.Error(t, err)
		})
	}
}
