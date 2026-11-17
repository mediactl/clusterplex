# Library renames and PlexHubLayout (cluster-plex half)

Status: built 2026-11-16, not deployed. Proven against the fake PMS only
(§7). The clustarr half (the CRD and its UI) is clustarr's; this document
is the contract the two share and what cluster-plex does with it.

Owner, 2026-11-16: "Rename the Plex libraries from "Clustarr Movies" and
"Clustarr TV" to "Movies" and "TV Shows". Add a mechanism for managing
"Hubs" within the libraries - including collections, sorting, and
everything available from the provided Plex Web UI image. We should also be
able to control the "Library Recommended", "Home", and "Friends' Home"
options directly from the collection UI." The image was Plex Web's "Manage
Recommendations": per library an ordered, drag-sortable list of hubs, each
with three checkboxes, and "Reset Customization".

## 1. A library can be renamed

`plex.libraries[].formerNames` (config file and chart value) lists the
titles a library had. Each provisioning pass, per library:

| Sections found | What happens |
| --- | --- |
| one titled `name`, none of its type under a former name | as before |
| none titled `name`, exactly one of its type under a former name | renamed in place, then as before (agent, settings) |
| none titled `name`, none under a former name | created, as before |
| one titled `name` and one of its type under a former name | conflict |
| none titled `name`, two or more of its type under former names | conflict |

A conflict is logged, raises `clusterplex_library_agent_drift{library}`,
and nothing of the library is written in that pass: no rename, no create,
no agent switch, no settings, and its section key is not reported to the
projection (which of the sections is the library is a person's to say).
A section under a former name but of another type is not the library.

The rename is `api.Client.RenameSection`: `PUT /library/sections/<key>`
with `name`, and the section's own `agent`, `scanner`, `language` and one
`location` per folder sent again, as Plex Web's edit does (PMS refuses an
edit without `agent`; python-plexapi's `LibrarySection.edit` sends `agent`
and whatever is edited). `metadataAgentProviderGroupId` is sent too when
the section is already on the library's configured agent, as
`SetSectionAgent` sends it, and left out otherwise so the rename never
moves a library between agents.

Whatever finds a library by title takes a former title too
(`provision.Config.SectionsTitled`: the sections of exactly that title,
else, when the title is a configured library's name or former name, the
sections of that library's type under any of its names):

- a PlexCollection's `spec.library` (immutable, so "Clustarr Movies" stays
  in every existing resource) and a PlexHubLayout's;
- a non-holder pod's fallback from library names to section keys
  (`provisionedKeys`);
- the seeder's library filter (`Config.LibraryTitles`), so a file seeds
  before the rename as after it.

The projection owns sections by key, which a rename keeps.

## 2. The contract: PlexHubLayout

`plexhublayouts.catalog.clustarr.io/v1alpha1`, namespaced, one per library.
The spec is the UI's; the status is cluster-plex's alone, by server-side
apply under field manager `cluster-plex`. No finalizer. cluster-plex reads
it as unstructured through its own mirror structs
(`pkg/plex/collections/hubs.go`), as it reads PlexCollection: it imports
none of clustarr's code.

```yaml
metadata:
  annotations:
    catalog.clustarr.io/hubs-reset: "2026-11-16T20:00:00Z"   # optional nonce
spec:
  library: Movies                     # the section's title, or a former one
  hubs:                               # optional, at most 100: the wanted order
    - identifier: movie.recentlyreleased   # exactly one of identifier,
      libraryRecommended: true             # collection, collectionTitle
      home: false
      friendsHome: false
    - collection: a24                 # a PlexCollection's name, same namespace
      libraryRecommended: true
      home: true
      friendsHome: true
    - collectionTitle: Studio Ghibli  # a Plex collection no resource manages
      libraryRecommended: true
      home: false
      friendsHome: false
status:
  observedGeneration: 3
  sectionKey: "1"
  resetHandled: "2026-11-16T20:00:00Z"
  hubs:                               # Plex's list, in Plex's order, at most 200
    - identifier: custom.collection.1.16811
      title: A24                      # cut to 256
      kind: collection                # builtin | collection
      collection: a24                 # the PlexCollection holding ratingKey, if any
      ratingKey: "16811"              # a collection hub's
      libraryRecommended: true
      home: true
      friendsHome: true
    - identifier: movie.recentlyreleased
      title: Recently Released Movies
      kind: builtin
      libraryRecommended: true
      home: false
      friendsHome: false
  conditions:
    - type: Ready
      status: "True"
      reason: Synced
      message: Plex's hubs match the layout
      lastTransitionTime: "2026-11-16T20:00:05Z"
      observedGeneration: 3
```

