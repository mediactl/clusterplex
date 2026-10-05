package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/remux"
)

func key(input string) Key {
	return Key{
		Input: input, Size: 10, ModTime: time.Unix(100, 0), AudioStream: 1, Channels: 2,
		SampleRate: 96000, BitRate: 256000, SegmentDuration: 5 * time.Second,
	}
}

func seg(n int, start time.Duration) remux.Segment {
	return remux.Segment{
		N: n, Start: start, End: start + 5*time.Second,
		Video: remux.Fragment{Data: []byte{byte(n), 'v'}, T: int64(n), D: 1},
		Audio: remux.Fragment{Data: []byte{byte(n), 'a'}, T: int64(n), D: 1},
	}
}

func TestSegmentsComeBackByNumberWhereverTheyLie(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	e, err := c.Open(key("/m/a.mkv"))
	require.NoError(t, err)
	info := remux.StreamInfo{VideoCodec: "hev1", Width: 640, Height: 360, Channels: 2, Duration: time.Minute}
	require.NoError(t, e.SetInit([]byte("vinit"), []byte("ainit"), [2]int32{12288, 96000}, info))
	require.NoError(t, e.Put(seg(5, 20*time.Second))) // a seek run first
	require.NoError(t, e.Put(seg(1, 0)))
	require.NoError(t, e.Put(seg(1, 0)), "a segment already held is not appended twice")

	e2, err := c.Open(key("/m/a.mkv")) // a later job reads the index from disk
	require.NoError(t, err)
	v, a, ts, gotInfo, ok := e2.Init()
	require.True(t, ok)
	assert.Equal(t, info, gotInfo, "a replay publishes its manifest from this")
	assert.Equal(t, "vinit", string(v))
	assert.Equal(t, "ainit", string(a))
	assert.Equal(t, [2]int32{12288, 96000}, ts)
	got, ok, err := e2.Segment(5)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, seg(5, 20*time.Second), got)
	_, ok, _ = e2.Segment(2)
	assert.False(t, ok)
	fi, err := os.Stat(filepath.Join(c.Dir, key("/m/a.mkv").String(), "video.mp4"))
	require.NoError(t, err)
	assert.Equal(t, int64(len("vinit")+2+2), fi.Size(), "init once, then each fragment once")
}

func TestADifferentInitResetsTheEntry(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	e, _ := c.Open(key("/m/a.mkv"))
	require.NoError(t, e.SetInit([]byte("old"), []byte("old"), [2]int32{1, 1}, remux.StreamInfo{}))
	require.NoError(t, e.Put(seg(1, 0)))
	require.NoError(t, e.SetInit([]byte("new"), []byte("new"), [2]int32{1, 1}, remux.StreamInfo{}))
	_, ok, _ := e.Segment(1)
	assert.False(t, ok, "segments muxed against another init are dropped")
	v, _, _, _, _ := e.Init()
	assert.Equal(t, "new", string(v))
}

func TestAChangedFileIsAnotherEntry(t *testing.T) {
	k := key("/m/a.mkv")
	k2 := k
	k2.ModTime = k.ModTime.Add(time.Second)
	assert.NotEqual(t, k.String(), k2.String())
}

func TestEvictionRemovesTheLeastRecentlyUsed(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &Cache{Dir: t.TempDir(), MaxBytes: 30, Now: func() time.Time { return now }}
	for i, name := range []string{"/m/old.mkv", "/m/new.mkv"} {
		now = now.Add(time.Duration(i) * time.Hour)
		e, err := c.Open(key(name))
		require.NoError(t, err)
		require.NoError(t, e.SetInit(make([]byte, 10), make([]byte, 10), [2]int32{1, 1}, remux.StreamInfo{}))
	}
	require.NoError(t, c.Evict())
	_, err := os.Stat(filepath.Join(c.Dir, key("/m/old.mkv").String()))
	assert.True(t, os.IsNotExist(err), "the older entry goes")
	_, err = os.Stat(filepath.Join(c.Dir, key("/m/new.mkv").String()))
	assert.NoError(t, err)
}
