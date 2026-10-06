// Package cache keeps the segments the remux worker produced, per file and
// audio choice, on the worker's own disk: video.mp4 and audio.mp4 are
// fragmented MP4s (the init, then fragments in the order produced) and
// index.json finds each segment by number.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mediactl/clusterplex/pkg/remux"
)

type Key struct {
	Input           string
	Size            int64
	ModTime         time.Time
	AudioStream     int
	AudioCopy       bool
	Channels        int
	SampleRate      int
	BitRate         int64
	SegmentDuration time.Duration
}

func (k Key) String() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%t|%d|%d|%d|%d", k.Input, k.Size, k.ModTime.UnixNano(),
		k.AudioStream, k.AudioCopy, k.Channels, k.SampleRate, k.BitRate, k.SegmentDuration)))
	return hex.EncodeToString(h[:16])
}

type span struct{ Off, Len int64 }

type part struct {
	span
	T, D int64
}

type record struct {
	Start, End   time.Duration
	Video, Audio part
}

type index struct {
	VideoInit, AudioInit span
	Timescales           [2]int32
	Info                 remux.StreamInfo
	HasInit              bool
	Segments             map[string]record
	Last                 int
	Used                 time.Time
}

type Cache struct {
	Dir      string
	MaxBytes int64
	Now      func() time.Time

	mu      sync.Mutex
	entries map[string]*shared
}

// ErrFull is a segment refused because its entry alone would pass the
// cache's limit; the job plays on, uncached from there.
var ErrFull = errors.New("cache: the entry would pass the cache's limit")

// shared is one key's index, held once per worker: two jobs on one key --
// two viewers, or a seek's job starting while the old one stops -- write
// through it, so neither erases the other's records.
type shared struct {
	mu     sync.Mutex
	idx    index
	loaded bool
	refs   int
}

type Entry struct {
	c    *Cache
	name string
	dir  string
	mu   *sync.Mutex
	idx  *index
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Open returns k's entry, creating its directory; two jobs for one key on
// one worker share its index and lock. Close releases it.
func (c *Cache) Open(k Key) (*Entry, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*shared{}
	}
	name := k.String()
	sh, ok := c.entries[name]
	if !ok {
		sh = &shared{}
		c.entries[name] = sh
	}
	sh.refs++
	c.mu.Unlock()
	e := &Entry{c: c, name: name, dir: filepath.Join(c.Dir, name), mu: &sh.mu, idx: &sh.idx}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		e.release()
		return nil, err
	}
	if !sh.loaded {
		if err := e.load(); err != nil {
			e.release()
			return nil, err
		}
		sh.loaded = true
	}
	e.idx.Used = c.now()
	return e, e.save()
}

// Close releases the entry; once no job holds it, eviction may remove it.
func (e *Entry) Close() { e.release() }

func (e *Entry) release() {
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	if sh, ok := e.c.entries[e.name]; ok && sh.refs > 0 {
		sh.refs--
	}
}

// open reports whether a job holds name's entry.
func (c *Cache) open(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sh, ok := c.entries[name]
	return ok && sh.refs > 0
}

func (e *Entry) load() error {
	b, err := os.ReadFile(filepath.Join(e.dir, "index.json"))
	if os.IsNotExist(err) {
		*e.idx = index{Segments: map[string]record{}}
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, e.idx); err != nil {
		return e.resetLocked()
	}
	if e.idx.Segments == nil {
		e.idx.Segments = map[string]record{}
	}
	return nil
}

