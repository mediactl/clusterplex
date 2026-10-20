package codecs_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexcodecs "github.com/mediactl/clusterplex/pkg/plex/codecs"
)

// fakePMS answers like PMS: a decision per item, and a transcode whose
// first segment it serves (never, for a missing decoder).
type fakePMS struct {
	mu         sync.Mutex
	version    string
	versionErr error
	// refused items get a decision that converts the video.
	refused map[int64]bool
	// noSegment items never write one.
	noSegment map[int64]bool
	// blockOnRead makes the segment read wait for its context.
	blockOnRead bool
	readStarted chan struct{}

	decided  []int64
	started  map[string]int64
	stopped  []string
	clients  []plexapi.TranscodeClient
	sessions []string
}

func (f *fakePMS) ServerVersion(context.Context) (string, error) { return f.version, f.versionErr }

func (f *fakePMS) DecideTranscode(_ context.Context, t plexapi.AudioTranscode) (plexapi.TranscodeDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decided = append(f.decided, t.Item)
	f.clients = append(f.clients, t.Client)
	if f.refused[t.Item] {
		return plexapi.TranscodeDecision{Code: 2000, Text: "maximum simultaneous video transcodes"}, nil
	}
	return plexapi.TranscodeDecision{Code: 1001, Streams: []plexapi.StreamDecision{
		{StreamType: 1, Codec: "hevc", Decision: "copy"}, {StreamType: 2, Codec: "aac", Decision: "transcode"},
	}}, nil
}

func (f *fakePMS) StartTranscode(_ context.Context, t plexapi.AudioTranscode) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started == nil {
		f.started = map[string]int64{}
	}
	f.started[t.Session] = t.Item
	f.sessions = append(f.sessions, t.Session)
	return "/video/:/transcode/universal/session/" + t.Session + "/base/index.m3u8", nil
}

func (f *fakePMS) FirstSegment(ctx context.Context, t plexapi.AudioTranscode, _ string) (int64, error) {
	if f.blockOnRead {
		if f.readStarted != nil {
			close(f.readStarted)
			f.readStarted = nil
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noSegment[f.started[t.Session]] {
		return 0, errors.New("GET /video/:/transcode/universal/session/s/base/00000.ts: 500 Internal Server Error")
	}
	return 1332920, nil
}

func (f *fakePMS) StopTranscode(_ context.Context, t plexapi.AudioTranscode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, t.Session)
	return nil
}

// library maps a codec to the items whose first audio it is.
func library(items map[string][]int64) plexcodecs.Items {
	return func(_ context.Context, codec string, n int) ([]int64, error) {
		got := items[codec]
		return got[:min(n, len(got))], nil
	}
}

func prefetcher(t *testing.T, pms *fakePMS, items map[string][]int64, codecs ...string) *plexcodecs.Prefetcher {
	t.Helper()
	return &plexcodecs.Prefetcher{
		PMS: pms, Items: library(items), Dir: t.TempDir(), Platform: "linux-aarch64", Codecs: codecs,
		FirstSegment: time.Second,
	}
}

func readRecord(t *testing.T, p *plexcodecs.Prefetcher) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.Dir, plexcodecs.RecordName))
	require.NoError(t, err)
	var rec map[string]map[string]any
	require.NoError(t, json.Unmarshal(b, &rec))
	return rec
}

func TestEachDecoderIsFetchedOncePerPMSVersion(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c"}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}, "ac3": {9}}, "aac", "ac3")

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac", "ac3"}, res.Fetched)
	assert.True(t, res.Done())
	assert.Equal(t, []int64{72, 9}, pms.decided)
	assert.ElementsMatch(t, pms.sessions, pms.stopped, "every transcode it started is stopped")
	assert.Equal(t, map[string]map[string]any{
		"linux-aarch64": {"version": "1.43.4.10903-e5521bd8c", "codecs": []any{"aac", "ac3"}},
	}, readRecord(t, p))

	res, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Empty(t, res.Fetched)
	assert.Equal(t, []string{"aac", "ac3"}, res.Recorded)
	assert.Len(t, pms.decided, 2, "nothing asked of PMS the second time")
}

func TestANewPMSVersionFetchesEveryDecoderAgain(t *testing.T) {
	// It may keep its codecs in a new folder, whose name only PMS knows.
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c"}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")
	_, err := p.Run(t.Context())
	require.NoError(t, err)

	pms.version = "1.43.5.11000-0123abcd"
	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Fetched)
	assert.Equal(t, "1.43.5.11000-0123abcd", readRecord(t, p)["linux-aarch64"]["version"])
}

