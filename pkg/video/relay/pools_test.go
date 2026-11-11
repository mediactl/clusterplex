package relay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/video"
)

type listers []remoteexec.Worker

func (l listers) ListReady(context.Context) ([]remoteexec.Worker, error) { return l, nil }

func TestThePoolsAreOrderedByTheRuleAndTheJob(t *testing.T) {
	gpu, cpu := listers{{Name: "gpu-0"}}, listers{{Name: "cpu-0"}}
	hls := video.Job{Output: video.OutputHLS}
	names := func(ps []pool) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.name)
		}
		return out
	}
	r := &Relay{Workers: gpu}
	assert.Equal(t, []string{PoolGPU}, names(r.order(t.Context(), hls)), "no CPU pool")
	r = &Relay{Workers: gpu, CPU: cpu, CPUFirst: func(context.Context) bool { return true }}
	assert.Equal(t, []string{PoolCPU, PoolGPU}, names(r.order(t.Context(), hls)))
	r.CPUFirst = func(context.Context) bool { return false }
	assert.Equal(t, []string{PoolGPU, PoolCPU}, names(r.order(t.Context(), hls)), "the CPU pool is the second chance")
	r.CPUFirst = func(context.Context) bool { return true }
	assert.Equal(t, []string{PoolGPU}, names(r.order(t.Context(), video.Job{Output: video.OutputFile})), "a whole-file job never asks the CPU pool")
	assert.Equal(t, []string{PoolGPU}, names(r.order(t.Context(), video.Job{Output: video.OutputHLS, Video: video.VideoOut{ToneMap: true}})), "nor an HDR job")
	r = &Relay{Workers: gpu, CPU: cpu}
	assert.Equal(t, []string{PoolGPU, PoolCPU}, names(r.order(t.Context(), hls)), "nil CPUFirst is never first")
	assert.Equal(t, "gpu-0", r.order(t.Context(), hls)[0].workers.(listers)[0].Name, "each pool carries its own lister")
	assert.Equal(t, "cpu-0", r.order(t.Context(), hls)[1].workers.(listers)[0].Name)
}
