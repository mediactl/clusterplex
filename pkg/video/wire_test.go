package video_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/mediactl/clusterplex/pkg/video"
)

func TestAJobCrossesTheWireWithoutSecrets(t *testing.T) {
	for _, c := range fixtures(t) {
		t.Run(c.Shape, func(t *testing.T) {
			j, err := video.Parse(c.Args, c.Env)
			require.NoError(t, err)
			j.Token = "a-session-token"
			if len(j.Inputs) > 0 && j.Inputs[0].Streamed {
				require.NotEmpty(t, j.Inputs[0].Source)
			}

			want := j
			want.Token, want.ProgressURL, want.SegmentListURL, want.ManifestURL, want.OutputPath = "", "", "", "", ""
			want.Inputs = append([]video.Input(nil), j.Inputs...)
			for i := range want.Inputs {
				want.Inputs[i].Source = ""
			}
			require.Equal(t, want, video.JobFromProto(j.Proto()))

			b, err := proto.Marshal(j.Proto())
			require.NoError(t, err)
			for _, secret := range []string{j.Token, j.ProgressURL, j.SegmentListURL, j.ManifestURL, j.OutputPath} {
				if secret != "" {
					require.False(t, bytes.Contains(b, []byte(secret)), "the wire carries %q", secret)
				}
			}
			for _, in := range j.Inputs {
				if in.Source != "" {
					require.False(t, bytes.Contains(b, []byte(in.Source)), "the wire carries a streamed input's source")
				}
			}
		})
	}
}
