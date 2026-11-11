package relay

import (
	"context"

	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/video"
)

// The two pools a conversion may run on: the GPU pool (h264_qsv on the
// Intel GPU's video engines) and the CPU pool (libx264 on the NUC's cores,
// docs/superpowers/specs/2026-11-10-cpu-video-route-design.md), each the
// other's second chance when full.
const (
	PoolGPU = "gpu"
	PoolCPU = "cpu"
)

// pool is one pool to try: its name and its ready pods.
type pool struct {
	name    string
	workers remoteexec.WorkerLister
}

// cpuEligible is whether the CPU pool may be asked for job at all: a
// streamed conversion (HLS, DASH) without a tone map. A whole-file job (a
// download, an Optimize, a DVR conversion) has no viewer waiting and would
// hold the pool's cores for its length, and an HDR job's tone map is the
// GPU's; the pod judges the source itself (codec, size, HDR) once it opens
// it, since the manager never probes the file.
func cpuEligible(job video.Job) bool {
	return job.Output != video.OutputFile && !job.Video.ToneMap
}

// order is the pools to try for job, in order: the GPU pool alone without
// a CPU pool or for a job the CPU pool may not take; else the CPU pool
// first while CPUFirst says so (clustarr holds work for the GPU, or the
// route is always CPU), the GPU pool first otherwise, and the other pool
// after it as its second chance.
func (r *Relay) order(ctx context.Context, job video.Job) []pool {
	gpu := pool{name: PoolGPU, workers: r.Workers}
	if r.CPU == nil || !cpuEligible(job) {
		return []pool{gpu}
	}
	cpu := pool{name: PoolCPU, workers: r.CPU}
	if r.CPUFirst != nil && r.CPUFirst(ctx) {
		return []pool{cpu, gpu}
	}
	return []pool{gpu, cpu}
}