func TestEachPlatformKeepsItsOwnRecord(t *testing.T) {
	// One config volume, pods of two architectures: an arm64 PMS's
	// decoders are no use to an x86_64 one.
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c"}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")
	_, err := p.Run(t.Context())
	require.NoError(t, err)

	p.Platform = "linux-x86_64"
	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Fetched)
	rec := readRecord(t, p)
	assert.Equal(t, []any{"aac"}, rec["linux-aarch64"]["codecs"], "the other platform's record is kept")
	assert.Equal(t, []any{"aac"}, rec["linux-x86_64"]["codecs"])
}

func TestACodecNoLibraryItemCarriesIsLeftToPlexAndNotRetried(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c"}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac", "mp2")

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Fetched)
	assert.Equal(t, []string{"mp2"}, res.NoItem)
	assert.True(t, res.Done(), "nothing a retry would change")
	assert.Equal(t, []any{"aac"}, readRecord(t, p)["linux-aarch64"]["codecs"], "mp2 is not recorded as fetched")
}

func TestAnItemPMSWouldConvertTheVideoOfIsPassedOverForTheNext(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c", refused: map[int64]bool{5: true}}
	p := prefetcher(t, pms, map[string][]int64{"aac": {5, 6}}, "aac")

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Fetched)
	assert.Equal(t, []int64{5, 6}, pms.decided)
	assert.NotContains(t, pms.started, int64(5), "a refused item is never started")
}

func TestATranscodeThatWritesNoSegmentIsAFailureToRetry(t *testing.T) {
	// What a decoder PMS could not download looks like: the transcoder
	// starts and exits without writing anything.
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c", noSegment: map[int64]bool{5: true, 6: true}}
	p := prefetcher(t, pms, map[string][]int64{"aac": {5, 6}, "ac3": {9}}, "aac", "ac3")

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Failed)
	assert.Equal(t, []string{"ac3"}, res.Fetched, "one codec's failure does not hold up the next")
	assert.False(t, res.Done())
	assert.ElementsMatch(t, pms.sessions, pms.stopped, "the failed transcodes are stopped too")
	assert.Equal(t, []any{"ac3"}, readRecord(t, p)["linux-aarch64"]["codecs"])

	pms.noSegment = nil
	res, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Fetched, "retried on the next run")
}

func TestEveryRefusalIsAFailureNotAFetch(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c", refused: map[int64]bool{5: true, 6: true}}
	p := prefetcher(t, pms, map[string][]int64{"aac": {5, 6}}, "aac")

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Failed)
	assert.NoFileExists(t, filepath.Join(p.Dir, plexcodecs.RecordName))
}

func TestTheWaitForTheFirstSegmentIsBounded(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c", blockOnRead: true}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")
	p.FirstSegment = 20 * time.Millisecond

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Failed)
	assert.Equal(t, pms.sessions, pms.stopped)
}

func TestATranscodeIsStoppedWhenTheRunIsCancelled(t *testing.T) {
	// A lease lost mid-wait must not leave the transcoder running.
	started := make(chan struct{})
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c", blockOnRead: true, readStarted: started}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")
	p.FirstSegment = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-started; cancel() }()

	_, err := p.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, pms.sessions, 1)
	assert.Equal(t, pms.sessions, pms.stopped)
	assert.NoFileExists(t, filepath.Join(p.Dir, plexcodecs.RecordName))
}

func TestAPMSThatDoesNotAnswerFetchesNothing(t *testing.T) {
	pms := &fakePMS{versionErr: errors.New("connection refused")}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")

	_, err := p.Run(t.Context())
	require.Error(t, err)
	assert.Empty(t, pms.decided)
}

func TestAnUnreadableRecordCostsOnlyAFetch(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c"}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")
	require.NoError(t, os.WriteFile(filepath.Join(p.Dir, plexcodecs.RecordName), []byte("{not json"), 0o644))

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"aac"}, res.Fetched)
	assert.Equal(t, []any{"aac"}, readRecord(t, p)["linux-aarch64"]["codecs"], "rewritten whole")
}

func TestTheTranscodesAreAskedForAsAnAndroidTVThatPlaysHEVC(t *testing.T) {
	pms := &fakePMS{version: "1.43.4.10903-e5521bd8c"}
	p := prefetcher(t, pms, map[string][]int64{"aac": {72}}, "aac")
	_, err := p.Run(t.Context())
	require.NoError(t, err)

	require.Len(t, pms.clients, 1)
	assert.Equal(t, "Plex for Android (TV)", pms.clients[0].Product)
	assert.Contains(t, pms.clients[0].ProfileExtra, "videoCodec=hevc")
}

func TestPlatformIsTheSuffixOfPlexsCodecsFolder(t *testing.T) {
	assert.Equal(t, "linux-aarch64", plexcodecs.Platform("arm64"))
	assert.Equal(t, "linux-x86_64", plexcodecs.Platform("amd64"))
}
