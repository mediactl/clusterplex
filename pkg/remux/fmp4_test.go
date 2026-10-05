package remux

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func box(typ string, body string) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// The writer muxer delivers bytes in AVIO-buffer chunks that cut boxes
// anywhere; the splitter returns whole init and whole fragments.
func TestTheSplitterCutsInitAndFragmentsAcrossWrites(t *testing.T) {
	init := cat(box("ftyp", "iso5"), box("moov", "tracks"))
	f1 := cat(box("moof", "one"), box("mdat", "AAAA"))
	f2 := cat(box("styp", "msdh"), box("moof", "two"), box("mdat", "BBBBBBBB"))
	stream := cat(init, f1, f2, box("mfra", "index"))

	var gotInit []byte
	var frags [][]byte
	s := &Splitter{
		OnInit:     func(b []byte) error { gotInit = append([]byte(nil), b...); return nil },
		OnFragment: func(b []byte) error { frags = append(frags, append([]byte(nil), b...)); return nil },
	}
	for i := 0; i < len(stream); i += 5 {
		n, err := s.Write(stream[i:min(i+5, len(stream))])
		require.NoError(t, err)
		require.Equal(t, min(5, len(stream)-i), n)
	}
	assert.Equal(t, init, gotInit)
	assert.Equal(t, [][]byte{f1, f2}, frags)
}

func TestAnUnsizedBoxIsAnError(t *testing.T) {
	b := box("mdat", "x")
	binary.BigEndian.PutUint32(b, 0) // "to the end of the file": no length to cut on
	_, err := (&Splitter{OnInit: func([]byte) error { return nil }, OnFragment: func([]byte) error { return nil }}).Write(b)
	assert.Error(t, err)
}
