# ADR-0008: Plex's library rows for a provisioned section are a projection of clustarr's MediaFiles

**Status:** Accepted
**Date:** 2026-10-15
**Deciders:** cluster-plex maintainers, with the owner
**Supersedes:** ADR-0006's "media tables only" bound, for provisioned sections

## Context

For a clustarr-backed library, Plex's scanner repeated work clustarr had
already done and recorded on the MediaFile: the path, size and container,
every stream, the chapters and markers, the item it belongs to and that
item's Plex id. A renamed or transcoded file reached Plex in four hops
(clustarr updates the MediaFile; the watcher asks Plex to rescan the old
and the new folder; Plex's scanner makes a new row and trashes the old; the
seeder retries until the new row exists) and left a stale version in the
trash. Plex probed every new file over NFS after clustarr had, and
`Plex Media Scanner --analyze` failed on NFS files outright.

ADR-0006 let cluster-plex write the media tables (`media_items`,
`media_parts`, `media_streams`, marker `taggings`) because Plex's API can
write none of them. It stopped there: the item rows were Plex's.

The capture of 2026-10-15 (`docs/superpowers/specs/2026-10-15-clustarr-scanner-capture.md`)
showed that Plex 1.43 scans a library inside its own process -- the shim
never sees a library scan -- and runs the scanner binary only to analyse one
item. It also showed that Plex fills an item row it did not insert, on
refresh, when the row carries no `hash`.

## Decision

For every section `plex.libraries` provisions, cluster-plex writes the whole
structure of a library entry from the MediaFile, and Plex's scanner never
runs there:

- One `media_items` row per MediaFile, stamped `cp:mediaFile=<uid>` in
  `extra_data`; `media_parts`, `media_streams` and markers with it; a path
  or probe change updates the same row in place.
- The item chain -- a movie, or show → season → episode -- as
  `metadata_items` rows keyed by guid under one rule
  (`plexproject.GUID`): Plex's own id when clustarr holds it, the
  provider's custom form otherwise, re-keyed when the id arrives. An
  inserted row carries the structure only and no `hash`; Plex is asked to
  refresh it and fills title, summary, art and tags from clustarr's
  provider.
- `section_locations` and `directories` for the file's folder, under
  Plex's root row per section.
- `media_analysis_version` at Plex's current value (8), so Plex never
  analyses a projected file; an analyse the binary is still started for is
  answered with exit 0 (`plexscan.AnalyzeRoute`).
- A deleted MediaFile soft-deletes its rows and every item left with
  nothing live, as Plex's scanner does; the empty-trash job empties them.

"Scan Library Files" is a reconcile of Plex's rows against clustarr's
MediaFiles (`plexproject.Projector.Reconcile`): rows whose UID names no
MediaFile are deleted, missing or drifted MediaFiles applied, stamp-less
rows at a known path adopted, and stamp-less rows nobody names -- files
clustarr does not know -- counted and left alone. It runs on the lease
holder, which has the clustarr watch; the 6-hour resync runs it for every
provisioned section; the proxy sends a plain-HTTP scan request to the
configured `--scan-route` (the manager's `/api/v1/scan`, which forwards
to the holder from any pod) and relays the answer, so Plex never walks
the library for one. A TLS client's request still reaches Plex until the
proxy can terminate with Plex's own certificate; that walk finds nothing
to add and nothing to analyse once the projection is current.

Writes stay on the lease holder, one transaction per file, serialised per
section. Sections are owned by id, recorded from provisioning, never by
name.

## Consequences

- Direct writes widen from ADR-0006's media tables to `metadata_items`,
  `directories` and `section_locations`, for provisioned sections only.
  Every other section is Plex's as before.
- Plex's own analysis never runs for a projected file, and the `analyze`
  and `deep-analyze` maintenance tasks find nothing to do there. They
  should not be scheduled for provisioned sections.
- A library scanned by Plex before this ADR is adopted row by row: the
  first reconcile stamps every row whose path a MediaFile names and
  leaves the rest.
- Files clustarr never imported (Plex's own optimised versions, anything
  copied in by hand) are never projected and never deleted by the
  reconcile; clustarr's scan is what finds them.
- The rows depend on Plex's schema, as the shim already does; the
  descriptive fields and images stay Plex's, fetched through the provider
  contract.