// save writes the index by rename, after the bytes it points at are synced.
func (e *Entry) save() error {
	b, err := json.Marshal(e.idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(e.dir, "index.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(e.dir, "index.json"))
}

func (e *Entry) resetLocked() error {
	for _, f := range []string{"video.mp4", "audio.mp4", "index.json"} {
		if err := os.Remove(filepath.Join(e.dir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	*e.idx = index{Segments: map[string]record{}, Used: e.c.now()}
	return nil
}

func appendTo(path string, data []byte) (span, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return span{}, err
	}
	fi, err := f.Stat()
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return span{}, err
	}
	return span{Off: fi.Size(), Len: int64(len(data))}, nil
}

func readAt(path string, s span) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, s.Len)
	_, err = io.ReadFull(io.NewSectionReader(f, s.Off, s.Len), b)
	return b, err
}

func (e *Entry) Init() (video, audio []byte, timescales [2]int32, info remux.StreamInfo, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.idx.HasInit {
		return nil, nil, timescales, info, false
	}
	v, err1 := readAt(filepath.Join(e.dir, "video.mp4"), e.idx.VideoInit)
	a, err2 := readAt(filepath.Join(e.dir, "audio.mp4"), e.idx.AudioInit)
	if err1 != nil || err2 != nil {
		return nil, nil, timescales, info, false
	}
	return v, a, e.idx.Timescales, e.idx.Info, true
}

// SetInit records the inits; an entry holding different ones is reset, as
// its fragments were muxed against them.
func (e *Entry) SetInit(video, audio []byte, timescales [2]int32, info remux.StreamInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idx.HasInit {
		v, err1 := readAt(filepath.Join(e.dir, "video.mp4"), e.idx.VideoInit)
		a, err2 := readAt(filepath.Join(e.dir, "audio.mp4"), e.idx.AudioInit)
		if err1 == nil && err2 == nil && slices.Equal(v, video) && slices.Equal(a, audio) && e.idx.Timescales == timescales {
			e.idx.Info = info
			return e.save()
		}
		if err := e.resetLocked(); err != nil {
			return err
		}
	}
	vs, err := appendTo(filepath.Join(e.dir, "video.mp4"), video)
	if err != nil {
		return err
	}
	as, err := appendTo(filepath.Join(e.dir, "audio.mp4"), audio)
	if err != nil {
		return err
	}
	e.idx.VideoInit, e.idx.AudioInit, e.idx.Timescales, e.idx.Info, e.idx.HasInit = vs, as, timescales, info, true
	return e.save()
}

func (e *Entry) Segment(n int) (remux.Segment, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.idx.Segments[strconv.Itoa(n)]
	if !ok {
		return remux.Segment{}, false, nil
	}
	v, err := readAt(filepath.Join(e.dir, "video.mp4"), r.Video.span)
	if err != nil {
		return remux.Segment{}, false, err
	}
	a, err := readAt(filepath.Join(e.dir, "audio.mp4"), r.Audio.span)
	if err != nil {
		return remux.Segment{}, false, err
	}
	e.idx.Used = e.c.now()
	return remux.Segment{
		N: n, Start: r.Start, End: r.End,
		Video: remux.Fragment{Data: v, T: r.Video.T, D: r.Video.D},
		Audio: remux.Fragment{Data: a, T: r.Audio.T, D: r.Audio.D},
	}, true, nil
}

func (e *Entry) Put(s remux.Segment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.idx.HasInit {
		return fmt.Errorf("cache: segment %d before the init", s.N)
	}
	if _, ok := e.idx.Segments[strconv.Itoa(s.N)]; ok {
		return nil
	}
	if e.c.MaxBytes > 0 && e.size()+int64(len(s.Video.Data)+len(s.Audio.Data)) > e.c.MaxBytes {
		return ErrFull
	}
	vs, err := appendTo(filepath.Join(e.dir, "video.mp4"), s.Video.Data)
	if err != nil {
		return err
	}
	as, err := appendTo(filepath.Join(e.dir, "audio.mp4"), s.Audio.Data)
	if err != nil {
		return err
	}
	e.idx.Segments[strconv.Itoa(s.N)] = record{
		Start: s.Start, End: s.End,
		Video: part{span: vs, T: s.Video.T, D: s.Video.D}, Audio: part{span: as, T: s.Audio.T, D: s.Audio.D},
	}
	e.idx.Used = e.c.now()
	if err := e.save(); err != nil {
		return err
	}
	// Evicted as the run writes, not after it: one 4K remux, or two fresh
	// plays on a nearly full cache, would pass the volume's limit mid-run.
	return e.c.Evict()
}

// size is the bytes the entry's files hold.
func (e *Entry) size() int64 {
	var n int64
	for _, f := range []string{"video.mp4", "audio.mp4"} {
		if fi, err := os.Stat(filepath.Join(e.dir, f)); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Last is the number of the file's final segment once a run reached its
// end, 0 before.
func (e *Entry) Last() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.idx.Last
}

func (e *Entry) SetLast(n int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idx.Last = n
	return e.save()
}

// Evict removes the least recently used entries until the cache holds at
// most MaxBytes; 0 keeps everything.
func (c *Cache) Evict() error {
	if c.MaxBytes <= 0 {
		return nil
	}
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return err
	}
	type entry struct {
		name string
		size int64
		used time.Time
	}
	var all []entry
	var total int64
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		var size int64
		for _, f := range []string{"video.mp4", "audio.mp4"} {
			if fi, err := os.Stat(filepath.Join(c.Dir, d.Name(), f)); err == nil {
				size += fi.Size()
			}
		}
		var idx index
		if b, err := os.ReadFile(filepath.Join(c.Dir, d.Name(), "index.json")); err == nil {
			_ = json.Unmarshal(b, &idx)
		}
		all = append(all, entry{d.Name(), size, idx.Used})
		total += size
	}
	slices.SortFunc(all, func(a, b entry) int { return a.used.Compare(b.used) })
	for _, e := range all {
		if total <= c.MaxBytes {
			break
		}
		if c.open(e.name) { // a job is writing or reading it
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.Dir, e.name)); err != nil {
			return err
		}
		total -= e.size
	}
	return nil
}
