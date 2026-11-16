# One worker for Plex's conversions

Status: proposed, 2026-11-16. Not built. Awaits the owner's approval.

## Why

Plex's conversions run on three Deployments from two images: `plex-remux`
(`remux-worker`, the browser's copied-video streams), `plex-video-transcode`
(`video-worker --encoder h264_qsv`, the Intel GPU) and
`plex-video-transcode-cpu` (`video-worker --encoder libx264`). The owner,
2026-11-16:

- "investigate combining the remux and the transcode containers for Plex.
  we shouldn't need a separate transcoder - also, the final single workload
  should be capable of running on cpu"
- "When it runs on cpu, it should prefer the intel nuc 15 pro node"
- "I would like it to run on a CPU node - falling back from a GPU node like
  CPU video currently."
- "We should also allow building this multi-arch since ... we're making the
  image amd64 only due to the intel libraries - which we could exclude in
  the arm64 version"

## What is true today

- Both images carry the same FFmpeg 9 build and the same ffgo shim
  (`images/Dockerfile.remux`, `images/Dockerfile.video`). The video image
  adds Intel's runtime (iHD driver, libmfx, libva) and Noto fonts, and its
  ffmpeg stage refuses any `TARGETARCH` but amd64 (`Dockerfile.video:55`);
  the staging stage names `x86_64-linux-gnu` paths.
- The GPU and CPU video pools are one binary; the encoder is a flag of the
  pod, and `h264_qsv` with no GPU exits at its start-up measurement
  (`cmd/video-worker/main.go`, `measureDevice`).
- Managers find workers by label, and try the pools in an order: the GPU
  pool alone for a whole-file or tone-mapped job, else the GPU first and
  the CPU pool as its second chance, or the CPU first while clustarr holds
  work for the GPU (`pkg/video/relay/pools.go`, `order`).
- The Lease `cluster-plex-video-transcodes` counts the conversions on the
  GPU pool, known from which pool a worker was found in; clustarr sizes its
  Intel budget from it.
- Remux has no job limit and an HPA on CPU, with a 25Gi disk cache; video
  counts slots and keeps nothing on disk.

## Goals

- One image and one worker binary that serves both the remux and the video
  services.
- It runs on a node without a GPU, encoding with libx264, and uses the
  Intel GPU where it has one.
- GPU first, CPU as the fallback, as the two video pools are tried today
  (and CPU first while clustarr holds GPU work).
- On CPU it prefers the NUC 15 Pro (`talos-88-ae-dd-75-3d-a0`) and may run
  on any other node that can take it.
- The image builds for amd64 and arm64; the arm64 image carries no Intel
  runtime and is a CPU worker.

## Non-goals

- Changing which conversions cluster-plex allows, or Plex's own fallback
  for a remux that fails.
- NVIDIA.

## Design

**The binary.** `cmd/worker` serves `remux.Remux` and `video.Video` on
their ports from one process, with the two packages as they are.
`--encoder auto` (the default) measures the render node at start: a GPU
that passes the measurement means `h264_qsv`; none, or a build without the
Intel runtime, means `libx264`, said in the log and in a `encoder` label
the pod's metrics carry. `h264_qsv` and `libx264` stay as explicit values,
and `h264_qsv` still exits without a GPU.

**The image.** `images/Dockerfile.worker`: the FFmpeg stage takes the
amd64 or arm64 BtbN build by `TARGETARCH` (the remux image already pins
both); the Intel runtime is its own stage, copied in only on amd64; the
fonts on both. The self-check asks for `libx264` and `aac` everywhere and
`h264_qsv` on amd64 only. `Dockerfile.remux` and `Dockerfile.video` go.

**The workloads.** One template, `templates/worker.yaml`, rendered once per
entry of `workers` in values, each with its own name, resources, slots and
placement. Two entries by default:

- `gpu`: requests `gpu.intel.com/i915`, required on an Intel GPU node,
  `--encoder h264_qsv`.
- `cpu`: no device request, `--encoder libx264`, required
  `kubernetes.io/arch` in amd64 and arm64 as the image is, and a
  **preferred** node affinity whose hostname is a value
  (`talos-88-ae-dd-75-3d-a0` on talos). On talos the Pis are excluded by a
  required rule in the values, not the chart: the 2026-10-26 design
  measured a 4-core Pi that is an etcd member as "terrible" for an encode.

One Deployment cannot be both: a pod that requests the device schedules
only where the device is, and reaching `/dev/dri` without the request
means a privileged pod, which the design refuses. So "falls back from a
GPU node" is the managers' order over the two, not the scheduler's.

**Routing.** The managers keep `order`: the `gpu` workers first and the
`cpu` workers second for a video job, the other way round while clustarr
holds GPU work, the GPU alone for a whole-file or tone-mapped job. A remux
job goes to the `cpu` workers first and the `gpu` workers second, so
copies never take a GPU pod's slots while a CPU pod has room.

**The Lease.** A worker's answer names the encoder it ran, and the pod
carries `clusterplex.io/encoder: h264_qsv|libx264`. The Lease counts
conversions on pods labelled `h264_qsv`, where it counts the GPU pool's
today. A test holds the count to the label.

**Capacity.** One slot count per pod covers both kinds: a video
conversion takes a slot and a remux a quarter of one (a copy measured a
fraction of an encode's cores; the number is a value). The HPA stays, on
the `cpu` entry only. The remux disk cache becomes an emptyDir of the
worker, sized by a value, and a video job still writes nothing to disk.

**Health.** A wedged conversion fails that job, not the pod's liveness,
while a remux stream is open on it; the pod is restarted once it is idle.

## Migration

Cold turkey, as the project does: the three Deployments and two images are
replaced in one chart version. On talos the chart change restarts every
Plex worker once; conversions in flight end.

## Tests

- The image self-check per architecture (CI builds both).
- `test/manifests`: the two default workers, their placement, the device
  request on `gpu` alone, the preferred hostname on `cpu`.
- The relay's order for video and remux jobs; the Lease count by label.
- The existing remux and video package tests, unchanged.

## Risks

- **The Lease miscounting** makes clustarr yield its GPU for nothing.
- **libx264 on a Pi**: the image allows it, the talos values must not.
- **Remux beside encodes**: the slot weight is a guess until measured.
- **A larger image** for what was the remux pool.

## Open, to verify before building

- Whether the published remux tag is in fact multi-arch (talos pins the
  remux pool to amd64 with the note "The image is amd64 only").
- libx264 at real time on arm64 hardware other than the Pis: none exists
  on talos, so the arm64 image is built and self-checked, not load-proven.
- The size of the combined image.

## Owner's calls

1. Two Deployments of one image (this design), or one CPU-only
   Deployment with no GPU use at all.
2. Remux first on the CPU workers, as above.
3. Whether arm64 workers should ever be scheduled on talos.
