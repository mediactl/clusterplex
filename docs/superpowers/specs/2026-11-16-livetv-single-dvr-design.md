# Live TV: one DVR over a provider's devices (cluster-plex half)

Status: proposed, 2026-11-16. Not built. The design of record, with the rig
findings and the cutover, is clustarr's
`docs/superpowers/specs/2026-11-16-livetv-single-dvr-design.md`.

## The contract

An IPTVProvider whose status carries `dvr` (`title`, `guideURI`,
`guideHash`) wants one DVR over all of its `status.blocks[]` devices on the
XMLTV lineup `XMLTVLineup(dvr.guideURI, dvr.title)`. One without it wants a
DVR per block, as today.

## Changes

- `pkg/plex/livetv`: `Provider.DVR`; `converge` splits into a device step
  (register, replace a stale address, revive) and a DVR step that runs per
  block or per group. In a group: delete a group device's DVR on another
  lineup, create the DVR over every registered device, attach a later one,
  save each device's map on its block's `lineupHash`, and ask the builder
  once (`XMLTVGuides.Want(uuid, guideURI, guideHash)`).
- Every existing rule holds per write: one guide-loading write a pass,
  never a delete, save, reload or create while PMS loads a guide, leases
  per device, the saved and registered notes on each device's Lease (the
  saved note gains the DVR's key as today).
- `pkg/plex/api`: `CreateDVR` takes several device uuids; an attach call
  once the rig settles which one PMS honours.
- `cmd/manager/livetv.go`: reads `status.dvr`.
- No change to `pkg/plex/xmltvguide` or `guidebuild`: a guide is built per
  DVR uuid from a URL and a hash already.

## Tests

- The fake PMS grows several devices per DVR and the attach call.
- Group passes: fresh install, per-block to group (the cutover), group to
  per-block (the way back), a device added later, a create that fails after
  the deletes, a guide load in progress at each step.