`title`, `collection` and `ratingKey` are left out when empty; the three
booleans, `identifier` and `kind` are always written. `sectionKey`,
`resetHandled` and `hubs` are absent until first known.

`Ready` is True only with reason `Synced`. False reasons: `LibraryNotFound`
(none or several sections of the title), `CollectionNotReady` (a
`collection` whose PlexCollection does not exist or has no
`status.ratingKey` yet), `CollectionNotFound` and `CollectionAmbiguous` (a
`collectionTitle`, matched in any case), `UnknownIdentifier` (an
`identifier` Plex does not list for the library), `InvalidEntry` (none or
several of the three stated, the same hub twice, a PlexCollection of
another library, an unreadable spec), `Conflict` (an older layout of the
same library), `PlexUnavailable` (any PMS error; an unclaimed server lists
no hubs to manage).

## 3. Semantics

- **Listed hubs first, in spec order; the rest after them in Plex's
  relative order, their visibility untouched.**
- A listed hub's three flags are written when they differ. A collection
  entry whose hub does not exist gets one made, promoted as the entry says
  (even nowhere), then ordered.
- A collection's hub is `custom.collection.<sectionKey>.<ratingKey>`.
- **The fewest moves.** The wanted order of every hub is compared with
  Plex's; the hubs already in the wanted relative order (a longest such
  run) stay, and each other hub is moved once, behind the hub it follows
  (`PlanMoves`). An order that already holds sends nothing.
- **A reset** is asked for by a new value of the annotation: Plex's "Reset
  Customization" for the section, once, before the layout is applied on
  top; `status.resetHandled` records the honoured value. The value is a
  nonce: any non-empty value different from the recorded one is honoured.
- **A collection a layout lists takes its visibility from the layout.**
  The PlexCollection syncer skips its own `spec.visibility` for a
  collection a PlexHubLayout of its namespace lists by `collection`
  (`HubController.Lists`, `Previous.HubLayout`). A collection no layout
  lists behaves as before.
- An entry removed from the spec is left as Plex has it. Deleting the
  layout changes nothing in Plex.
- **When:** a layout as it is added, when its generation or reset
  annotation changes; every layout when a PlexCollection gains, loses or
  changes its `status.ratingKey`, every 10 minutes, and after PMS restarts.
  Never while this pod's Plex is not serving (retried every 15 s). Lease
  holder only, behind `plex.clustarr.enabled`, started with the collections
  controller. It waits for the CRD and for its RBAC (a chart older than the
  image) as the collections controller does.
- **Cost:** the section list is read once and kept (dropped at a PMS
  restart, when the library is not found, and when a section's hubs answer
  404). An idle pass is one `GET /hubs/sections/<key>/manage`, no write to
  Plex and no status write (the status is compared first).

## 4. Rulings where the brief was silent

1. **An entry that cannot be resolved is skipped; the rest are applied.**
   `Ready` is False with the first problem's reason and every problem in
   the message. A layout naming a collection still `Waiting` in clustarr
   would otherwise freeze the whole library's order for days.
