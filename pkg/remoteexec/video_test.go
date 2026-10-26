package remoteexec

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/mediactl/clusterplex/proto"
)

// conversion is the captured library HLS job (pkg/video's fixture), with no
// EAE_ROOT, so tryRemote would take it if the guard let it through.
func conversion(t *testing.T) *pb.ExecRequest {
	t.Helper()
	f, err := os.Open("../video/testdata/plex-video-jobs.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var c struct {
			Shape string   `json:"shape"`
			Args  []string `json:"args"`
		}
		require.NoError(t, json.Unmarshal(s.Bytes(), &c))
		if c.Shape == "library-hls" {
			return &pb.ExecRequest{TargetBinary: Transcoder, Args: c.Args, Env: map[string]string{"X_PLEX_TOKEN": "REDACTED"}}
		}
	}
	t.Fatal("no library-hls fixture")
	return nil
}

// countingWorkers counts how often the dispatcher looked for a worker pod.
type countingWorkers struct{ calls int }

func (c *countingWorkers) ListReady(context.Context) ([]Worker, error) {
	c.calls++
	return []Worker{{Name: "plex-1", Addr: "10.0.0.2:50051"}}, nil
}

type fakeVideo struct{ calls int }

func (f *fakeVideo) Execute(_ context.Context, _ *pb.ExecRequest, sink Sink) error {
	f.calls++
	return Finish(sink, "video", nil)
}

type fakeRemux struct{ calls int }

func (f *fakeRemux) Execute(_ context.Context, _ *pb.ExecRequest, sink Sink) (bool, error) {
	f.calls++
	return true, Finish(sink, "remux", nil)
}

func localTranscoder(t *testing.T) *Executor {
	bin := t.TempDir()
	writeScript(t, bin, "Plex Transcoder.real", `echo ran-locally`)
	return &Executor{BinDir: bin}
}

func TestAVideoConversionNeverRunsLocally(t *testing.T) {
	w := &countingWorkers{}
	var refused []string
	d := &Dispatcher{
		Local: localTranscoder(t), Workers: w, Dial: noDial(t), PMSAddr: "pms:32400",
		OnRefused: func(reason string) { refused = append(refused, reason) },
	}
	sink := &collectSink{}
	err := d.Execute(context.Background(), conversion(t), sink)

	require.Error(t, err)
	assert.Equal(t, "video: no-pool: no video transcode pool is configured\n", sink.stderr())
	require.NotNil(t, sink.final())
	assert.EqualValues(t, 1, sink.final().ExitCode)
	assert.NotContains(t, sink.stdout(), "ran-locally", "Plex's own transcoder ran a video conversion")
	assert.Zero(t, w.calls, "a video conversion was offered to another Plex pod")
	assert.Equal(t, []string{"no-pool"}, refused)
}

func TestAVideoConversionTakesTheVideoRoute(t *testing.T) {
	v := &fakeVideo{}
	var routes [][2]string
	d := &Dispatcher{
		Local: localTranscoder(t), Video: v, Dial: noDial(t),
		OnRoute: func(target, mode string) { routes = append(routes, [2]string{target, mode}) },
	}
	sink := &collectSink{}
	require.NoError(t, d.Execute(context.Background(), conversion(t), sink))
	assert.Equal(t, 1, v.calls)
	assert.NotContains(t, sink.stdout(), "ran-locally")
	assert.Equal(t, [][2]string{{Transcoder, "video"}}, routes)
}

func TestABrowserRemuxStillTakesTheRemuxPool(t *testing.T) {
	v, r := &fakeVideo{}, &fakeRemux{}
	d := &Dispatcher{Local: localTranscoder(t), Video: v, Remux: r, Dial: noDial(t)}
	req := &pb.ExecRequest{TargetBinary: Transcoder, Args: []string{
		"-codec:0", "hevc", "-i", "/library/x.mkv", "-map", "0:0", "-codec:0", "copy",
		"-map", "0:1", "-codec:1", "aac", "-f", "dash", "dash",
	}}
	require.NoError(t, d.Execute(context.Background(), req, &collectSink{}))
	assert.Equal(t, 1, r.calls)
	assert.Zero(t, v.calls)
}

func TestAnAudioOnlyConversionStillRunsWhereItDid(t *testing.T) {
	v := &fakeVideo{}
	d := &Dispatcher{Local: localTranscoder(t), Video: v, Dial: noDial(t)}
	req := &pb.ExecRequest{TargetBinary: Transcoder, Env: map[string]string{"EAE_ROOT": "/tmp/eae"}, Args: []string{
		"-i", "/media/x.mkv", "-map", "0:0", "-codec:0", "copy",
		"-map", "0:1", "-codec:1", "aac", "-b:1", "256k", "-f", "ssegment", "media-%05d.ts",
	}}
	sink := &collectSink{}
	require.NoError(t, d.Execute(context.Background(), req, sink))
	assert.Zero(t, v.calls)
	assert.Contains(t, sink.stdout(), "ran-locally")
}
