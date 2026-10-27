package pipeline_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/stretchr/testify/require"
)

// supBox is one PGS display: a solid rectangle in one colour (BT.709
// limited-range Y, Cr, Cb) on a canvas, shown from start to end.
type supBox struct {
	start, end time.Duration
	x, y, w, h int
	cy, cr, cb byte
}

// writeSup writes a Blu-ray PGS stream (.sup) showing each box, then
// clearing it: FFmpeg has no PGS encoder, so tests make their own.
func writeSup(t *testing.T, canvasW, canvasH int, boxes ...supBox) string {
	t.Helper()
	var b bytes.Buffer
	seg := func(pts time.Duration, typ byte, data []byte) {
		ticks := uint32(pts * 90000 / time.Second)
		b.WriteString("PG")
		_ = binary.Write(&b, binary.BigEndian, ticks)
		_ = binary.Write(&b, binary.BigEndian, uint32(0))
		b.WriteByte(typ)
		_ = binary.Write(&b, binary.BigEndian, uint16(len(data)))
		b.Write(data)
	}
	u16 := func(v int) []byte { return []byte{byte(v >> 8), byte(v)} }
	for i, bx := range boxes {
		comp := 2 * i
		// Presentation composition: the canvas, an epoch start, one object.
		pcs := append(append(u16(canvasW), u16(canvasH)...), 0x10)
		pcs = append(pcs, u16(comp)...)
		pcs = append(pcs, 0x80, 0, 0, 1)
		pcs = append(pcs, u16(0)...)
		pcs = append(pcs, 0, 0)
		pcs = append(append(pcs, u16(bx.x)...), u16(bx.y)...)
		seg(bx.start, 0x16, pcs)
		wds := append([]byte{1, 0}, append(append(u16(bx.x), u16(bx.y)...), append(u16(bx.w), u16(bx.h)...)...)...)
		seg(bx.start, 0x17, wds)
		// Palette: 0 transparent, 1 the box's colour, opaque.
		seg(bx.start, 0x14, []byte{0, 0, 0, 16, 128, 128, 0, 1, bx.cy, bx.cr, bx.cb, 255})
		// Object: each line one run of colour 1, then end of line.
		var rle []byte
		for range bx.h {
			rle = append(rle, 0x00, 0xC0|byte(bx.w>>8), byte(bx.w), 0x01, 0x00, 0x00)
		}
		n := len(rle) + 4
		ods := append(u16(0), 0, 0xC0, byte(n>>16), byte(n>>8), byte(n))
		ods = append(append(append(ods, u16(bx.w)...), u16(bx.h)...), rle...)
		seg(bx.start, 0x15, ods)
		seg(bx.start, 0x80, nil)
		// Cleared: a composition with no object.
		clr := append(append(u16(canvasW), u16(canvasH)...), 0x10)
		clr = append(clr, u16(comp+1)...)
		clr = append(clr, 0x00, 0, 0, 0)
		seg(bx.end, 0x16, clr)
		seg(bx.end, 0x17, wds)
		seg(bx.end, 0x80, nil)
	}
	path := filepath.Join(t.TempDir(), "subs.sup")
	require.NoError(t, os.WriteFile(path, b.Bytes(), 0o644))
	return path
}

// red is BT.709 limited-range red.
var red = supBox{cy: 63, cr: 240, cb: 102}

func TestAPGSTrackDecodesToItsBitmap(t *testing.T) {
	// The writer and ffgo agree: the box decodes at its place and size, its
	// palette BGRA bytes red and opaque.
	need(t)
	box := red
	box.start, box.end, box.x, box.y, box.w, box.h = time.Second, 3*time.Second, 100, 600, 64, 32
	d, err := ffgo.NewDecoder(writeSup(t, 1920, 1080, box))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()
	sd, err := ffgo.NewSubtitleDecoder(d.SubtitleStream())
	require.NoError(t, err)
	defer func() { _ = sd.Close() }()
	var shown *ffgo.Subtitle
	for shown == nil {
		p, err := d.ReadPacket()
		require.NoError(t, err)
		require.NotNil(t, p, "no displayed subtitle")
		s, err := sd.Decode(p)
		require.NoError(t, err)
		if s != nil && len(s.Rects) > 0 {
			shown = s
		}
	}
	r := shown.Rects[0]
	require.Equal(t, []int{100, 600, 64, 32}, []int{r.X, r.Y, r.Width, r.Height})
	px := r.Palette[4*int(r.Data[0]):]
	bl, g, rd, a := px[0], px[1], px[2], px[3]
	require.Greater(t, int(rd), int(g)+150, "BGRA %v", px[:4])
	require.Less(t, int(bl), 60)
	require.Equal(t, byte(255), a)
	t.Logf("pts %d start %s end %s", shown.PTS, shown.StartTime, shown.EndTime)
}

// TestWriteTheBurnClip writes testdata/clips/burn.mkv, which the GPU image
// (no FFmpeg CLI) burns in: 4 s of gray at 1280x720, an ASS track (stream 1)
// reading HELLO and a PGS track (stream 2) with a red box at (100,500),
// both from 1 s to 3 s. Run with CLUSTERPLEX_WRITE_CLIPS=1.
func TestWriteTheBurnClip(t *testing.T) {
	if os.Getenv("CLUSTERPLEX_WRITE_CLIPS") != "1" {
		t.Skip("CLUSTERPLEX_WRITE_CLIPS=1 writes the clip")
	}
	need(t)
	dir := t.TempDir()
	box := red
	box.start, box.end, box.x, box.y, box.w, box.h = time.Second, 3*time.Second, 100, 500, 64, 32
	makeClip(t, "../testdata/clips/burn.mkv", "-copyts",
		"-f", "lavfi", "-i", "color=c=gray:size=1280x720:rate=24", "-i", srtFile(t, dir, "s.srt"), "-i", writeSup(t, 1280, 720, box),
		"-t", "4", "-map", "0:v", "-map", "1:s", "-map", "2:s",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:s:0", "ass", "-c:s:1", "copy")
}
