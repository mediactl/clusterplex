package remux

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAJobCrossesTheWireWithoutItsSecrets(t *testing.T) {
	args, env := loggedJob(arcane)
	j, err := Parse(args, env)
	assert.NoError(t, err)
	back := JobFromProto(j.Proto())
	assert.Equal(t, 495*time.Second, back.Start)
	assert.Equal(t, j.AudioSampleRate, back.AudioSampleRate)
	want := j
	want.Token, want.ProgressURL, want.ManifestURL = "", "", ""
	assert.Equal(t, want, back)
}
