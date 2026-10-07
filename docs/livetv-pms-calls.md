# PMS's Live TV calls, as recorded

Recorded on 2026-10-07 against PMS `1.43.4.10903-e5521bd8c` on kind-cluster-plex,
with the shim at `v1.3.17-clusterplex.25`, by `hack/record-livetv.sh`. The
answers are in `pkg/plex/api/testdata/livetv`, token-scrubbed. The device was
`hack/hdhrprobe`: an HDHomeRun with N synthetic channels and an XMLTV guide
with a 2-hour programme per channel.

Every call sends `Accept: application/json` and `X-Plex-Token`. Parameters go
in the query string; PMS reads no request body (see "The size limit").

## Before anything: the shim

Before `v1.3.17-clusterplex.25`, `POST /livetv/dvrs` with an XMLTV lineup
answered 500. The guide database's migration failed on `cannot VACUUM - SQL
statements in progress` (hack/plex-postgresql/README.md, "Fixed: creating a
Live TV DVR"). That failed creation left a DVR row with no device behind. PMS
listed it only after a restart (`00-ghost-dvrs.json`), and
`DELETE /livetv/dvrs/<key>` removed it with 200.

## The calls

| Step | Call | Answer |
| --- | --- | --- |
| 1 | `GET /media/grabbers/devices` | `MediaContainer.Device[]`, `size` |
| 2 | `POST /media/grabbers/devices/discover?uri=<device URL>` | `size: 0` for the probe. Not an error, and not needed |
| 3 | `POST /media/grabbers/devices?uri=<device URL>` | the `Device` it registered |
| 4 | `GET /media/grabbers/devices` | the device, now listed |
| 5 | `POST /livetv/dvrs?language=eng&device=<device uuid>&lineup=<lineup>` | the new `Dvr`, with its `Device` |
| 6 | `GET /livetv/dvrs` | `MediaContainer.Dvr[]` |
| 7 | `GET /livetv/epg/channelmap?device=<device uuid>&lineup=<lineup>` | `MediaContainer.ChannelMapping[]` |
| 8 | `PUT /media/grabbers/devices/<device key>/channelmap?channelsEnabled=…&channelMappingByKey[<id>]=…&channelMapping[<id>]=…` | `{size: 0, message: "", status: 0}` |
| 9 | `GET /livetv/dvrs/<dvr key>`, `GET /media/grabbers/devices/<device key>` | the DVR, and the device with its `ChannelMapping[]`, each `enabled: "1"` |
| 10 | `POST /livetv/dvrs/<dvr key>/reloadGuide` | 200, empty body |
| cleanup | `DELETE /livetv/dvrs/<dvr key>` | 200, empty body |
| cleanup | `DELETE /media/grabbers/devices/<device key>` | `{size: 0, message: "", status: 0}` |

The lineup is `lineup://tv.plex.providers.epg.xmltv/<guide URL>#<title>`. The
guide URL is inside it unescaped, and the whole lineup is then escaped as one
query value.

### The JSON the code reads

- **Device**:
  - `key`, a string (`"4"`);
  - `uuid`, `device://tv.plex.grabbers.hdhomerun/<DeviceID>`, which is what
    `device=` takes;
  - `uri`, `deviceId`, `state`, `status`, `tuners`;
  - `model`, which is the device's FriendlyName (there is no `name`);
  - `parentID`, **a number**, the DVR's key once the device has one.
- **Dvr**:
  - `key`, a string, and `uuid`;
  - `lineup`, `lineupTitle`, `language`, and `epgIdentifier`
    (`tv.plex.providers.epg.xmltv:<key>`);
  - `Lineup[]` (`{id, title}`), `Device[]` and `Setting[]`;
  - there is **no `title`**.
- **ChannelMapping**:
  - from the channel map: `channelKey`, `deviceIdentifier`,
    `lineupIdentifier` and `favorite`;
  - read from the device, also `enabled` (`"1"`).
  - For the probe all three identifiers are the GuideNumber, and that
    number is the XMLTV channel id.

## The size limit: 32 KiB of request line, not a channel count

The save in step 8 must carry the whole map in one request. A save replaces
the device's map, so a batch would keep only its own channels (iptvtunerr's
finding). That one request's URL is the limit.

| channels in one PUT | encoding | path + query | answer |
| --- | --- | --- | --- |
| 100 | Go `url.Values` | 7,250 bytes | 200, 100 enabled |
| 454 | Go `url.Values` | 32,738 bytes | 200 |
| 455 | Go `url.Values` | 32,810 bytes | **400** |
| 527 | brackets and commas left unescaped, as iptvtunerr sends them | 32,726 bytes | 200 |
| 528 | the same | 32,788 bytes | **400** |
| 480, 481, 1000 | Go `url.Values` | 34,610 / 34,682 / 72,050 bytes | **400** |

- **How PMS fails it.** The 400 is PMS's HTTP parser refusing the request
  line: it logs `Error parsing HTTP request`, the answer is an HTML
  `400 Bad Request`, and the map in place is kept (100 enabled after every
  400).
- **Where the edge is.** Both encodings stop at the same length, about
  32,768 bytes once `PUT ` and ` HTTP/1.1` are added. The wizard's "~480
  channels" is this same wall.
- **No way around it in the request.**
  - A form-encoded body is ignored: `PUT` with the parameters in an
    `application/x-www-form-urlencoded` body answered 400 and logged
    `Missing required query parameter channelsEnabled`.
  - **That parameter-less save cleared the device's map (0 enabled).** Never
    send a save without the whole map.
  - The same body sent as a `POST` answered 404.
- **What a channel costs.** With Go's encoding it is about 72 bytes, for a
  4-digit GuideNumber used as all three identifiers. Longer identifiers cost
  more.

The guide side has no such limit. The channel map in step 7 listed all
1,000, 2,000 and 5,000 channels after a `reloadGuide`, with no device rescan:
PMS re-reads `lineup.json` and the guide itself. (The 2,000 and 5,000 saves
never reached PMS. They were over curl's argument limit, which the script now
avoids by passing the URL through a config file.)

**Consequence for the design.** One device holds at most ~450 channels
through the API. Spec §3.1's "no channel cap" does not hold for the API path.

## Which PMS answers

- **Writes go to the lease holder.** `plex-main` balances across pods.
- **Each PMS process caches its DVRs from its start.** Devices are read from
  the database on every request, but a DVR created or deleted on one pod is
  not seen by the others until they restart:
  - plex-0 and plex-1, started before the ghost DVR was deleted, kept
    listing it;
  - plex-2, restarted afterwards, listed only the DVR it had made.

  A client that `plex-main` sends to another pod sees that pod's DVRs.

## Deleting while the guide loads deadlocks PMS

- **What happened.** A channel-map save starts a guide rebuild (activity
  `provider.epg.load`, "Refreshing guide data"), which creates and migrates
  a new guide database. A `DELETE /livetv/dvrs/<key>` sent while that rebuild
  was migrating never returned, and from then on every `/livetv/dvrs` request
  on that PMS hung.
- **Where the threads were** (from their stacks):
  - the migration thread, inside SQLite, called back into PMS and waited on
    a mutex;
  - the request thread holding that mutex was in a condition wait.
- **The fix.** Only a pod restart cleared it.
- **The rule.** Never delete a DVR (and do not save its map again) while
  `GET /activities` lists a `provider.epg.load`. Wait for it to end first.

## What the wizard is needed for

Nothing. Steps 3, 5 and 8 make a working DVR with no setup wizard. A device
uses the probe's FriendlyName (`model`); the DVR's guide is the XMLTV lineup.
