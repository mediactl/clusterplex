package remoteexec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRewriteArgs(t *testing.T) {
	const pms = "plex-0.plex-workers.media.svc.cluster.local:32400"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "progress url on loopback",
			args: []string{"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/x/progress"},
			want: []string{"-progressurl", "http://" + pms + "/video/:/transcode/session/x/progress"},
		},
		{
			name: "localhost spelling",
			args: []string{"http://localhost:32400/a"},
			want: []string{"http://" + pms + "/a"},
		},
		{
			name: "unrelated args untouched",
			args: []string{"-i", "/media/x.mkv", "-y"},
			want: []string{"-i", "/media/x.mkv", "-y"},
		},
		{
			name: "other port untouched",
			args: []string{"http://127.0.0.1:32401/"},
			want: []string{"http://127.0.0.1:32401/"},
		},
		{name: "empty", args: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RewriteArgs(tt.args, pms))
		})
	}
}

func TestRewriteArgsDoesNotMutateInput(t *testing.T) {
	in := []string{"http://127.0.0.1:32400/"}
	_ = RewriteArgs(in, "pms:32400")
	assert.Equal(t, "http://127.0.0.1:32400/", in[0])
}
