package gpu_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/obinnaokechukwu/ffgo"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video/gpu"
)

// needFFmpeg skips without FFmpeg 9 and the shim, unless make test-native
// asked for the ffgo suites to run for real.
func needFFmpeg(t *testing.T) {
	t.Helper()
	if err := ffgo.Init(); err != nil || ffgo.ShimAPIVersion() < 2 {
		if os.Getenv("CLUSTERPLEX_REQUIRE_FFGO") == "1" {
			t.Fatalf("FFmpeg 9 with shim API 2: %v (API %d)", err, ffgo.ShimAPIVersion())
		}
		t.Skipf("FFmpeg 9 with shim API 2 not available: %v", err)
	}
}

func TestAClipDecodesBack(t *testing.T) {
	needFFmpeg(t)
	for _, c := range []struct {
		codec string
		bits  int
	}{{"h264", 8}, {"hevc", 8}, {"hevc", 10}, {"mpeg2video", 8}, {"vp9", 8}, {"vp9", 10}} {
		b, err := gpu.Clip(context.Background(), c.codec, c.bits, time.Second, 320, 180)
		require.NoError(t, err, "%s:%d", c.codec, c.bits)
		d, err := ffgo.NewDecoderFromReader(bytes.NewReader(b), "matroska")
		require.NoError(t, err)
		v := d.VideoStream()
		require.NotNil(t, v)
		require.Equal(t, c.codec, v.CodecName, "%s:%d", c.codec, c.bits)
		n := 0
		for {
			p, err := d.ReadPacket()
			require.NoError(t, err)
			if p == nil {
				break
			}
			if p.StreamIndex() == v.Index {
				n++
			}
		}
		require.NoError(t, d.Close())
		require.Equal(t, 30, n, "%s:%d: a second at 30 fps", c.codec, c.bits)
	}
}

func TestMeasureWithoutADeviceFails(t *testing.T) {
	needFFmpeg(t)
	caps, err := gpu.Measure(context.Background(), "/dev/dri/none")
	require.Error(t, err)
	require.Equal(t, gpu.Caps{}, caps)
}

// TestTheNUCsDeviceMeasures runs where an Intel render node is expected
// (CLUSTERPLEX_EXPECT_INTEL=1, the video-test image on the NUC), and fails
// there rather than skip.
func TestTheNUCsDeviceMeasures(t *testing.T) {
	if os.Getenv("CLUSTERPLEX_EXPECT_INTEL") != "1" {
		t.Skip("no Intel render node expected")
	}
	needFFmpeg(t)
	_, err := os.Stat("/dev/dri/renderD128")
	require.NoError(t, err, "the Intel render node")
	caps, err := gpu.Measure(context.Background(), "/dev/dri/renderD128")
	require.NoError(t, err)
	require.True(t, caps.Encode, "h264_qsv")
	require.True(t, caps.Scale, "vpp_qsv")
	require.True(t, caps.DecodesOnGPU("h264", 8))
	t.Logf("measured %+v", caps)
}

func TestAPathThatIsNoRenderNodeIsRefusedBeforeFFmpeg(t *testing.T) {
	require.Error(t, gpu.IsRenderNode("/dev/dri/none"))
	require.Error(t, gpu.IsRenderNode(t.TempDir()), "a directory")
	require.Error(t, gpu.IsRenderNode("/etc/hostname"), "a regular file")
	require.NoError(t, gpu.IsRenderNode("/dev/null"), "a character device")
}
