# ADR-0010: Video conversions run on the GPU pool

**Status:** Accepted
**Date:** 2026-10-26
**Deciders:** cluster-plex maintainers, with the owner

## Context

ADR-0007's amendment set `TranscoderCanOnlyRemuxVideo` to `0` so that Plex
for iOS could play Live TV, which it asks for as a video transcode (decision
4010 until then). Plex then converted video on the pod serving the stream,
and Plex's pods run on the cluster's Raspberry Pi 5 control plane, whose
software encodes cannot keep up with a 1080p channel.

The Intel NUC (`gpu.intel.com/i915`, NFD's
`intel.feature.node.kubernetes.io/gpu`) already encodes clustarr's library
on QSV. The remux pool (ADR-0007) already shows how to send a shimmed
transcoder job to another pod and bring its segments back into Plex's
session directory.

## Decision

Every video conversion Plex asks for runs on a GPU pool on the Intel node,
in process with ffgo
(`docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md`):

- **Which jobs.** The manager's dispatcher sends every shimmed
  `Plex Transcoder` job that converts video (`pkg/video.IsConversion`) to the
  pool. That covers Live TV and library playback from any client,
  downloads, Optimize, and the DVR's convert-while-recording. The Video route
  comes after the remux route and before the remote and local ones.
- **No fallback** (owner). A conversion the pool cannot take is refused and
  never runs on Plex's node, so the player shows a playback error. Each
  refusal is counted by reason: `no-pool`, `full`, `shape`, `worker-error`
  or `cancelled`.
- **The worker.** `cmd/video-worker` decodes on the GPU (VA-API, for the
  formats its start-up measure proved) or else on the CPU. It scales and
  converts with `vpp_qsv` and encodes with `h264_qsv`. The job is parsed
  from Plex's own arguments (`pkg/video.Parse`), without Plex's token or
  URLs.
- **Nothing on the worker's disk** (owner). It muxes every output in memory
  and streams it back over gRPC (`Video.Convert`). HLS and DASH outputs go
  back segment by segment; a whole file goes back as offset writes. The relay
  on Plex's pod (`pkg/video/relay`) writes them into Plex's session
  directory, posts each segment list as Plex's transcoder does, and relays
  progress with the session's token.
- **Inputs.** Anything outside `/media`, such as a Live TV stream or a
  sidecar subtitle, is streamed to the worker by the manager.
- **Capacity.** A pod runs at most 3 conversions, as many as the IPTV
  account has tuners. A whole-file job never takes a pod's last free slot.
- **The Lease.** The managers count every live conversion in the Lease
  `cluster-plex-video-transcodes` (`pkg/video/lease`). clustarr's transcode
  admission reads it (`--video-transcode-lease`) and admits no new file to
  its Intel class while the count is above zero.

## Consequences

- Plex for iOS plays Live TV, encoded on the GPU rather than on the Pi.
- With the pool down, every video conversion is refused. Remuxes,
  copied-video DASH streams and audio-only conversions are unaffected.
- clustarr's Intel admission pauses while anyone watches a converted stream.
  Its running encodes finish, and a held file reads "waiting: Plex is
  converting video on the GPU".
- The pool's outputs live in memory, so its pods carry a 4 GiB memory limit.
  Only a streamed subtitle is held, in a memory-backed `emptyDir`.
- `cmd/video-worker` links ffgo, like `cmd/remux-worker`. The static
  binaries still do not, and `TestNoStaticBinaryLinksFFgo` holds them to it.
- The pool is off by default (chart `videoTranscode.enabled`, kustomize
  component `k8s/components/video-transcode`). Its pods schedule only on a
  node with Intel's GPU plugin and NFD's Intel rules.
