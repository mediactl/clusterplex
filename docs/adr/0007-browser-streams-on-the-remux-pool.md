# ADR-0007: Browser streams run on the remux pool

**Status:** Accepted
**Date:** 2026-10-01 (built 2026-10-05)
**Deciders:** cluster-plex maintainers, with the owner

## Context

Plex is a UI and API here: clustarr probes, detects and transcodes, and
`TranscoderCanOnlyRemuxVideo` stops Plex re-encoding video. The one
transcode Plex still runs is Plex Web's stream: browsers cannot play
Matroska, nor EAC3, AC3, DTS or more than two channels, so every browser
session is the video copied and the audio converted to stereo AAC,
packaged as DASH (44 of 44 DASH jobs in plex-0's log on 2026-10-01).

The shim already sends a transcode to another pod, but that path could not
serve one of these: Plex's transcoder writes its segments into its working
directory, which is the serving pod's per-pod transcode `emptyDir`, and on
a worker that directory does not exist. Every browser stream ran on the pod
serving it.

## Decision

Plex Web's DASH jobs run on a remux pool, in process with ffgo, after
clustarr's transcode engine
(`docs/superpowers/specs/2026-10-01-browser-remux-transcoder-design.md`):

- The manager classifies each shimmed `Plex Transcoder` job: a DASH stream
  of a copied video and a copied or AAC audio track, with no subtitle
  output and only options the 39 subtitle-free real jobs carry, for a
  session `/status/sessions` says Plex Web is playing (`pkg/remux`).
- The job goes, without Plex's token or URLs, to a remux worker chosen by
  consistent hash, so a file returns to the worker holding its cache. The
  worker demuxes once, cuts segments at deterministic keyframe boundaries,
  and muxes each representation into fragmented MP4 through an `io.Writer`
  (`pkg/remux/pipeline`, `cmd/remux-worker`).
- Segments stream back over gRPC; the serving pod's manager writes them
  into Plex's session directory, then posts the manifest that lists them,
  and relays progress, with the session's token (`pkg/remux/relay`).
- Each worker keeps a cache of what it remuxed, keyed by file and audio
  choice, on its own disk; a replay is served from it without decoding.

Anything refused at any step, and any worker failure before the first
segment, runs on Plex's transcoder as before.

## Consequences

- Plex pods carry no FFmpeg 9: only `cmd/remux-worker` links ffgo, and
  `TestNoStaticBinaryLinksFFgo` holds the manager, shim, proxy and
  maintenance binaries to that.
- Browser streams scale with the remux Deployment's autoscaler instead of
  landing on Plex pods.
- Matroska stores no decode timestamps, and libavformat's derivation of
  them restarts after every seek a constant reorder delay off a run from
  the start (measured: 3 frames). Segments of different runs therefore sit
  ~0.13 s apart; a cached replay that crosses from one run's segments to
  another's meets that gap once, which DASH players jump.
- Browsers that cannot decode HEVC still cannot play an HEVC file: that
  needs a real-time video transcode, which this does not do.
- The 5 browser jobs that also stream ASS subtitles stay on Plex's
  transcoder.