2. **A `collectionTitle` entry costs one more GET** per pass (the
   library's collections): a collection with no hub yet is not in the
   managed list, and matching titles among existing hubs alone could pick
   the wrong one of two.
3. **One layout per library is enforced**: a newer layout of the same
   `spec.library` (in any case) or the same recorded `sectionKey` is
   `Conflict` and changes nothing.
4. **A `collection` entry's PlexCollection must be of this library**
   (`InvalidEntry` otherwise): a hub is not made for another section's
   collection.
5. **The reset nonce is not parsed.** Any new non-empty value resets.
6. A reset Plex refuses is not recorded and is tried again next pass.
7. `status.hubs` beyond 200 is cut, and the `Ready` message says how many
   Plex lists.
8. When a layout stops listing a collection, the collection's own
   `spec.visibility` returns at that collection's next sync (within 10
   minutes); nothing wakes it sooner.
9. Events: `Applied` (Normal) when a pass wrote to Plex, and a Warning
   with the reason when `Ready` turns to a new False reason.
10. In a library-rename conflict the library's section key is withheld
    from the projection (§1).
11. `DeleteManagedHub` exists in `pkg/plex/api` and nothing calls it: the
    contract never removes a hub.

## 5. The requests

All under the holder's own PMS (`pkg/plex/api`):

| Operation | Request |
| --- | --- |
| list sections | `GET /library/sections/all` |
| rename a library | `PUT /library/sections/<key>?name=<new>&agent=<its agent>&scanner=<its scanner>&language=<its language>&location=<path>[&location=...][&metadataAgentProviderGroupId=<id>]` |
| list a library's hubs | `GET /hubs/sections/<key>/manage` |
| list a library's collections (`collectionTitle` only) | `GET /library/sections/<key>/collections` |
| a hub's title Plex left out | `GET /library/metadata/<ratingKey>` |
| make a collection's hub | `POST /hubs/sections/<key>/manage?metadataItemId=<ratingKey>&promotedToRecommended=<0\|1>&promotedToOwnHome=<0\|1>&promotedToSharedHome=<0\|1>` |
| set a hub's visibility | `PUT /hubs/sections/<key>/manage/<identifier>?promotedToRecommended=<0\|1>&promotedToOwnHome=<0\|1>&promotedToSharedHome=<0\|1>` |
| move a hub | `PUT /hubs/sections/<key>/manage/<identifier>/move?after=<identifier>`; first place: no query |
| reset | `DELETE /hubs/sections/<key>/manage` |
| remove one hub (unused) | `DELETE /hubs/sections/<key>/manage/<identifier>` |

## 6. As built

- `pkg/plex/api`: `ManagedHub.Title`, `MoveManagedHub`,
  `ResetManagedHubs`, `DeleteManagedHub`, `Section.Scanner`,
  `RenameSection`.
- `pkg/plex/provision`: `Library.FormerNames`, the rename in
  `ensureLibrary`, `Config.SectionsTitled`, `Config.LibraryTitles`.
- `pkg/plex/collections`: `hubs.go` (`HubSyncer`, `PlanMoves`, the mirror
  structs), `hubs_controller.go` (`HubController`), `SectionFinder` on both
  syncers, `Previous.HubLayout`, `Controller.HubLayouts`.
- `cmd/manager/collections.go`: both controllers started by
  `collectionsLoop`; subsystem `collections.hubs`.
- Chart: `plex.libraries[].formerNames`; the `clustarr-reader` Role grants
  `get`, `list`, `watch` on `plexhublayouts` and `patch` on
  `plexhublayouts/status` with `plex.clustarr.enabled`. The RBAC lives in
  this chart, not clustarr's.

## 7. Not proven against a server

Everything here ran against the fake PMS (`fakepms_test.go`) only. The
recorded, known-good calls are the managed-hub list, create and visibility
update (collections, talos). To check on a rig, on a claimed server:

1. `PUT /library/sections/<key>` with `name` and the section's fields
   again renames it and keeps its agent, group, locations and settings.
2. `PUT .../manage/<identifier>/move?after=<identifier>` and the same
   without a query (first place) reorder a built-in and a collection hub,
   and `GET .../manage` returns the hubs in the order Plex Web shows them.
3. `DELETE .../manage` is "Reset Customization" and removes the
   collection hubs.
4. A hub made by `POST .../manage` is listed as
   `custom.collection.<sectionKey>.<ratingKey>` and where (assumed: last).
5. A hub is made with all three flags `0` without error.
