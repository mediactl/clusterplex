# clustarr scanner implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Plex's rows for a clustarr-backed library are written from clustarr's MediaFiles by cluster-plex, and `Plex Media Scanner` runs for those libraries become a reconcile of Plex's rows against clustarr's records.

**Architecture:** A new package `pkg/plexproject` writes `metadata_items`, `section_locations`, `directories`, `media_items`, `media_parts` (and, through `pkg/plexseed`, streams and markers) from a MediaFile and its item, keyed by the MediaFile UID stamped in `media_items.extra_data`. `pkg/clustarrwatch` feeds it from the informer it already runs, on the lease holder. A new package `pkg/plexscan` parses the scanner's argv and, on any pod, forwards a scan of a provisioned section to the lease holder over the manager's HTTP API, where it runs as a reconcile.

**Tech Stack:** Go 1.25, pgx v5 against PostgreSQL (the plex-postgresql shim's schema), client-go dynamic informers, cobra/viper config, testify. Tests that touch the database skip unless `CLUSTERPLEX_TEST_POSTGRES_DSN` is set.

**Spec:** `docs/superpowers/specs/2026-10-15-clustarr-scanner-design.md`

## Global Constraints

- Every Go file carries the repository's licence header; every test name is a sentence naming the behaviour (`TestAFileNotInPlexIsRetriedOnAShortBackoff`), never the function.
- Commit with a pathspec (`git commit -m '...' -- <paths>`), never a bare `git commit`; `git add` every new file first. Never `git stash`.
- Writes to Plex's database happen only on the lease holder, only for provisioned sections (spec §4, §9), in one transaction per file (spec §6).
- A row with no UID and no matching path is never deleted (spec §8).
- `media_analysis_version` is always written at the value captured in Task 1 (spec §6 step 4).
- The guid rule is one function (`plexproject.GUID`), used by every lookup and insert (spec §4).
- Run `make vet`, `make lint` and `go test ./<package>/...` before every commit; `make test` once at the end of each task that touched more than one package.
- Database tests: copy the `plexDB` helper pattern (`pkg/plexseed/seeder_test.go:25-71`) and create the schema with `bootstrap.InitSchema`; never share rows between tests, and name every inserted row with the test's own ids.

## Review Focus

1. A MediaFile whose item has no Plex id yet gets the custom guid; when the id arrives later, the row must be re-keyed to `plex://…` (watched state keyed by guid would otherwise split). Pinned in Task 6 (`TestAnItemThatGainsAPlexIDIsReKeyed`).
2. Two MediaFiles of one item (two versions, or a donor): both rows hang off one `metadata_items` row, and deleting one must not trash the item. Pinned in Task 8 (`TestDeletingOneOfTwoVersionsKeepsTheItem`).
3. A path mapping that does not cover a file (a root folder outside `plex.clustarr.pathMappings`): skipped and counted, never inserted under an unmapped path. Pinned in Task 10 (`TestAnUnmappedFileIsCountedNotProjected`).
4. A scan request for a section that is not provisioned, or with a path outside its locations, must reach the real scanner untouched. Pinned in Task 3 and Task 12.
5. A scan arriving on a non-holder pod while no pod holds the lease (an election in progress): the shim must get a non-zero exit, not hang. Pinned in Task 12 (`TestAScanWithNoHolderFailsFast`).

---

### Task 1: Capture what Plex does (the deferred spike)

No code. Produces the facts spec §10 leaves open, as a file the later tasks read.

**Files:**
- Create: `docs/superpowers/specs/2026-10-15-clustarr-scanner-capture.md`
- Create: `pkg/plexscan/testdata/argv.txt`

- [ ] **Step 1: Capture the scanner's argv on talos.** The dispatcher logs nothing about scanner args today, so read them from the process table while a scan runs. In one terminal start a watch, in another trigger each kind of scan:

```bash
P=$(kubectl --context admin@talos -n clustarr-system get pods -l app.kubernetes.io/name=plex -o name | head -1)
# terminal 1: sample argv of every scanner process, deduplicated
kubectl --context admin@talos -n clustarr-system exec $P -c plex -- sh -c \
  'while true; do for p in /proc/[0-9]*; do c=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$c" in *"Media Scanner"*) echo "$c";; esac; done; sleep 0.2; done' | sort -u
```

```bash
# terminal 2, with the token from Secret clustarr-system/plex-token:
T=$(kubectl --context admin@talos -n clustarr-system get secret plex-token -o jsonpath='{.data.token}' | base64 -d)
kubectl --context admin@talos -n clustarr-system port-forward svc/plex 32499:32400 &
curl -s -X POST -H "X-Plex-Token: $T" 'http://127.0.0.1:32499/library/sections/2/refresh'                      # button / API section scan
curl -s -X POST -H "X-Plex-Token: $T" --data-urlencode 'path=/media/movies/Heat (1995) {tmdb-949}' 'http://127.0.0.1:32499/library/sections/2/refresh'  # folder scan
curl -s -X POST -H "X-Plex-Token: $T" 'http://127.0.0.1:32499/library/sections/2/refresh?force=1'              # forced
```

Save every distinct line to `pkg/plexscan/testdata/argv.txt`, one argv per line, with a leading `# section|folder|forced|other` comment naming which trigger produced it. Expect the shape `Plex Media Scanner --scan --refresh --section 2 [--directory <path>] [--force] --log-file-suffix …`; if Plex uses short flags (`-s -r -c 2 -d <path>`), record those.

- [ ] **Step 2: Capture Plex's current `media_analysis_version`.**

```bash
kubectl --context admin@talos -n clustarr-system exec clustarr-plex-db-1 -c postgres -- \
  psql -U postgres -d plex -Atc "SELECT media_analysis_version, count(*) FROM plex.media_items WHERE deleted_at IS NULL GROUP BY 1 ORDER BY 2 DESC"
```

Record the dominant non-zero value as `MediaAnalysisVersion` in the capture doc.

- [ ] **Step 3: Test an inserted item row on a copy, never on the live database.** Clone the live database, insert one movie's rows by hand for a file Plex does not yet know, point a throwaway Plex at it? That needs a second Plex; instead do the cheaper check on the live server with a file that already exists: pick a movie Plex knows, `UPDATE plex.metadata_items SET refreshed_at = NULL, summary = '' WHERE id = <id>`, call `PUT /library/metadata/<id>/refresh`, and confirm Plex refills `summary` from clustarr's provider. Then the insert case: in one transaction insert a `metadata_items` row (type 1, the library's `library_section_id`, a guid of the `plex://movie/<24hex>` form for a movie the provider knows and the library does not have a file for, `title`, `title_sort`, `year`, `added_at`, `created_at`, `updated_at`, `media_item_count = 0`), call refresh on its id, and record whether Plex fills `summary`, `user_thumb_url` and `tags_genre`. Delete the row afterwards (`DELETE FROM plex.metadata_items WHERE id = …` and any `taggings` it gained). Record the answer and the columns that had to be set.

- [ ] **Step 4: Check client notifications.** With `curl -N -H "X-Plex-Token: $T" -H 'Accept: text/event-stream' http://127.0.0.1:32499/:/eventsource/notifications` running, do the insert of step 3 and note whether a `timeline` event arrives without a scan. Record it.

- [ ] **Step 5: Write the capture doc** with four headings (Scanner argv, Media analysis version, Refresh of an inserted row, Notifications), each stating what was observed verbatim. Commit:

```bash
git add docs/superpowers/specs/2026-10-15-clustarr-scanner-capture.md pkg/plexscan/testdata/argv.txt
git commit -m "docs: capture the scanner's argv, Plex's media_analysis_version and what refresh does to an inserted row" -- docs/superpowers/specs/2026-10-15-clustarr-scanner-capture.md pkg/plexscan/testdata/argv.txt
```

If step 3 shows Plex will not fill an inserted row, stop and report: the spec's §10 says the descriptive layer then becomes its own design.

---

### Task 2: `plexdb.Pool.Query`

**Files:**
- Modify: `pkg/plex/db/pool.go`
- Test: `pkg/plex/db/pool_test.go` (create if absent)

**Interfaces:**
- Produces: `func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)`

- [ ] **Step 1: Write the failing test.** The package's other DB tests use `CLUSTERPLEX_TEST_POSTGRES_DSN`; follow `pkg/plex/bootstrap/postgres_test.go`'s `freshDatabase` to get a config, then `Open`.

```go
func TestQueryReturnsEveryRow(t *testing.T) {
	dsn := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	p, err := Open(context.Background(), Config{Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database,
		User: cfg.User, Password: cfg.Password, Schema: "public"})
	require.NoError(t, err)
	t.Cleanup(p.Close)
	rows, err := p.Query(context.Background(), "SELECT n FROM generate_series(1, 3) AS g(n) ORDER BY n")
	require.NoError(t, err)
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		require.NoError(t, rows.Scan(&n))
		got = append(got, n)
	}
	require.Equal(t, []int{1, 2, 3}, got)
}
```

- [ ] **Step 2: Run it:** `CLUSTERPLEX_TEST_POSTGRES_DSN=… go test ./pkg/plex/db/ -run TestQueryReturnsEveryRow` — expect a compile error, `p.Query undefined`.

- [ ] **Step 3: Implement**, next to `QueryRow` in `pool.go`:

```go
// Query runs a statement that returns rows. The caller closes the rows.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}
```

(`p.pool` is the unexported `*pgxpool.Pool`; use whatever name `QueryRow` uses.)

- [ ] **Step 4: Run the test:** PASS. Run `go test ./pkg/plex/db/`.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plex/db/pool.go pkg/plex/db/pool_test.go
git commit -m "db: Pool.Query for multi-row reads" -- pkg/plex/db/pool.go pkg/plex/db/pool_test.go
```

---

### Task 3: Parse the scanner's argv

**Files:**
- Create: `pkg/plexscan/args.go`
- Test: `pkg/plexscan/args_test.go`
- Uses: `pkg/plexscan/testdata/argv.txt` (Task 1)

**Interfaces:**
- Produces:
  ```go
  package plexscan
  type Kind int
  const ( KindSection Kind = iota + 1; KindFolder )
  type Request struct { Section string; Path string; Force bool; Kind Kind }
  // Parse reads a Plex Media Scanner argv (without argv[0]). ok is false for
  // anything that is not a library scan (analyze, tree, help).
  func Parse(args []string) (req Request, ok bool)
  ```

- [ ] **Step 1: Write the failing tests.** The table rows come from `testdata/argv.txt`; the first three below are the expected shapes and must be replaced by the captured lines if they differ.

```go
func TestParseReadsEveryCapturedScannerArgv(t *testing.T) {
	b, err := os.ReadFile("testdata/argv.txt")
	require.NoError(t, err)
	var kind string
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		case strings.TrimSpace(line) == "":
		default:
			args := strings.Fields(line)[1:] // drop "Plex" ... argv[0] is two words; fields of the joined cmdline
			args = argsAfterBinary(line)
			req, ok := Parse(args)
			switch kind {
			case "section":
				require.True(t, ok, line)
				require.Equal(t, KindSection, req.Kind, line)
				require.NotEmpty(t, req.Section, line)
			case "folder":
				require.True(t, ok, line)
				require.Equal(t, KindFolder, req.Kind, line)
				require.NotEmpty(t, req.Path, line)
			case "forced":
				require.True(t, ok, line)
				require.True(t, req.Force, line)
			default:
				require.False(t, ok, line)
			}
		}
	}
}

// argsAfterBinary splits a captured cmdline on the binary's name.
func argsAfterBinary(line string) []string {
	_, rest, _ := strings.Cut(line, "Plex Media Scanner")
	return strings.Fields(rest)
}

func TestParseKnowsBothFlagSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want Request
	}{
		{"long section", []string{"--scan", "--refresh", "--section", "2", "--log-file-suffix", "x"}, Request{Section: "2", Kind: KindSection}},
		{"short folder", []string{"-s", "-r", "-c", "3", "-d", "/media/tv/Show", "--force"}, Request{Section: "3", Path: "/media/tv/Show", Force: true, Kind: KindFolder}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Parse(tc.args)
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseLeavesAnalyzeAndTreeToPlex(t *testing.T) {
	for _, args := range [][]string{
		{"--analyze", "--item", "12"}, {"-a", "-i", "12"}, {"--tree", "--section", "2"}, {"--help"}, {},
	} {
		_, ok := Parse(args)
		require.False(t, ok, "%v", args)
	}
}
```

- [ ] **Step 2: Run:** `go test ./pkg/plexscan/ -run TestParse` — FAIL, `Parse` undefined.

- [ ] **Step 3: Implement** `args.go`:

```go
// Package plexscan turns a Plex Media Scanner run into a reconcile of
// clustarr's projection (spec 2026-10-15 §5): the argv is parsed here, the
// Route in route.go decides where it runs.
package plexscan

// Kind is what the scanner was asked to walk.
type Kind int

const (
	// KindSection is a whole library: Scan Library Files, the API without
	// a path, the maintenance refresh.
	KindSection Kind = iota + 1
	// KindFolder is one directory of a library: the API's ?path=.
	KindFolder
)

// Request is a library scan the scanner was started for.
type Request struct {
	Section string
	Path    string
	Force   bool
	Kind    Kind
}

// Parse reads a scanner argv without argv[0]. Only a scan (--scan/-s with
// a --section/-c) is ours; an analyze, a tree, a help or anything without
// a section is left to Plex's own binary, so ok is false for those.
func Parse(args []string) (Request, bool) {
	var req Request
	scan := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--scan", "-s":
			scan = true
		case "--section", "-c":
			req.Section = next()
		case "--directory", "-d":
			req.Path = next()
		case "--force":
			req.Force = true
		case "--analyze", "-a", "--analyze-deeply", "--tree", "-t", "--help", "-h", "--item", "-i", "--file", "-f":
			return Request{}, false
		case "--log-file-suffix", "--refresh", "-r", "--no-thumbs":
			if a == "--log-file-suffix" {
				next()
			}
		}
	}
	if !scan || req.Section == "" {
		return Request{}, false
	}
	req.Kind = KindSection
	if req.Path != "" {
		req.Kind = KindFolder
	}
	return req, true
}
```

Adjust the flag set to the captured argv: any flag Task 1 saw that takes a value must be consumed with `next()`.

- [ ] **Step 4: Run:** PASS. Falsify once: remove the `!scan` guard and watch `TestParseLeavesAnalyzeAndTreeToPlex` fail on `{"--tree", "--section", "2"}`; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexscan/args.go pkg/plexscan/args_test.go
git commit -m "plexscan: parse the scanner's argv into a section or folder scan" -- pkg/plexscan
```

---

### Task 4: `plexseed` learns HDR, commentary, bitmap subtitles, the analysis version, and a transaction-level entry point

**Files:**
- Modify: `pkg/plexseed/input.go`, `pkg/plexseed/rows.go`, `pkg/plexseed/seeder.go`
- Modify: `pkg/clustarrwatch/fields.go` (`SeedInputOf` copies the new fields; `Trim` already keeps `status.mediaInfo` whole)
- Test: `pkg/plexseed/rows_test.go`, `pkg/plexseed/seeder_test.go`

**Interfaces:**
- Consumes: the `MediaAnalysisVersion` value from Task 1's capture doc.
- Produces:
  ```go
  type Probe struct { ...existing...; HDR string }        // clustarr's status.mediaInfo.hdr enum, "" for SDR
  type Audio struct { ...existing...; Commentary bool }
  type Subtitle struct { ...existing...; Bitmap bool }
  type MediaRow struct { ...existing...; ColorTRC string; AnalysisVersion int32 }
  // MediaAnalysisVersion is Plex's current analysis version (capture doc);
  // a media_items row at it is never analysed by Plex.
  const MediaAnalysisVersion int32 = <captured>
  // Target is one media row the seeder writes to: its media_parts id,
  // media_items id and metadata_items id.
  type Target struct { Part, Media, Item int64 }
  // SeedTx seeds one target inside the caller's transaction; Seed finds
  // targets by path and calls it.
  func (s *Seeder) SeedTx(ctx context.Context, tx pgx.Tx, t Target, in Input) (Result, error)
  ```

- [ ] **Step 1: Write the failing tests.** In `rows_test.go`:

```go
func TestAnHDRProbeSetsColorTRCAndTheAnalysisVersion(t *testing.T) {
	row, _ := MediaRows(Probe{Container: "mkv", VideoCodec: "hevc", HDR: "hdr10", Width: 3840, Height: 2160}, "h", "en")
	require.Equal(t, "smpte2084", row.ColorTRC)
	require.Equal(t, MediaAnalysisVersion, row.AnalysisVersion)
	sdr, _ := MediaRows(Probe{Container: "mkv", VideoCodec: "h264"}, "h", "en")
	require.Equal(t, "bt709", sdr.ColorTRC)
}

func TestHLGAndDolbyVisionMapToTheirTransferCurves(t *testing.T) {
	for in, want := range map[string]string{"hlg10": "arib-std-b67", "dovi-p8-hdr10": "smpte2084", "pq10": "smpte2084", "": "bt709"} {
		row, _ := MediaRows(Probe{HDR: in}, "h", "en")
		require.Equal(t, want, row.ColorTRC, in)
	}
}

func TestCommentaryAndBitmapFlagsReachTheStreamExtra(t *testing.T) {
	_, streams := MediaRows(Probe{
		Audio:     []Audio{{Index: 1, Codec: "aac", Commentary: true}},
		Subtitles: []Subtitle{{Index: 2, Codec: "pgs", Bitmap: true}},
	}, "h", "en")
	require.Contains(t, streams[0].Extra, `"ma:commentary":"1"`)
	require.Contains(t, streams[1].Extra, `"ma:bitmap":"1"`)
}
```

In `seeder_test.go`, next to `TestAnUnanalysedFileIsSeededOnce`:

```go
func TestSeedTxWritesTheAnalysisVersionSoPlexNeverAnalyses(t *testing.T) {
	conn := plexDB(t)
	part, media, item := insertFile(t, conn, "/media/movies/A (2020)/A.mkv", "Clustarr Movies")
	s := seeder(conn)
	tx, err := conn.Begin(context.Background())
	require.NoError(t, err)
	_, err = s.SeedTx(context.Background(), tx, plexseed.Target{Part: part, Media: media, Item: item},
		plexseed.Input{Path: "/media/movies/A (2020)/A.mkv", ProbeHash: "h1", Probe: &plexseed.Probe{Container: "mkv", VideoCodec: "hevc", HDR: "hdr10"}})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(context.Background()))
	require.Equal(t, int(plexseed.MediaAnalysisVersion), count(t, conn, "SELECT media_analysis_version FROM media_items WHERE id = $1", media))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND color_trc = 'smpte2084'", media))
}
```

(`insertFile` returns ids today; if it returns fewer, extend it to return all three.)

- [ ] **Step 2: Run:** `go test ./pkg/plexseed/ -run 'TestAnHDR|TestHLG|TestCommentary|TestSeedTx'` — FAIL to compile.

- [ ] **Step 3: Implement.** `input.go`: add `HDR string \`json:"hdr"\`` to `Probe`, `Commentary bool \`json:"commentary"\`` to `Audio`, `Bitmap bool \`json:"bitmap"\`` to `Subtitle`. `rows.go`: add to `MediaRow` `ColorTRC string; AnalysisVersion int32`; add

```go
// MediaAnalysisVersion is the media_analysis_version Plex 1.43 stamps on a
// file it has analysed (captured 2026-10-15, spec §6 step 4). A row at it
// is never analysed by Plex, which is the point: clustarr's probe is the
// analysis.
const MediaAnalysisVersion int32 = <captured value>

// colorTRC is the media_items.color_trc Plex records for a transfer curve.
func colorTRC(hdr string) string {
	switch {
	case hdr == "":
		return "bt709"
	case hdr == "hlg10":
		return "arib-std-b67"
	default: // hdr10, hdr10plus, pq10 and every Dolby Vision profile carry PQ
		return "smpte2084"
	}
}
```

and in `MediaRows` set `row.ColorTRC = colorTRC(p.HDR)`, `row.AnalysisVersion = MediaAnalysisVersion`, and in the audio and subtitle stream loops add `"ma:commentary": "1"` / `"ma:bitmap": "1"` to the extra map when set. `seeder.go`: write `color_trc = $n, media_analysis_version = $n` in the `media_items` UPDATE of `seedMedia`; export `Target` (rename the unexported `target`) and split `Seed` so the per-target body is

```go
// SeedTx seeds one target inside tx: the media row, its streams and its
// markers. Seed finds the targets of a path and calls it in a transaction of
// its own; the projection (pkg/plexproject) calls it inside the transaction
// that wrote the row.
func (s *Seeder) SeedTx(ctx context.Context, tx pgx.Tx, t Target, in Input) (Result, error) {
	var r Result
	n, err := s.seedMedia(ctx, tx, t, in)
	if err != nil {
		return r, err
	}
	r.Parts, r.MediaSeeded = 1, n
	w, rm, err := s.reconcileMarkers(ctx, tx, t.Item, in)
	if err != nil {
		return r, err
	}
	r.MarkersWritten, r.MarkersRemoved = w, rm
	return r, nil
}
```

with `Seed` looping its found targets over `SeedTx` and summing results. `fields.go`'s `SeedInputOf` decodes `status.mediaInfo` through JSON into `Probe`, so the new tags pick the fields up; confirm with the existing `fields_test.go` fixture (`test/data/clustarr/mediafile.yaml` carries `hdr`? if not, add `hdr: hdr10` and a `commentary: true` audio stream to the fixture and assert them in `TestSeedInputOf…`).

- [ ] **Step 4: Run** `go test ./pkg/plexseed/ ./pkg/clustarrwatch/` — PASS. Falsify: set `colorTRC` to always return `bt709`, watch `TestAnHDRProbeSetsColorTRC…` fail; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexseed pkg/clustarrwatch/fields.go pkg/clustarrwatch/fields_test.go test/data/clustarr
git commit -m "plexseed: HDR to color_trc, commentary and bitmap flags, the analysis version, and SeedTx for a caller's transaction" -- pkg/plexseed pkg/clustarrwatch test/data/clustarr
```

---

### Task 5: `plexproject` identity: the guid rule and the resolved item chain

**Files:**
- Create: `pkg/plexproject/guid.go`, `pkg/plexproject/resolve.go`
- Modify: `pkg/clustarrwatch/fields.go` (`Trim` keeps `spec.seasonNumber`, `spec.episodeNumber`, `status.plexID`; `Guid` delegates to `plexproject.GUID`)
- Test: `pkg/plexproject/guid_test.go`, `pkg/plexproject/resolve_test.go`, `pkg/clustarrwatch/fields_test.go`

**Interfaces:**
- Produces:
  ```go
  package plexproject
  // Provider identifiers, as clustarr's chart names them.
  const ( MoviesProvider = "tv.plex.agents.custom.clustarr.movies"; TVProvider = "tv.plex.agents.custom.clustarr.tv" )
  type ItemType int
  const ( TypeMovie ItemType = 1; TypeShow ItemType = 2; TypeSeason ItemType = 3; TypeEpisode ItemType = 4 )
  // GUID is the guid Plex stores for an item (spec §4): plex://<type>/<id>
  // when plexID is set, else the provider's custom form over ratingKey.
  func GUID(provider string, t ItemType, ratingKey, plexID string) string
  // Node is one row of the item chain, top down.
  type Node struct { Type ItemType; GUID, Title, TitleSort string; Index int32; Year int32; AddedAt time.Time }
  // Item is a MediaFile's resolved item: Chain[0] is the movie or show,
  // Chain[len-1] the row the media hangs off.
  type Item struct { Provider string; Chain []Node; OriginalLanguage string }
  // Lookup finds an item object by kind and name in the namespace.
  type Lookup func(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool)
  // Resolve builds the chain for a MediaFile from its mediaRef; ok is false
  // when the item (or an episode's series) is not in the caches yet.
  func Resolve(file *unstructured.Unstructured, look Lookup) (Item, bool)
  ```

- [ ] **Step 1: Write the failing tests.** `guid_test.go`:

```go
func TestGUIDPrefersPlexsOwnIDAndFallsBackToTheCustomForm(t *testing.T) {
	require.Equal(t, "plex://movie/5d7768b7880197001ec967c6", GUID(MoviesProvider, TypeMovie, "uid-1", "5d7768b7880197001ec967c6"))
	require.Equal(t, "tv.plex.agents.custom.clustarr.movies://movie/uid-1", GUID(MoviesProvider, TypeMovie, "uid-1", ""))
	require.Equal(t, "plex://season/abc", GUID(TVProvider, TypeSeason, "s-1-s02", "abc"))
	require.Equal(t, "tv.plex.agents.custom.clustarr.tv://episode/e-1", GUID(TVProvider, TypeEpisode, "e-1", ""))
}
```

`resolve_test.go`, with the fixtures `test/data/clustarr/{mediafile,movie,series,episode}.yaml` (`loadFixture` as `fields_test.go` loads them; copy its helper, since it is unexported in another package):

```go
func TestAMovieFileResolvesToAOneNodeChain(t *testing.T) {
	file, movie := fixture(t, "mediafile-movie.yaml"), fixture(t, "movie.yaml")
	it, ok := Resolve(file, lookupOf(movie))
	require.True(t, ok)
	require.Len(t, it.Chain, 1)
	require.Equal(t, TypeMovie, it.Chain[0].Type)
	require.Equal(t, MoviesProvider, it.Provider)
	require.Equal(t, GUID(MoviesProvider, TypeMovie, string(movie.GetUID()), plexIDOf(movie)), it.Chain[0].GUID)
	require.NotEmpty(t, it.Chain[0].Title)
}

func TestAnEpisodeFileResolvesShowSeasonEpisode(t *testing.T) {
	file, ep, series := fixture(t, "mediafile.yaml"), fixture(t, "episode.yaml"), fixture(t, "series.yaml")
	it, ok := Resolve(file, lookupOf(ep, series))
	require.True(t, ok)
	require.Len(t, it.Chain, 3)
	require.Equal(t, []ItemType{TypeShow, TypeSeason, TypeEpisode}, []ItemType{it.Chain[0].Type, it.Chain[1].Type, it.Chain[2].Type})
	require.Equal(t, int32(1), it.Chain[1].Index, "season number")
	require.Equal(t, int32(1), it.Chain[2].Index, "episode number")
	require.Equal(t, GUID(TVProvider, TypeSeason, string(series.GetUID())+"-s01", ""), it.Chain[1].GUID, "no plexSeasons on the fixture: custom season guid")
}

func TestAFileWhoseItemIsNotCachedYetDoesNotResolve(t *testing.T) {
	file := fixture(t, "mediafile.yaml")
	_, ok := Resolve(file, lookupOf())
	require.False(t, ok)
}
```

`fixture`, `lookupOf` (a map keyed by `gvr.Resource+"/"+namespace+"/"+name` over the objects' own GVK) and `plexIDOf` (reads `status.metadata.externalIDs.plex`) are small test helpers in `resolve_test.go`. Add `test/data/clustarr/mediafile-movie.yaml` by copying `mediafile.yaml` with `spec.mediaRef.kind: Movie` and the movie fixture's name.

In `fields_test.go` add:

```go
func TestTrimKeepsTheEpisodeNumbersAndPlexID(t *testing.T) {
	ep := loadFixture(t, "episode.yaml")
	unstructured.SetNestedField(ep.Object, "5d9c0b1e2f3a4b5c6d7e8f90", "status", "plexID")
	out, err := Trim(ep)
	require.NoError(t, err)
	u := out.(*unstructured.Unstructured)
	for _, p := range [][]string{{"spec", "seasonNumber"}, {"spec", "episodeNumber"}, {"status", "plexID"}} {
		_, found, _ := unstructured.NestedFieldNoCopy(u.Object, p...)
		require.True(t, found, strings.Join(p, "."))
	}
}
```

- [ ] **Step 2: Run:** `go test ./pkg/plexproject/ ./pkg/clustarrwatch/ -run 'TestGUID|TestAMovie|TestAnEpisode|TestAFileWhose|TestTrimKeeps'` — FAIL to compile.

- [ ] **Step 3: Implement.** `guid.go`:

```go
// Package plexproject writes Plex's library rows as a projection of
// clustarr's MediaFiles (spec 2026-10-15). Nothing here runs Plex's scanner
// or analyzer; every row comes from what clustarr already recorded.
package plexproject

const (
	MoviesProvider = "tv.plex.agents.custom.clustarr.movies"
	TVProvider     = "tv.plex.agents.custom.clustarr.tv"
)

type ItemType int

const (
	TypeMovie   ItemType = 1
	TypeShow    ItemType = 2
	TypeSeason  ItemType = 3
	TypeEpisode ItemType = 4
)

var typeNames = map[ItemType]string{TypeMovie: "movie", TypeShow: "show", TypeSeason: "season", TypeEpisode: "episode"}

// GUID is the one guid rule (spec §4): Plex's own id when clustarr holds
// it, since that is what Plex stored when it matched through the provider
// with plexGuids on, else the provider's custom form, which is what Plex
// stored before the id was known. Every lookup and insert goes through it.
func GUID(provider string, t ItemType, ratingKey, plexID string) string {
	if plexID != "" {
		return "plex://" + typeNames[t] + "/" + plexID
	}
	return provider + "://" + typeNames[t] + "/" + ratingKey
}
```

`resolve.go`: read `spec.mediaRef.kind` and `.name` from the file; for `Movie` look the movie up, build one `Node` from `status.metadata.title` (fallback `status.title`), `year`, `externalIDs.plex`, `metadata.creationTimestamp` as `AddedAt`; for `Episode` look the episode up, then its `spec.seriesRef` series, and build show (series title/year/plex id), season (`spec.seasonNumber`; plex id from `status.metadata.plexSeasons[]` whose `number` matches, else the custom `<seriesUID>-s%02d` key), episode (`spec.episodeNumber`, `status.title`, `status.plexID`). `TitleSort` is the title with a leading "The ", "A " or "An " removed and lower-cased, the way Plex sorts. `OriginalLanguage` comes from the movie's or series' `status.metadata.originalLanguage`. `Trim` in `fields.go`: add `[]string{"spec","seasonNumber"}, {"spec","episodeNumber"}, {"status","plexID"}` to the kept paths; make `Guid` call `plexproject.GUID(providerIdentifier, typeOf(plexType), uid, "")` so its callers keep working until Task 10 replaces them.

- [ ] **Step 4: Run** the two packages' tests — PASS. Falsify: make `GUID` ignore `plexID`; `TestGUIDPrefers…` fails; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexproject test/data/clustarr/mediafile-movie.yaml pkg/clustarrwatch/fields.go pkg/clustarrwatch/fields_test.go
git commit -m "plexproject: the guid rule and a MediaFile's resolved item chain; Trim keeps the episode numbers and plexID" -- pkg/plexproject test/data/clustarr pkg/clustarrwatch
```

---

### Task 6: `plexproject` item chain in the database

**Files:**
- Create: `pkg/plexproject/items.go`, `pkg/plexproject/dbtest_test.go` (the copied `plexDB` helper plus `newSection`)
- Test: `pkg/plexproject/items_test.go`

**Interfaces:**
- Consumes: `Item`, `Node`, `GUID` (Task 5); `pgx.Tx`.
- Produces:
  ```go
  // Refresher is told the metadata_items id of a row Plex must fill
  // (spec §6 step 2); the watcher queues PUT /library/metadata/{id}/refresh.
  type Refresher interface { NeedsRefresh(itemID int64) }
  // EnsureChain returns the metadata_items id of the chain's last node,
  // inserting any missing node top down under section. An inserted node
  // is reported to refresh.
  func EnsureChain(ctx context.Context, tx pgx.Tx, section int64, it Item, now time.Time, refresh Refresher) (int64, error)
  // ReKey moves a node from the custom guid to its Plex guid once the id
  // is known (Review Focus 1); a no-op when the row already carries it.
  func ReKey(ctx context.Context, tx pgx.Tx, section int64, from, to string) error
  ```

- [ ] **Step 1: Write the DB helper and the failing tests.** `dbtest_test.go` copies `plexDB` from `pkg/plexseed/seeder_test.go` (database name prefix `plexproject_`) and adds:

```go
// newSection inserts a library section and one location, returning both ids.
func newSection(t *testing.T, conn *pgx.Conn, name string, typ int, root string) (section, location int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO library_sections (name, section_type, agent, scanner, language, created_at, updated_at, uuid)
		VALUES ($1, $2, 'tv.plex.agents.custom.clustarr.movies', 'Plex Movie', 'en', now(), now(), gen_random_uuid()::text) RETURNING id`, name, typ).Scan(&section))
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO section_locations (library_section_id, root_path, available, created_at, updated_at)
		VALUES ($1, $2, true, now(), now()) RETURNING id`, section, root).Scan(&location))
	return section, location
}
```

(Match `library_sections`' NOT NULL columns against `hack/plex-postgresql/schema/plex_schema.sql`; add any the insert is missing.) `items_test.go`:

```go
func movieItem(uid, plexID, title string) Item {
	return Item{Provider: MoviesProvider, Chain: []Node{{Type: TypeMovie, GUID: GUID(MoviesProvider, TypeMovie, uid, plexID), Title: title, TitleSort: strings.ToLower(title), Year: 1995}}}
}

func TestAMissingMovieRowIsInsertedOnceAndQueuedForRefresh(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	now := time.Unix(1790800000, 0)
	id1 := ensure(t, conn, section, movieItem("u1", "5d7768b7880197001ec967c6", "Heat"), now, r)
	id2 := ensure(t, conn, section, movieItem("u1", "5d7768b7880197001ec967c6", "Heat"), now, r)
	require.Equal(t, id1, id2)
	require.Equal(t, []int64{id1}, r.ids, "inserted once, refreshed once")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE guid = $1 AND library_section_id = $2 AND metadata_type = 1 AND title = 'Heat' AND year = 1995", "plex://movie/5d7768b7880197001ec967c6", section))
}

func TestAnEpisodeChainIsInsertedTopDownWithParents(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	it := Item{Provider: TVProvider, Chain: []Node{
		{Type: TypeShow, GUID: GUID(TVProvider, TypeShow, "s1", "aaaaaaaaaaaaaaaaaaaaaaaa"), Title: "Show", TitleSort: "show", Year: 2010},
		{Type: TypeSeason, GUID: GUID(TVProvider, TypeSeason, "s1-s01", ""), Title: "Season 1", Index: 1},
		{Type: TypeEpisode, GUID: GUID(TVProvider, TypeEpisode, "e1", ""), Title: "Pilot", Index: 1},
	}}
	r := &refreshes{}
	epID := ensure(t, conn, section, it, time.Unix(1790800000, 0), r)
	var seasonID, showID int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT parent_id FROM metadata_items WHERE id = $1", epID).Scan(&seasonID))
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT parent_id FROM metadata_items WHERE id = $1", seasonID).Scan(&showID))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND metadata_type = 2 AND parent_id IS NULL", showID))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND metadata_type = 3 AND \"index\" = 1", seasonID))
	require.Len(t, r.ids, 3)
}

func TestAnExistingRowsDescriptiveFieldsAreLeftToPlex(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	id := ensure(t, conn, section, movieItem("u2", "", "Heat"), time.Unix(1790800000, 0), r)
	_, err := conn.Exec(context.Background(), "UPDATE metadata_items SET title = 'Heat (edited)', summary = 'by plex' WHERE id = $1", id)
	require.NoError(t, err)
	ensure(t, conn, section, movieItem("u2", "", "Heat"), time.Unix(1790800000, 0), r)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND title = 'Heat (edited)' AND summary = 'by plex'", id))
	require.Len(t, r.ids, 1)
}

func TestAnItemThatGainsAPlexIDIsReKeyed(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	id := ensure(t, conn, section, movieItem("u3", "", "Heat"), time.Unix(1790800000, 0), r)
	tx, err := conn.Begin(context.Background())
	require.NoError(t, err)
	require.NoError(t, ReKey(context.Background(), tx, section, GUID(MoviesProvider, TypeMovie, "u3", ""), GUID(MoviesProvider, TypeMovie, "u3", "5d7768b7880197001ec967c6")))
	require.NoError(t, tx.Commit(context.Background()))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = 'plex://movie/5d7768b7880197001ec967c6'", id))
	again := ensure(t, conn, section, movieItem("u3", "5d7768b7880197001ec967c6", "Heat"), time.Unix(1790800000, 0), r)
	require.Equal(t, id, again, "the re-keyed row is found by its new guid, not inserted again")
}

type refreshes struct{ ids []int64 }

func (r *refreshes) NeedsRefresh(id int64) { r.ids = append(r.ids, id) }

func ensure(t *testing.T, conn *pgx.Conn, section int64, it Item, now time.Time, r Refresher) int64 {
	t.Helper()
	tx, err := conn.Begin(context.Background())
	require.NoError(t, err)
	id, err := EnsureChain(context.Background(), tx, section, it, now, r)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(context.Background()))
	return id
}
```

- [ ] **Step 2: Run:** `CLUSTERPLEX_TEST_POSTGRES_DSN=… go test ./pkg/plexproject/ -run 'TestAMissing|TestAnEpisodeChain|TestAnExisting|TestAnItemThatGains'` — FAIL to compile.

- [ ] **Step 3: Implement** `items.go`:

```go
// EnsureChain walks it.Chain top down, finding each node by guid within
// section and inserting a missing one under its parent. An inserted row
// carries only what the structure needs (spec §6 step 2); Plex fills the
// rest when the watcher asks it to refresh the id.
func EnsureChain(ctx context.Context, tx pgx.Tx, section int64, it Item, now time.Time, refresh Refresher) (int64, error) {
	var parent *int64
	for _, n := range it.Chain {
		id, err := findItem(ctx, tx, section, n.GUID)
		if errors.Is(err, pgx.ErrNoRows) {
			id, err = insertItem(ctx, tx, section, parent, n, now)
			if err == nil && refresh != nil {
				refresh.NeedsRefresh(id)
			}
		}
		if err != nil {
			return 0, fmt.Errorf("plexproject: %s %q: %w", typeNames[n.Type], n.GUID, err)
		}
		parent = &id
	}
	return *parent, nil
}

func findItem(ctx context.Context, tx pgx.Tx, section int64, guid string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL ORDER BY id LIMIT 1`, section, guid).Scan(&id)
	return id, err
}

func insertItem(ctx context.Context, tx pgx.Tx, section int64, parent *int64, n Node, now time.Time) (int64, error) {
	var id int64
	var year, index any
	if n.Year != 0 {
		year = n.Year
	}
	if n.Type == TypeSeason || n.Type == TypeEpisode {
		index = n.Index
	}
	added := n.AddedAt
	if added.IsZero() {
		added = now
	}
	err := tx.QueryRow(ctx, `INSERT INTO metadata_items
		(library_section_id, parent_id, metadata_type, guid, media_item_count, title, title_sort, "index", year,
		 added_at, created_at, updated_at, refreshed_at, extra_data)
		VALUES ($1, $2, $3, $4, 0, $5, $6, $7, $8, $9, $10, $10, NULL, '')
		RETURNING id`,
		section, parent, int(n.Type), n.GUID, n.Title, n.TitleSort, index, year, added, now).Scan(&id)
	return id, err
}

// ReKey renames a node's guid in place. Watched state in
// metadata_item_settings is keyed by guid too, so it is moved with it.
func ReKey(ctx context.Context, tx pgx.Tx, section int64, from, to string) error {
	if from == to {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE metadata_items SET guid = $3, updated_at = now() WHERE library_section_id = $1 AND guid = $2`, section, from, to); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE metadata_item_settings SET guid = $2 WHERE guid = $1`, from, to)
	return err
}
```

If Task 1's capture found columns Plex needs on an inserted row (`hash`, `changed_at`), add them to `insertItem` here with the values the capture recorded.

- [ ] **Step 4: Run** — PASS. Falsify: drop the `deleted_at IS NULL` from `findItem`, insert a soft-deleted row in `TestAMissingMovieRow…` first and watch it be reused wrongly; add that as a fourth assertion (`a soft-deleted row is not revived`) and restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexproject
git commit -m "plexproject: the item chain in metadata_items -- found by guid, inserted top down, re-keyed when the Plex id arrives" -- pkg/plexproject
```

---

### Task 7: `plexproject` folders and the media row

**Files:**
- Create: `pkg/plexproject/folders.go`, `pkg/plexproject/media.go`, `pkg/plexproject/apply.go`
- Test: `pkg/plexproject/apply_test.go`

**Interfaces:**
- Consumes: `EnsureChain` (Task 6), `plexseed.Seeder.SeedTx`, `plexseed.Target`, `plexseed.Input`, `plexseed.MediaRows` (Task 4).
- Produces:
  ```go
  // Keys in media_items.extra_data beside plexseed's.
  const ( FileUIDKey = "cp:mediaFile"; SeedKeyKey = "cp:seedKey" )
  // Projector writes one library's rows.
  type Projector struct {
      DB      interface{ Begin(context.Context) (pgx.Tx, error) }
      Seeder  *plexseed.Seeder
      Now     func() time.Time
      Refresh Refresher
  }
  // Change is one MediaFile as the projection needs it.
  type Change struct {
      UID      string        // the MediaFile's uid
      PlexPath string        // spec.path through the mapper
      SeedKey  string        // clustarrwatch.SeedKey
      Item     Item
      Input    plexseed.Input // path, size, probeHash, probe, markers, original language
  }
  // Apply writes the file's rows (spec §6) under section and its location.
  func (p *Projector) Apply(ctx context.Context, section, location int64, c Change) error
  // EnsureDirectory returns the directories id for dir under location's root,
  // creating each missing element.
  func EnsureDirectory(ctx context.Context, tx pgx.Tx, section int64, root, dir string, now time.Time) (int64, error)
  ```

- [ ] **Step 1: Write the failing tests** in `apply_test.go` (the `plexDB`, `newSection`, `count` helpers from Task 6):

```go
func change(uid, path, seedKey string, it Item) Change {
	return Change{UID: uid, PlexPath: path, SeedKey: seedKey, Item: it, Input: plexseed.Input{
		Path: path, SizeBytes: 1234, ProbeHash: "p-" + seedKey,
		Probe: &plexseed.Probe{Container: "mkv", VideoCodec: "hevc", Width: 1920, Height: 1080, RuntimeMillis: 60000,
			Audio: []plexseed.Audio{{Index: 1, Codec: "aac", Channels: 2, Language: "eng"}}},
	}}
}

func projector(conn *pgx.Conn, r Refresher) *Projector {
	return &Projector{DB: conn, Seeder: &plexseed.Seeder{DB: conn, Now: func() time.Time { return time.Unix(1790800000, 0) }},
		Now: func() time.Time { return time.Unix(1790800000, 0) }, Refresh: r}
}

func TestANewFileGetsItemDirectoryPartMediaAndStreams(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM media_parts mp JOIN media_items mi ON mi.id = mp.media_item_id JOIN directories d ON d.id = mp.directory_id
		WHERE mp.file = '/media/movies/Heat (1995)/Heat.mkv' AND mp.size = 1234 AND mp.duration = 60000 AND d.path = 'Heat (1995)' AND d.library_section_id = $1
		AND mi.section_location_id = $2 AND mi.container = 'mkv' AND mi.video_codec = 'hevc' AND mi.width = 1920 AND mi.media_analysis_version = $3
		AND mi.extra_data LIKE '%"cp:mediaFile":"f1"%' AND mi.extra_data LIKE '%"cp:seedKey":"k1"%'`, section, loc, plexseed.MediaAnalysisVersion))
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams ms JOIN media_items mi ON mi.id = ms.media_item_id WHERE mi.extra_data LIKE '%\"cp:mediaFile\":\"f1\"%'"), "video and audio")
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE guid = $1", GUID(MoviesProvider, TypeMovie, "u1", "")))
}

func TestAPathChangeUpdatesTheSameRowInPlace(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f2", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u2", "", "Heat"))))
	var before int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT id FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"f2\"%'").Scan(&before))
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f2", "/media/movies/Heat (1995)/Heat.mp4", "k2", movieItem("u2", "", "Heat"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"f2\"%' AND deleted_at IS NULL"), "one row, not two")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND file = '/media/movies/Heat (1995)/Heat.mp4' AND deleted_at IS NULL", before))
	require.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_parts WHERE file = '/media/movies/Heat (1995)/Heat.mkv'"))
}

func TestARowPlexScannedBeforeIsAdoptedByPath(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	// A row the real scanner made: no cp:mediaFile stamp, Plex's own guid.
	var itemID, mediaID int64
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, created_at, updated_at, added_at) VALUES ($1, 1, $2, 'Heat', now(), now(), now()) RETURNING id`, section, GUID(MoviesProvider, TypeMovie, "u3", "")).Scan(&itemID))
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data) VALUES ($1, $2, $3, now(), now(), '') RETURNING id`, section, loc, itemID).Scan(&mediaID))
	_, err := conn.Exec(context.Background(), `INSERT INTO media_parts (media_item_id, file, size, created_at, updated_at) VALUES ($1, '/media/movies/Heat (1995)/Heat.mkv', 1, now(), now())`, mediaID)
	require.NoError(t, err)
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f3", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u3", "", "Heat"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND extra_data LIKE '%\"cp:mediaFile\":\"f3\"%'", mediaID), "adopted and stamped")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL", itemID), "no second row")
}

func TestAnUnchangedSeedKeyWritesNothing(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	c := change("f4", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u4", "", "Heat"))
	require.NoError(t, p.Apply(context.Background(), section, loc, c))
	var updated time.Time
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT updated_at FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"f4\"%'").Scan(&updated))
	p.Now = func() time.Time { return time.Unix(1790900000, 0) }
	require.NoError(t, p.Apply(context.Background(), section, loc, c))
	var again time.Time
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT updated_at FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"f4\"%'").Scan(&again))
	require.Equal(t, updated, again)
}

func TestEnsureDirectoryBuildsTheParentChainOnce(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	tx, err := conn.Begin(context.Background())
	require.NoError(t, err)
	a, err := EnsureDirectory(context.Background(), tx, section, "/media/tv", "/media/tv/Show (2010)/Season 01", time.Unix(1790800000, 0))
	require.NoError(t, err)
	b, err := EnsureDirectory(context.Background(), tx, section, "/media/tv", "/media/tv/Show (2010)/Season 01", time.Unix(1790800000, 0))
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.NoError(t, tx.Commit(context.Background()))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM directories WHERE library_section_id = $1 AND path = 'Show (2010)' AND parent_directory_id IS NULL", section))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM directories d JOIN directories p ON p.id = d.parent_directory_id WHERE d.library_section_id = $1 AND d.path = 'Show (2010)/Season 01' AND p.path = 'Show (2010)'", section))
}
```

Confirm `directories.path` is relative to the section location (check a live row on talos: `SELECT path FROM plex.directories LIMIT 5`); if Plex stores it differently, change the assertions and the implementation to match the live rows.

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Implement.** `folders.go`:

```go
// EnsureDirectory returns the directories row for dir, creating the chain
// from the section location's root down. Plex stores the path relative to
// the root, one row per element, each pointing at its parent.
func EnsureDirectory(ctx context.Context, tx pgx.Tx, section int64, root, dir string, now time.Time) (int64, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return 0, fmt.Errorf("plexproject: %s is not under %s", dir, root)
	}
	var parent *int64
	var id int64
	acc := ""
	for _, el := range strings.Split(filepath.ToSlash(rel), "/") {
		if el == "" || el == "." {
			continue
		}
		acc = path.Join(acc, el)
		err := tx.QueryRow(ctx, `SELECT id FROM directories WHERE library_section_id = $1 AND path = $2 AND deleted_at IS NULL LIMIT 1`, section, acc).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO directories (library_section_id, parent_directory_id, path, created_at, updated_at) VALUES ($1, $2, $3, $4, $4) RETURNING id`, section, parent, acc, now).Scan(&id)
		}
		if err != nil {
			return 0, err
		}
		parent = &id
	}
	if parent == nil {
		return 0, fmt.Errorf("plexproject: %s is the root itself", dir)
	}
	return id, nil
}
```

`media.go`:

```go
const (
	FileUIDKey = "cp:mediaFile"
	SeedKeyKey = "cp:seedKey"
)

// found is a media row located by UID or adopted by path.
type found struct {
	media, part int64
	seedKey     string
}

func findMedia(ctx context.Context, tx pgx.Tx, section int64, uid, plexPath string) (found, bool, error) {
	var f found
	var extra string
	err := tx.QueryRow(ctx, `SELECT mi.id, mp.id, mi.extra_data FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
		WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mi.extra_data LIKE $2 ORDER BY mi.id LIMIT 1`,
		section, `%"`+FileUIDKey+`":"`+uid+`"%`).Scan(&f.media, &f.part, &extra)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT mi.id, mp.id, mi.extra_data FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
			WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mp.deleted_at IS NULL AND mp.file = $2
			AND mi.extra_data NOT LIKE '%"cp:mediaFile":%' ORDER BY mi.id LIMIT 1`, section, plexPath).Scan(&f.media, &f.part, &extra)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	f.seedKey = extraValue(extra, SeedKeyKey)
	return f, true, nil
}

// extraValue reads one key of plexseed's JSON extra_data.
func extraValue(extra, key string) string {
	var m map[string]string
	if json.Unmarshal([]byte(extra), &m) != nil {
		return ""
	}
	return m[key]
}

// upsertMedia writes the media_items and media_parts rows for c, stamped
// with its UID and seed key, and returns the media_items id. plexseed owns
// the codec columns and the streams through SeedTx, so this writes only
// identity, placement and size; SeedTx follows in the same transaction.
func upsertMedia(ctx context.Context, tx pgx.Tx, section, location, item, dir int64, f found, have bool, c Change, now time.Time) (plexseed.Target, error) {
	row, _ := plexseed.MediaRows(derefProbe(c.Input.Probe), c.Input.ProbeHash, c.Input.OriginalLanguage)
	extra := plexseed.ExtraData(map[string]string{plexseed.SourceKey: plexseed.SourceValue, plexseed.ProbeHashKey: c.Input.ProbeHash, FileUIDKey: c.UID, SeedKeyKey: c.SeedKey})
	if !have {
		if err := tx.QueryRow(ctx, `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data, media_analysis_version)
			VALUES ($1, $2, $3, $4, $4, $5, $6) RETURNING id`, section, location, item, now, extra, row.AnalysisVersion).Scan(&f.media); err != nil {
			return plexseed.Target{}, err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO media_parts (media_item_id, directory_id, file, "index", size, duration, created_at, updated_at, extra_data)
			VALUES ($1, $2, $3, 0, $4, $5, $6, $6, '') RETURNING id`, f.media, dir, c.PlexPath, c.Input.SizeBytes, row.Duration, now).Scan(&f.part); err != nil {
			return plexseed.Target{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE media_items SET section_location_id = $2, metadata_item_id = $3, updated_at = $4, extra_data = $5, media_analysis_version = $6, deleted_at = NULL WHERE id = $1`,
			f.media, location, item, now, extra, row.AnalysisVersion); err != nil {
			return plexseed.Target{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE media_parts SET directory_id = $2, file = $3, size = $4, duration = $5, updated_at = $6, deleted_at = NULL WHERE id = $1`,
			f.part, dir, c.PlexPath, c.Input.SizeBytes, row.Duration, now); err != nil {
			return plexseed.Target{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE metadata_items SET media_item_count = (SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL) WHERE id = $1`, item); err != nil {
		return plexseed.Target{}, err
	}
	return plexseed.Target{Part: f.part, Media: f.media, Item: item}, nil
}

func derefProbe(p *plexseed.Probe) plexseed.Probe {
	if p == nil {
		return plexseed.Probe{}
	}
	return *p
}
```

`apply.go`:

```go
// Apply is spec §6: one file, one transaction. Steps 1 (resolve) and the
// mapping happen in the caller; here the chain, the folder, the media row
// and its streams and markers are written, in that order.
func (p *Projector) Apply(ctx context.Context, section, location int64, c Change) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := p.Now()

	f, have, err := findMedia(ctx, tx, section, c.UID, c.PlexPath)
	if err != nil {
		return fmt.Errorf("plexproject: find %s: %w", c.UID, err)
	}
	if have && f.seedKey == c.SeedKey {
		return nil // current: nothing to write (spec §8's "drifted" test)
	}
	item, err := EnsureChain(ctx, tx, section, c.Item, now, p.Refresh)
	if err != nil {
		return err
	}
	root, err := locationRoot(ctx, tx, location)
	if err != nil {
		return err
	}
	dir, err := EnsureDirectory(ctx, tx, section, root, path.Dir(c.PlexPath), now)
	if err != nil {
		return err
	}
	target, err := upsertMedia(ctx, tx, section, location, item, dir, f, have, c, now)
	if err != nil {
		return fmt.Errorf("plexproject: media row for %s: %w", c.UID, err)
	}
	if _, err := p.Seeder.SeedTx(ctx, tx, target, c.Input); err != nil {
		return fmt.Errorf("plexproject: seed %s: %w", c.UID, err)
	}
	return tx.Commit(ctx)
}

func locationRoot(ctx context.Context, tx pgx.Tx, location int64) (string, error) {
	var root string
	err := tx.QueryRow(ctx, `SELECT root_path FROM section_locations WHERE id = $1`, location).Scan(&root)
	return root, err
}
```

`plexseed.Seeder.Libraries` is unused on this path (the target is given), so `SeedTx` must not consult it; `seedMedia`'s "every stream is ours and the probe hash changed → refill" rule stays and is what a path-only change exercises (same probe hash, streams untouched).

- [ ] **Step 4: Run** `go test ./pkg/plexproject/` — PASS. Falsify: remove the `have && f.seedKey == c.SeedKey` early return, watch `TestAnUnchangedSeedKeyWritesNothing` fail; restore. Remove the path fallback in `findMedia`, watch `TestARowPlexScannedBeforeIsAdoptedByPath` fail; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexproject
git commit -m "plexproject: folders, and the media row keyed by MediaFile UID -- inserted, adopted by path, or updated in place" -- pkg/plexproject
```

---

### Task 8: `plexproject` delete

**Files:**
- Create: `pkg/plexproject/delete.go`
- Test: `pkg/plexproject/delete_test.go`

**Interfaces:**
- Produces: `func (p *Projector) Delete(ctx context.Context, section int64, uid string) error` — soft-deletes the media row and its parts, then any item up the chain left with no live media (spec §7). Unknown UID is not an error.

- [ ] **Step 1: Write the failing tests:**

```go
func TestDeletingTheOnlyFileTrashesTheMediaAndTheChain(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	it := episodeItem("s1", 1, "e1", 1)
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f1", "/media/tv/Show (2010)/Season 01/E01.mkv", "k1", it)))
	require.NoError(t, p.Delete(context.Background(), section, "f1"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"f1\"%' AND deleted_at IS NOT NULL"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts mp JOIN media_items mi ON mi.id = mp.media_item_id WHERE mi.extra_data LIKE '%\"cp:mediaFile\":\"f1\"%' AND mp.deleted_at IS NOT NULL"))
	for _, guid := range []string{it.Chain[2].GUID, it.Chain[1].GUID, it.Chain[0].GUID} {
		require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE guid = $1 AND deleted_at IS NOT NULL", guid), guid)
	}
}

func TestDeletingOneOfTwoVersionsKeepsTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	it := movieItem("u1", "", "Heat")
	require.NoError(t, p.Apply(context.Background(), section, loc, change("fa", "/media/movies/Heat (1995)/Heat - 1080p.mkv", "k1", it)))
	require.NoError(t, p.Apply(context.Background(), section, loc, change("fb", "/media/movies/Heat (1995)/Heat - 2160p.mkv", "k1", it)))
	require.NoError(t, p.Delete(context.Background(), section, "fa"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE guid = $1 AND deleted_at IS NULL", it.Chain[0].GUID))
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE guid = $1", it.Chain[0].GUID))
}

func TestDeletingAnotherEpisodeOfTheSeasonKeepsShowAndSeason(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f1", "/media/tv/Show (2010)/Season 01/E01.mkv", "k1", episodeItem("s1", 1, "e1", 1))))
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f2", "/media/tv/Show (2010)/Season 01/E02.mkv", "k1", episodeItem("s1", 1, "e2", 2))))
	require.NoError(t, p.Delete(context.Background(), section, "f1"))
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND metadata_type IN (2, 3) AND deleted_at IS NULL", section))
}

func TestDeletingAnUnknownUIDIsNotAnError(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	require.NoError(t, projector(conn, &refreshes{}).Delete(context.Background(), section, "nobody"))
}

// episodeItem is a show/season/episode chain for tests.
func episodeItem(seriesUID string, season int32, epUID string, ep int32) Item {
	return Item{Provider: TVProvider, Chain: []Node{
		{Type: TypeShow, GUID: GUID(TVProvider, TypeShow, seriesUID, ""), Title: "Show", TitleSort: "show", Year: 2010},
		{Type: TypeSeason, GUID: GUID(TVProvider, TypeSeason, fmt.Sprintf("%s-s%02d", seriesUID, season), ""), Title: fmt.Sprintf("Season %d", season), Index: season},
		{Type: TypeEpisode, GUID: GUID(TVProvider, TypeEpisode, epUID, ""), Title: "Episode", Index: ep},
	}}
}
```

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Implement** `delete.go`:

```go
// Delete is spec §7: the media row and its parts are soft-deleted the way
// Plex's scanner does it, and every item up the chain that is left with no
// live media follows, so the empty-trash CronJob can empty them later.
func (p *Projector) Delete(ctx context.Context, section int64, uid string) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := p.Now()

	var media, item int64
	err = tx.QueryRow(ctx, `SELECT id, metadata_item_id FROM media_items WHERE library_section_id = $1 AND deleted_at IS NULL AND extra_data LIKE $2 ORDER BY id LIMIT 1`,
		section, `%"`+FileUIDKey+`":"`+uid+`"%`).Scan(&media, &item)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_parts SET deleted_at = $2, updated_at = $2 WHERE media_item_id = $1 AND deleted_at IS NULL`, media, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_items SET deleted_at = $2, updated_at = $2 WHERE id = $1`, media, now); err != nil {
		return err
	}
	for id := item; id != 0; {
		var parent *int64
		var live int
		if err := tx.QueryRow(ctx, `SELECT parent_id,
			(SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL)
			+ (SELECT count(*) FROM metadata_items WHERE parent_id = $1 AND deleted_at IS NULL)
			FROM metadata_items WHERE id = $1`, id).Scan(&parent, &live); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE metadata_items SET media_item_count = (SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL), updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		if live > 0 {
			break
		}
		if _, err := tx.Exec(ctx, `UPDATE metadata_items SET deleted_at = $2, updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		if parent == nil {
			break
		}
		id = *parent
	}
	return tx.Commit(ctx)
}
```

- [ ] **Step 4: Run** — PASS. Falsify: stop the loop after the first item (never climb), watch `TestDeletingTheOnlyFileTrashesTheMediaAndTheChain` fail on the season guid; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexproject
git commit -m "plexproject: a deleted MediaFile trashes its media row and every item left without one" -- pkg/plexproject
```

---

### Task 9: `plexproject` reconcile

**Files:**
- Create: `pkg/plexproject/reconcile.go`
- Test: `pkg/plexproject/reconcile_test.go`

**Interfaces:**
- Consumes: `Projector.Apply`, `Projector.Delete`, `plexdb.Pool.Query` (Task 2; the projector's `DB` field gains a `Query` method in its interface).
- Produces:
  ```go
  // Reconcile is spec §8 for one section (folder == "") or one folder.
  // want is every MediaFile of the section (or folder) as a Change.
  type Outcome struct { Applied, Deleted, Adopted, Unknown, Failed int }
  func (p *Projector) Reconcile(ctx context.Context, section, location int64, folder string, want []Change) (Outcome, error)
  ```

- [ ] **Step 1: Write the failing tests:**

```go
func TestReconcileAppliesMissingAndDriftedDeletesGoneAndLeavesUnknownAlone(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	// present and current
	require.NoError(t, p.Apply(context.Background(), section, loc, change("same", "/media/movies/Same (2000)/Same.mkv", "k1", movieItem("u-same", "", "Same"))))
	// present but drifted
	require.NoError(t, p.Apply(context.Background(), section, loc, change("drift", "/media/movies/Drift (2001)/Drift.mkv", "k1", movieItem("u-drift", "", "Drift"))))
	// gone from clustarr
	require.NoError(t, p.Apply(context.Background(), section, loc, change("gone", "/media/movies/Gone (2002)/Gone.mkv", "k1", movieItem("u-gone", "", "Gone"))))
	// a row Plex scanned that clustarr does not know: no stamp, unmatched path
	var itemID, mediaID int64
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, created_at, updated_at, added_at) VALUES ($1, 1, 'plex://movie/ffffffffffffffffffffffff', 'Unknown', now(), now(), now()) RETURNING id`, section).Scan(&itemID))
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data) VALUES ($1, $2, $3, now(), now(), '') RETURNING id`, section, loc, itemID).Scan(&mediaID))
	_, err := conn.Exec(context.Background(), `INSERT INTO media_parts (media_item_id, file, size, created_at, updated_at) VALUES ($1, '/media/movies/Unknown (2003)/Unknown.mkv', 1, now(), now())`, mediaID)
	require.NoError(t, err)

	out, err := p.Reconcile(context.Background(), section, loc, "", []Change{
		change("same", "/media/movies/Same (2000)/Same.mkv", "k1", movieItem("u-same", "", "Same")),
		change("drift", "/media/movies/Drift (2001)/Drift.mkv", "k2", movieItem("u-drift", "", "Drift")),
		change("new", "/media/movies/New (2004)/New.mkv", "k1", movieItem("u-new", "", "New")),
	})
	require.NoError(t, err)
	require.Equal(t, Outcome{Applied: 2, Deleted: 1, Unknown: 1}, out)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"gone\"%' AND deleted_at IS NOT NULL"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE extra_data LIKE '%\"cp:seedKey\":\"k2\"%' AND extra_data LIKE '%\"cp:mediaFile\":\"drift\"%'"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL", mediaID), "unknown row untouched")
}

func TestReconcileOfAFolderTouchesOnlyThatFolder(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("a", "/media/movies/A (2000)/A.mkv", "k1", movieItem("u-a", "", "A"))))
	require.NoError(t, p.Apply(context.Background(), section, loc, change("b", "/media/movies/B (2001)/B.mkv", "k1", movieItem("u-b", "", "B"))))
	out, err := p.Reconcile(context.Background(), section, loc, "/media/movies/A (2000)", nil)
	require.NoError(t, err)
	require.Equal(t, Outcome{Deleted: 1}, out)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE extra_data LIKE '%\"cp:mediaFile\":\"b\"%' AND deleted_at IS NULL"), "B is outside the folder")
}

func TestReconcileAdoptsAStampedLessRowWhosePathClustarrKnows(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	var itemID, mediaID int64
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, created_at, updated_at, added_at) VALUES ($1, 1, $2, 'Heat', now(), now(), now()) RETURNING id`, section, GUID(MoviesProvider, TypeMovie, "u1", "")).Scan(&itemID))
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data) VALUES ($1, $2, $3, now(), now(), '') RETURNING id`, section, loc, itemID).Scan(&mediaID))
	_, err := conn.Exec(context.Background(), `INSERT INTO media_parts (media_item_id, file, size, created_at, updated_at) VALUES ($1, '/media/movies/Heat (1995)/Heat.mkv', 1, now(), now())`, mediaID)
	require.NoError(t, err)
	out, err := projector(conn, &refreshes{}).Reconcile(context.Background(), section, loc, "", []Change{change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))})
	require.NoError(t, err)
	require.Equal(t, Outcome{Applied: 1, Adopted: 1}, out)
}
```

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Implement** `reconcile.go`:

```go
// Outcome counts what a reconcile did.
type Outcome struct{ Applied, Deleted, Adopted, Unknown, Failed int }

// rowState is one live media row of the section: its stamp, if any, and
// its file.
type rowState struct {
	uid, seedKey, file string
}

// Reconcile is spec §8. The database side is every live media row of the
// section (or under folder); the clustarr side is want. Rows whose UID
// names no MediaFile are deleted; MediaFiles with no row or a drifted seed
// key are applied; a stamp-less row whose path a MediaFile names is
// adopted by Apply; a stamp-less row nobody names is counted and left.
func (p *Projector) Reconcile(ctx context.Context, section, location int64, folder string, want []Change) (Outcome, error) {
	var out Outcome
	rows, err := p.DB.Query(ctx, `SELECT mi.extra_data, mp.file FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id
		WHERE mi.library_section_id = $1 AND mi.deleted_at IS NULL AND mp.deleted_at IS NULL AND ($2 = '' OR mp.file LIKE $2 || '/%')`, section, folder)
	if err != nil {
		return out, err
	}
	have := map[string]rowState{}
	byPath := map[string]rowState{}
	for rows.Next() {
		var extra, file string
		if err := rows.Scan(&extra, &file); err != nil {
			rows.Close()
			return out, err
		}
		st := rowState{uid: extraValue(extra, FileUIDKey), seedKey: extraValue(extra, SeedKeyKey), file: file}
		if st.uid != "" {
			have[st.uid] = st
		} else {
			byPath[file] = st
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	wanted := map[string]bool{}
	for _, c := range want {
		if folder != "" && !strings.HasPrefix(c.PlexPath, folder+"/") {
			continue
		}
		wanted[c.UID] = true
		st, ok := have[c.UID]
		if ok && st.seedKey == c.SeedKey {
			continue
		}
		if !ok {
			if _, adopt := byPath[c.PlexPath]; adopt {
				out.Adopted++
				delete(byPath, c.PlexPath)
			}
		}
		if err := p.Apply(ctx, section, location, c); err != nil {
			out.Failed++
			logging(ctx).Warn("plexproject: apply during reconcile", "uid", c.UID, "error", err)
			continue
		}
		out.Applied++
	}
	for uid := range have {
		if wanted[uid] {
			continue
		}
		if err := p.Delete(ctx, section, uid); err != nil {
			out.Failed++
			continue
		}
		out.Deleted++
	}
	out.Unknown = len(byPath)
	return out, nil
}
```

`logging(ctx)` is whatever the repo's slog-from-context helper is (grep `pkg/` for `FromContext`); if there is none, give `Projector` a `Logger *slog.Logger` field and use it. Widen the `Projector.DB` interface to `interface{ Begin(context.Context) (pgx.Tx, error); Query(context.Context, string, ...any) (pgx.Rows, error) }`; `*pgx.Conn` and `*plexdb.Pool` both satisfy it after Task 2.

- [ ] **Step 4: Run** `go test ./pkg/plexproject/` — PASS. Falsify: make the delete loop ignore `wanted`, watch the first test's `same` row get deleted (add `require.Equal(t, 1, count(... "same" ... deleted_at IS NULL))` to pin it); restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexproject
git commit -m "plexproject: reconcile a section or folder against clustarr's MediaFiles -- apply, delete, adopt, leave the unknown" -- pkg/plexproject
```

---

### Task 10: The watcher feeds the projection

**Files:**
- Modify: `pkg/clustarrwatch/watcher.go`, `pkg/clustarrwatch/fields.go`, `pkg/clustarrwatch/scheduler.go`
- Create: `pkg/clustarrwatch/project.go`
- Test: `pkg/clustarrwatch/project_test.go`, `pkg/clustarrwatch/watcher_test.go`

**Interfaces:**
- Consumes: `plexproject.Projector`, `plexproject.Change`, `plexproject.Resolve`, `plexproject.Lookup`, `SeedInputOf`, `SeedKey`, `OriginalLanguageOf`, `Mapper.Map`, `sectionFor`.
- Produces on `Watcher`:
  ```go
  // Projection, when set, replaces Seed and the folder refreshes for
  // provisioned sections (spec §5): every MediaFile change is applied to
  // Plex's rows, a deleted one is removed, and the resync reconciles.
  Projection *plexproject.Projector
  // Sections maps a provisioned section's Plex key to its database id and
  // location id; a section not in it is not provisioned (Task 11 fills it).
  Sections func() map[string]SectionIDs
  type SectionIDs struct { Section, Location int64 }
  // ReconcileSection runs spec §8 for one provisioned section key, or one
  // folder of it, on the holder's queue; ok is false when the key is not
  // provisioned. It is what the scan route calls (Task 12).
  func (w *Watcher) ReconcileSection(ctx context.Context, key, folder string) (plexproject.Outcome, bool, error)
  ```

- [ ] **Step 1: Write the failing tests.** `project_test.go` uses the fake dynamic client and the fake Plex the existing `watcher_test.go` builds (copy its `newWatcher` helper usage), with a `recordingProjector` that implements the same methods as `*plexproject.Projector` behind a small interface added to the watcher (`projector` with `Apply`, `Delete`, `Reconcile`); the real `*plexproject.Projector` satisfies it.

```go
func TestAMediaFileAddIsAppliedToItsSectionNotScanned(t *testing.T) {
	w, dyn, plex := newWatcherWithProjection(t) // Sections returns {"2": {Section: 2, Location: 20}} for /media/movies
	addObject(t, dyn, loadFixture(t, "movie.yaml"))
	addObject(t, dyn, loadFixture(t, "mediafile-movie.yaml")) // spec.path under /data/media/movies, mapped to /media/movies
	require.Eventually(t, func() bool { return len(w.proj.applied) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(2), w.proj.applied[0].section)
	require.Equal(t, "/media/movies/Heat (1995)/Heat.mkv", w.proj.applied[0].change.PlexPath)
	require.Empty(t, plex.refreshCalls, "no folder refresh for a provisioned section")
}

func TestAMediaFileDeleteIsRemoved(t *testing.T) {
	w, dyn, _ := newWatcherWithProjection(t)
	addObject(t, dyn, loadFixture(t, "movie.yaml"))
	f := loadFixture(t, "mediafile-movie.yaml")
	addObject(t, dyn, f)
	require.Eventually(t, func() bool { return len(w.proj.applied) == 1 }, 5*time.Second, 10*time.Millisecond)
	deleteObject(t, dyn, f)
	require.Eventually(t, func() bool { return len(w.proj.deleted) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, string(f.GetUID()), w.proj.deleted[0].uid)
}

func TestAFileWhoseItemIsNotThereYetWaitsForIt(t *testing.T) {
	w, dyn, _ := newWatcherWithProjection(t)
	f := loadFixture(t, "mediafile-movie.yaml")
	addObject(t, dyn, f)
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, w.proj.applied, "no item: nothing applied, never guessed")
	addObject(t, dyn, loadFixture(t, "movie.yaml"))
	require.Eventually(t, func() bool { return len(w.proj.applied) == 1 }, 5*time.Second, 10*time.Millisecond)
}

func TestAnUnmappedFileIsCountedNotProjected(t *testing.T) {
	w, dyn, _ := newWatcherWithProjection(t)
	addObject(t, dyn, loadFixture(t, "movie.yaml"))
	f := loadFixture(t, "mediafile-movie.yaml")
	unstructured.SetNestedField(f.Object, "/elsewhere/Heat.mkv", "spec", "path")
	addObject(t, dyn, f)
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, w.proj.applied)
	require.Equal(t, int64(1), w.Counters.Unmappable.Load())
}

func TestAnItemsPlexIDArrivingRefreshesAndReKeys(t *testing.T) {
	w, dyn, plex := newWatcherWithProjection(t)
	m := loadFixture(t, "movie.yaml")
	unstructured.RemoveNestedField(m.Object, "status", "metadata", "externalIDs", "plex")
	addObject(t, dyn, m)
	addObject(t, dyn, loadFixture(t, "mediafile-movie.yaml"))
	require.Eventually(t, func() bool { return len(w.proj.applied) == 1 }, 5*time.Second, 10*time.Millisecond)
	unstructured.SetNestedField(m.Object, "5d7768b7880197001ec967c6", "status", "metadata", "externalIDs", "plex")
	updateObject(t, dyn, m)
	require.Eventually(t, func() bool { return len(w.proj.applied) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "plex://movie/5d7768b7880197001ec967c6", w.proj.applied[1].change.Item.Chain[0].GUID)
}

func TestTheResyncReconcilesEveryProvisionedSection(t *testing.T) {
	w, _, _ := newWatcherWithProjection(t)
	w.SeedResync = 50 * time.Millisecond
	require.Eventually(t, func() bool { return len(w.proj.reconciled) >= 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(2), w.proj.reconciled[0].section)
	require.Equal(t, "", w.proj.reconciled[0].folder)
}
```

`newWatcherWithProjection` starts the watcher with `Projection` set to a `recordingProjector` (fields `applied []struct{section int64; change plexproject.Change}`, `deleted []struct{uid string}`, `reconciled []struct{section int64; folder string}`), `Mapper` mapping `/data/media` → `/media`, `Sections` returning the fixed map, and a fake Plex whose `/library/sections/all` lists section 2 at `/media/movies`. Existing watcher tests that assert a folder refresh after a MediaFile add keep passing because they do not set `Projection`.

- [ ] **Step 2: Run** `go test ./pkg/clustarrwatch/ -run 'Projection|Projected|Applied|Removed|WaitsForIt|ReKeys|Resync'` — FAIL to compile.

- [ ] **Step 3: Implement** `project.go`:

```go
// projector is what the watcher needs of plexproject.Projector.
type projector interface {
	Apply(ctx context.Context, section, location int64, c plexproject.Change) error
	Delete(ctx context.Context, section int64, uid string) error
	Reconcile(ctx context.Context, section, location int64, folder string, want []plexproject.Change) (plexproject.Outcome, error)
}

// SectionIDs are a provisioned section's database ids.
type SectionIDs struct{ Section, Location int64 }

// projectHandler feeds the projection: an add or a changed SeedKey queues
// an apply, a delete queues a removal. The initial list is not skipped --
// the first flush applies every file, which is the first reconcile.
func (w *Watcher) projectHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { w.queueProject(obj, false) },
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(*unstructured.Unstructured)
			n, ok2 := newObj.(*unstructured.Unstructured)
			if ok1 && ok2 && SeedKey(o) == SeedKey(n) {
				return
			}
			w.queueProject(newObj, false)
		},
		DeleteFunc: func(obj any) { w.queueProject(obj, true) },
	}
}

type projectJob struct {
	key    string // namespace/name
	uid    string
	remove bool
	obj    *unstructured.Unstructured
}

func (w *Watcher) queueProject(obj any, remove bool) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	w.projMu.Lock()
	w.projQueue[u.GetNamespace()+"/"+u.GetName()] = projectJob{uid: string(u.GetUID()), remove: remove, obj: u}
	w.projMu.Unlock()
}

// flushProjection applies the queued jobs, one transaction each, through
// the per-section queue (spec §9): everything for one section runs on one
// goroutine, in arrival order, so a reconcile never interleaves.
func (w *Watcher) flushProjection(ctx context.Context) {
	w.projMu.Lock()
	jobs := w.projQueue
	w.projQueue = map[string]projectJob{}
	w.projMu.Unlock()
	for _, j := range jobs {
		w.sectionQueue(j).run(func() { w.projectOne(ctx, j) })
	}
}

func (w *Watcher) projectOne(ctx context.Context, j projectJob) {
	mapped, ok := w.Mapper.Map(pathOf(j.obj))
	if !ok {
		inc(w.Counters.Unmappable)
		return
	}
	s, ok, err := w.sectionFor(mapped)
	if err != nil || !ok {
		inc(w.Counters.Uncovered)
		return
	}
	ids, provisioned := w.Sections()[s.Key]
	if !provisioned {
		w.sched.EnqueueScan(s.Key, path.Dir(mapped)) // not ours: the old path
		return
	}
	if j.remove {
		if err := w.Projection.Delete(ctx, ids.Section, j.uid); err != nil {
			inc(w.Counters.SeedErrors)
			w.Logger.Warn("projection: delete", "file", j.key, "error", err)
		}
		return
	}
	c, ok := w.changeOf(j.obj, mapped)
	if !ok {
		w.requeueProject(j) // item not cached yet: try again next flush
		return
	}
	if err := w.Projection.Apply(ctx, ids.Section, ids.Location, c); err != nil {
		inc(w.Counters.SeedErrors)
		w.Logger.Warn("projection: apply", "file", j.key, "error", err)
		w.retrySeed(j.key) // the existing 1m/5m/30m backoff
		return
	}
	inc(w.Counters.Seeded)
}

// changeOf builds the projection's input for a MediaFile; ok is false when
// its item is not in the caches yet.
func (w *Watcher) changeOf(u *unstructured.Unstructured, mapped string) (plexproject.Change, bool) {
	in, ok := SeedInputOf(u)
	if !ok {
		return plexproject.Change{}, false
	}
	it, ok := plexproject.Resolve(u, w.item)
	if !ok {
		return plexproject.Change{}, false
	}
	in.Path = mapped
	in.OriginalLanguage = it.OriginalLanguage
	return plexproject.Change{UID: string(u.GetUID()), PlexPath: mapped, SeedKey: SeedKey(u), Item: it, Input: in}, true
}

// ReconcileSection is spec §8 for key, or for folder under it, run on the
// section's queue so it never overlaps the apply stream.
func (w *Watcher) ReconcileSection(ctx context.Context, key, folder string) (plexproject.Outcome, bool, error) {
	ids, ok := w.Sections()[key]
	if !ok {
		return plexproject.Outcome{}, false, nil
	}
	var out plexproject.Outcome
	var err error
	w.sectionQueueFor(key).runAndWait(func() {
		want := w.changesUnder(key, folder)
		out, err = w.Projection.Reconcile(ctx, ids.Section, ids.Location, folder, want)
	})
	return out, true, err
}

// changesUnder lists the cached MediaFiles whose mapped path is under the
// section's locations (and folder), as Changes; files whose item is not
// cached are left out, which the reconcile reads as "not wanted yet" --
// never deleted, since their rows (if any) carry a UID the next apply
// will refresh.
func (w *Watcher) changesUnder(key, folder string) []plexproject.Change
```

`w.item` is the existing `item(gvr, ns, name)` lookup (`watcher.go:545`), whose signature must match `plexproject.Lookup`. `changesUnder` iterates `w.files.List()` (the MediaFile informer's store, promoted from the local variable in `Run` to a field `files cache.Store`), maps each path, keeps those `sectionFor` resolves to `key` and, if `folder != ""`, under `folder+"/"`, and builds `changeOf`. **Careful:** a file whose item is not cached is excluded from `want`; `Reconcile` would then delete its row. Fix that in the reconcile call: pass such UIDs as a `keep` set — add `Keep map[string]bool` to `Reconcile`'s inputs? Simpler and what the plan does: `changesUnder` returns the Change with `Item` empty for such files and a flag; extend `plexproject.Change` with `Unresolved bool`, and `Reconcile` treats an unresolved wanted file as present (neither applied nor deleted). Add that branch to `Reconcile` and a test `TestReconcileKeepsARowWhoseItemIsUnresolved` in Task 9's file.

The per-section queue (`sectionQueue`) is a `map[string]*serialQueue` under `projMu`, each a goroutine draining a channel of funcs; `runAndWait` sends a func with a done channel. `Run` wires `projectHandler` instead of `seedHandler` when `Projection != nil`, keeps `fileHandler` for non-provisioned sections only (its `enqueueFile` checks `Sections()` and returns early for a provisioned key), calls `flushProjection` on the tick where `flushSeeds` was, and on the resync tick calls `ReconcileSection(ctx, key, "")` for every key in `Sections()` instead of re-seeding. The item handler's refresh path (`EnqueueRefresh`) uses `plexproject.GUID` with the item's Plex id, which fixes the lost refresh; and an item update whose Plex id changed from "" queues every MediaFile of that item for an apply (so `EnsureChain` finds the custom-guid row missing — add `ReKey` before `EnsureChain` in `Apply`: when `c.Item.Chain[i].GUID` is a `plex://` guid, call `ReKey(ctx, tx, section, customGUIDOf(node), node.GUID)`; `Change` therefore carries both forms per node: add `PriorGUID string` to `Node`, set by `Resolve` to the custom form whenever the Plex id is set).

`Refresher` for the projector is the watcher: `func (w *Watcher) NeedsRefresh(id int64) { w.sched.EnqueueRefreshID(id) }` with a new `Scheduler.EnqueueRefreshID(id int64)` that debounces like `EnqueueRefresh` and calls `Refresh` with the id directly (the existing `Refresh func(ctx, guid string)` becomes `RefreshID func(ctx, id int64) error`, wired to `PMS.RefreshItem`).

- [ ] **Step 4: Run** `go test ./pkg/clustarrwatch/ ./pkg/plexproject/` — PASS (including the old folder-refresh tests, which do not set `Projection`). Falsify: make `projectOne` skip the `provisioned` check so a provisioned section also gets `EnqueueScan`; `TestAMediaFileAddIsAppliedToItsSectionNotScanned` fails on `refreshCalls`; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/clustarrwatch pkg/plexproject
git commit -m "clustarrwatch: feed plexproject from the informer -- apply on change, delete on delete, reconcile on resync, refresh by id" -- pkg/clustarrwatch pkg/plexproject
```

---

### Task 11: Provisioning records section ids; the manager wires the projection

**Files:**
- Modify: `pkg/plex/provision/provision.go` (`Result` gains `Sections map[string]string` title → key), `pkg/plex/api/client.go` (nothing new: `Section.Key` is the id Plex uses in URLs, which is `library_sections.id`)
- Modify: `cmd/manager/leaderwork.go`, `cmd/manager/main.go`
- Test: `pkg/plex/provision/provision_test.go`, `cmd/manager/leaderwork_test.go`

**Interfaces:**
- Produces on `Manager`: `func (m *Manager) provisionedSections() map[string]clustarrwatch.SectionIDs` — key → `{Section: key as int64, Location: section_locations.id for the first location}`; the location id is read once per key from the pool (`SELECT id FROM section_locations WHERE library_section_id = $1 ORDER BY id LIMIT 1`) and cached until provisioning runs again.

- [ ] **Step 1: Write the failing tests.** In `provision_test.go` (next to the existing Run tests with the fake Plex):

```go
func TestRunReportsEveryProvisionedSectionsKey(t *testing.T) {
	p, _ := provisionerWithFakePlex(t, Config{Libraries: []Library{{Name: "Clustarr Movies", Type: "movie", Provider: MoviesProvider, Locations: []string{"/media/movies"}}}})
	res, err := p.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Clustarr Movies": "2"}, res.Sections)
}
```

(`provisionerWithFakePlex` is whatever helper the existing tests use; the fake lists the created section with key "2".) In `leaderwork_test.go`:

```go
func TestProvisionedSectionsFollowTheLastProvisioningRun(t *testing.T) {
	m := newTestManager()
	m.recordSections(map[string]string{"Clustarr Movies": "2"}, func(key string) (int64, error) { return 20, nil })
	require.Equal(t, map[string]clustarrwatch.SectionIDs{"2": {Section: 2, Location: 20}}, m.provisionedSections())
	m.recordSections(map[string]string{}, nil)
	require.Empty(t, m.provisionedSections())
}
```

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Implement.** `provision.go`: after the re-list at `:188`, fill `res.Sections[lib.Name] = s.Key` for every provisioned library found. `cmd/manager`: a `sections atomic.Pointer[map[string]clustarrwatch.SectionIDs]` on `Manager`; `recordSections(byName map[string]string, location func(key string) (int64, error))` converts and stores; `provisionLoop` calls it after each successful `Run` with a `location` that queries `m.pool`; `provisionedSections()` loads the pointer (empty map when nil). In `watchClustarr`, when `m.pool != nil`, set

```go
w.Projection = &plexproject.Projector{DB: m.pool, Seeder: seeder, Now: time.Now, Refresh: w, Logger: log}
w.Sections = m.provisionedSections
```

and keep `w.Seed` unset in that case (the projection seeds). `plexseed.Seeder.Libraries` is still needed by `Seed` for non-projected sections; leave it.

- [ ] **Step 4: Run** `go test ./pkg/plex/provision/ ./cmd/manager/` — PASS.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plex/provision cmd/manager
git commit -m "provision: report each library's section key; the manager wires the projection with the provisioned sections" -- pkg/plex/provision cmd/manager
```

---

### Task 12: The scan route: intercept on any pod, run on the holder

**Files:**
- Create: `pkg/plexscan/route.go`, `pkg/plexscan/holder.go`
- Modify: `pkg/remoteexec/dispatcher.go` (a `Scan ScanRoute` field, consulted before Remux), `cmd/manager/execserver.go` (wire it), `cmd/manager/api.go` or wherever `/api/v1/maintenance/{task}` is served (add `/api/v1/scan`), `cmd/manager/leaderwork.go`
- Test: `pkg/plexscan/route_test.go`, `pkg/remoteexec/dispatcher_test.go`, `cmd/manager/scan_test.go`

**Interfaces:**
- Consumes: `plexscan.Parse` (Task 3), `Watcher.ReconcileSection` (Task 10), `m.provisionedSections` (Task 11), the manager's HTTP API and `podsServingPlex`/`dispatchToPlex` (`cmd/manager/maintenance.go:127-160`).
- Produces:
  ```go
  package plexscan
  // Reconciler runs a scan on the pod that holds the lease.
  type Reconciler interface { ReconcileSection(ctx context.Context, key, folder string) (plexproject.Outcome, bool, error) }
  // Holder says whether this pod holds the lease, and otherwise where the
  // holder's manager API is ("" when nobody holds it).
  type Holder interface { Holder(ctx context.Context) (local bool, addr string, err error) }
  // Route implements remoteexec's ScanRoute: a scan of a provisioned section
  // runs here (holder) or is forwarded (non-holder); anything else is Plex's.
  type Route struct { Provisioned func() map[string]bool; Local Reconciler; Holder Holder; Forward func(ctx context.Context, addr string, req Request) (plexproject.Outcome, error); Logger *slog.Logger }
  func (r *Route) Execute(ctx context.Context, req *pb.ExecRequest, sink remoteexec.Sink) (done bool, err error)
  ```
  and in `remoteexec`: `type ScanRoute interface { Execute(ctx context.Context, req *pb.ExecRequest, sink Sink) (done bool, err error) }` (the same shape as `RemuxRoute`), field `Scan ScanRoute` on `Dispatcher`, consulted when `req.GetTargetBinary() == Scanner` with `const Scanner = "Plex Media Scanner"`.

- [ ] **Step 1: Write the failing tests.** `route_test.go`:

```go
func TestAProvisionedSectionScanRunsHereWhenThisPodHoldsTheLease(t *testing.T) {
	rec := &fakeReconciler{out: plexproject.Outcome{Applied: 3}}
	r := &Route{Provisioned: func() map[string]bool { return map[string]bool{"2": true} }, Local: rec, Holder: fakeHolder{local: true}}
	sink := &collectSink{}
	done, err := r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--scan", "--refresh", "--section", "2"}}, sink)
	require.True(t, done)
	require.NoError(t, err)
	require.Equal(t, []string{"2"}, rec.keys)
	require.Equal(t, int32(0), sink.exit)
	require.Contains(t, sink.stdout(), "applied 3")
}

func TestAFolderScanPassesTheFolder(t *testing.T) {
	rec := &fakeReconciler{}
	r := &Route{Provisioned: func() map[string]bool { return map[string]bool{"2": true} }, Local: rec, Holder: fakeHolder{local: true}}
	_, _ = r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--scan", "--section", "2", "--directory", "/media/movies/Heat (1995)"}}, &collectSink{})
	require.Equal(t, []string{"/media/movies/Heat (1995)"}, rec.folders)
}

func TestAScanOfAnUnprovisionedSectionIsLeftToPlex(t *testing.T) {
	rec := &fakeReconciler{}
	r := &Route{Provisioned: func() map[string]bool { return map[string]bool{"2": true} }, Local: rec, Holder: fakeHolder{local: true}}
	done, _ := r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--scan", "--section", "9"}}, &collectSink{})
	require.False(t, done)
	require.Empty(t, rec.keys)
}

func TestAnAnalyzeIsLeftToPlex(t *testing.T) {
	r := &Route{Provisioned: func() map[string]bool { return map[string]bool{"2": true} }, Local: &fakeReconciler{}, Holder: fakeHolder{local: true}}
	done, _ := r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--analyze", "--item", "5"}}, &collectSink{})
	require.False(t, done)
}

func TestANonHolderForwardsToTheHolder(t *testing.T) {
	var got []string
	r := &Route{Provisioned: func() map[string]bool { return map[string]bool{"2": true} }, Local: &fakeReconciler{}, Holder: fakeHolder{addr: "10.0.0.5:8080"},
		Forward: func(_ context.Context, addr string, req Request) (plexproject.Outcome, error) { got = append(got, addr, req.Section); return plexproject.Outcome{Applied: 1}, nil }}
	sink := &collectSink{}
	done, err := r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--scan", "--section", "2"}}, sink)
	require.True(t, done)
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.5:8080", "2"}, got)
	require.Equal(t, int32(0), sink.exit)
}

func TestAScanWithNoHolderFailsFast(t *testing.T) {
	r := &Route{Provisioned: func() map[string]bool { return map[string]bool{"2": true} }, Local: &fakeReconciler{}, Holder: fakeHolder{}}
	sink := &collectSink{}
	done, _ := r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--scan", "--section", "2"}}, sink)
	require.True(t, done, "ours, even though nobody can run it: Plex must not fall back to its own scanner")
	require.Equal(t, int32(1), sink.exit)
	require.Contains(t, sink.stderr(), "no lease holder")
}
```

`fakeReconciler` records `keys`/`folders` and returns `out`; `fakeHolder{local bool; addr string}` returns them; `collectSink` records `TranscodeLog`s with `stdout()`, `stderr()`, `exit`. In `pkg/remoteexec/dispatcher_test.go` add, beside the Remux routing test:

```go
func TestAScannerRunGoesToTheScanRouteBeforeTheLocalBinary(t *testing.T) {
	scan := &routeStub{done: true}
	d := &Dispatcher{Local: localScanner(t), Scan: scan, Dial: noDial}
	require.NoError(t, d.Execute(context.Background(), &pb.ExecRequest{TargetBinary: Scanner, Args: []string{"--scan", "--section", "2"}}, &collectSink{}))
	require.Equal(t, 1, scan.calls)
}
```

(`routeStub` records calls and returns `done`.) In `cmd/manager/scan_test.go`:

```go
func TestTheScanEndpointRunsOnlyOnTheHolder(t *testing.T) {
	m := newTestManager() // not holding
	rr := httptest.NewRecorder()
	m.apiMux().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/scan?section=2", nil))
	require.Equal(t, http.StatusConflict, rr.Code, "a non-holder refuses so the caller tries the holder")
}
```

- [ ] **Step 2: Run** the three packages — FAIL to compile.

- [ ] **Step 3: Implement.** `remoteexec/dispatcher.go`: add `const Scanner = "Plex Media Scanner"`, the `ScanRoute` interface, the `Scan` field, and before the Remux block:

```go
if d.Scan != nil && req.GetTargetBinary() == Scanner {
	if done, err := d.Scan.Execute(ctx, req, sink); done {
		d.route(req.GetTargetBinary(), "reconcile")
		return err
	}
}
```

`plexscan/route.go`:

```go
// Execute is remoteexec's ScanRoute. A scan of a provisioned section is
// ours wherever it arrives; it runs on the lease holder (spec §9), which
// is this pod or the one the Holder names. Plex reads the exit code as
// the scanner's, so the reconcile's outcome is written to stdout and a
// failure is exit 1 -- never done=false, which would start Plex's own
// scanner on a library this design owns.
func (r *Route) Execute(ctx context.Context, exec *pb.ExecRequest, sink remoteexec.Sink) (bool, error) {
	req, ok := Parse(exec.GetArgs())
	if !ok || !r.Provisioned()[req.Section] {
		return false, nil
	}
	out, err := r.run(ctx, req)
	if err != nil {
		_ = sink.Send(&pb.TranscodeLog{StderrChunk: []byte("clustarr scanner: " + err.Error() + "\n")})
		return true, sink.Send(&pb.TranscodeLog{ExitCode: 1, IsFinished: true})
	}
	_ = sink.Send(&pb.TranscodeLog{StdoutChunk: []byte(fmt.Sprintf("clustarr scanner: section %s%s: applied %d deleted %d adopted %d unknown %d failed %d\n",
		req.Section, folderNote(req), out.Applied, out.Deleted, out.Adopted, out.Unknown, out.Failed))})
	return true, sink.Send(&pb.TranscodeLog{ExitCode: 0, IsFinished: true})
}

func (r *Route) run(ctx context.Context, req Request) (plexproject.Outcome, error) {
	local, addr, err := r.Holder.Holder(ctx)
	if err != nil {
		return plexproject.Outcome{}, err
	}
	switch {
	case local:
		out, _, err := r.Local.ReconcileSection(ctx, req.Section, req.Path)
		return out, err
	case addr == "":
		return plexproject.Outcome{}, errors.New("no lease holder to run the scan")
	default:
		return r.Forward(ctx, addr, req)
	}
}

func folderNote(req Request) string {
	if req.Path == "" {
		return ""
	}
	return " folder " + req.Path
}
```

`plexscan/holder.go` has the HTTP forwarder: `func ForwardHTTP(client *http.Client) func(ctx, addr string, req Request) (plexproject.Outcome, error)` doing `POST http://<addr>/api/v1/scan?section=<key>&path=<folder>` with a 10-minute timeout and decoding the JSON `Outcome`; a 409 means the pod stopped holding meanwhile, returned as an error. In `cmd/manager`: `/api/v1/scan` handler reads the query, checks `m.holdsLease()` (`m.mu`; `m.stopLeader != nil`) and answers 409 when not, else calls `m.watcher.ReconcileSection` (keep the `*clustarrwatch.Watcher` on `Manager` when `watchClustarr` builds it) and writes the `Outcome` as JSON. `Holder` implementation on the manager: `local` from `holdsLease()`; otherwise read the `plex-plextv` Lease (`coordination.k8s.io/v1` through `m.K8sClient`), take `spec.holderIdentity` (a pod name), and resolve it through `podsServingPlex` to the manager API address the maintenance fan-out already uses (`dispatchToPlex`'s target). `execServices` sets `Dispatcher.Scan = &plexscan.Route{Provisioned: m.provisionedKeys, Local: m, Holder: m, Forward: plexscan.ForwardHTTP(m.HTTP), Logger: …}` where `m.provisionedKeys` maps `provisionedSections()` to `map[string]bool`, and `m.ReconcileSection` delegates to the watcher (an error "projection not running" when nil). Non-holders have no watcher, so `provisionedSections()` must work there too: `recordSections` is only called on the holder, so non-holders fall back to listing Plex's sections and matching the provisioned names from config (`m.Config.Provision.LibraryNames()`), cached 5 minutes — put that in `provisionedKeys`.

- [ ] **Step 4: Run** `go test ./pkg/plexscan/ ./pkg/remoteexec/ ./cmd/manager/` — PASS. `make vet lint`. Falsify: return `false, nil` from `Execute` when `Holder` reports nobody; `TestAScanWithNoHolderFailsFast` fails; restore.

- [ ] **Step 5: Commit:**

```bash
git add pkg/plexscan pkg/remoteexec cmd/manager
git commit -m "plexscan: a Plex Media Scanner run for a provisioned section is a reconcile on the lease holder, forwarded from any pod" -- pkg/plexscan pkg/remoteexec cmd/manager
```

---

### Task 13: The `fields.go` contract test against clustarr's CRDs

**Files:**
- Create: `pkg/clustarrwatch/contract_test.go`

- [ ] **Step 1: Write the test.** It reads the CRD YAML from the sibling checkout, skipping when absent, and walks each path in `fields.go` through the schema:

```go
// Every path fields.go reads must exist in clustarr's CRD schema, so a
// renamed field fails here rather than silently projecting nothing
// (spec §12). The CRDs live in the sibling checkout; CI sets CLUSTARR_CRD_DIR.
func TestEveryFieldPathExistsInClustarrsCRDs(t *testing.T) {
	dir := os.Getenv("CLUSTARR_CRD_DIR")
	if dir == "" {
		dir = "../../../clustarr-unify/config/crd/bases"
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no clustarr CRDs at %s (set CLUSTARR_CRD_DIR)", dir)
	}
	for kind, paths := range map[string][][]string{
		"mediafiles": fieldPaths(MediaFiles), "movies": fieldPaths(Movies), "series": fieldPaths(Series), "episodes": fieldPaths(Episodes),
	} {
		schema := crdSchema(t, filepath.Join(dir, "catalog.clustarr.io_"+kind+".yaml"))
		for _, p := range paths {
			require.True(t, schemaHas(schema, p), "%s: %s", kind, strings.Join(p, "."))
		}
	}
}
```

`fieldPaths(gvr)` returns the paths `Trim`, `FileOf`, `SeedInputOf`, `MetadataHash`, `OriginalLanguageOf` and `plexproject.Resolve` read for that kind — refactor `fields.go` so those paths are declared once in a per-kind table (`var kept = map[schema.GroupVersionResource][][]string{…}`) that both `Trim` and this test read. `crdSchema` decodes the YAML and returns `spec.versions[0].schema.openAPIV3Schema`; `schemaHas` descends `properties` by element, treating an `additionalProperties` map (e.g. `externalIDs`) as matching any key and an `items` array as transparent.

- [ ] **Step 2: Run** `go test ./pkg/clustarrwatch/ -run TestEveryFieldPath` — PASS (or fix the path that is wrong). Falsify: add a bogus path to the table, watch it fail, remove.

- [ ] **Step 3: Commit:**

```bash
git add pkg/clustarrwatch
git commit -m "clustarrwatch: every field path is checked against clustarr's CRD schema" -- pkg/clustarrwatch
```

---

### Task 14: ADR, docs and the chart

**Files:**
- Create: `docs/adr/0008-plex-rows-are-a-projection-of-clustarr.md`
- Modify: `docs/adr/0006-seed-analysis-and-markers-into-the-database.md` (status: superseded by 0008 for provisioned sections), `docs/adr/README.md`, `CLAUDE.md`, `README.md`, `charts/cluster-plex/values.yaml` (no new value; document that `plex.clustarr.enabled` now implies the projection), `docs/configuration.md`

- [ ] **Step 1: Write ADR-0008:** context (the scanner's four hops, the trashed version, the double probe), decision (spec §3's table, in prose), consequences (direct writes widen to `metadata_items`, `directories`, `section_locations`; Plex's scanner and analyze never run for provisioned sections; the `analyze` and `deep-analyze` maintenance tasks are harmless there because `media_analysis_version` is at Plex's value, but a CronJob should not schedule them for provisioned sections; the adoption path keeps a library scanned before this ADR). Mark 0006 "Superseded by 0008 for provisioned sections; still the rule for any other section".
- [ ] **Step 2: CLAUDE.md:** replace the scanner paragraph under the analyze gotchas (`CLAUDE.md:159-192`) with the new flow: where the scanner's argv goes, that `Plex Media Scanner.real` runs only for unprovisioned sections, the forwarding to the holder, and the two debugging commands (`kubectl exec … psql -c "SELECT extra_data FROM plex.media_items WHERE extra_data LIKE '%cp:mediaFile%' LIMIT 3"`, and `curl -X POST http://<pod>:8080/api/v1/scan?section=2`).
- [ ] **Step 3: README and docs/configuration.md:** one paragraph each: Scan Library Files is a reconcile; what it logs; that `plex.clustarr.pathMappings` must cover every RootFolder.
- [ ] **Step 4: Commit:**

```bash
git add docs CLAUDE.md README.md charts/cluster-plex/values.yaml
git commit -m "docs: ADR-0008 -- Plex's rows for a provisioned section are a projection of clustarr; the scanner is a reconcile" -- docs CLAUDE.md README.md charts/cluster-plex/values.yaml
```

---

### Task 15: Gate, deploy to talos, prove it end to end

- [ ] **Step 1: Gate:** `make vet lint test` (with `CLUSTERPLEX_TEST_POSTGRES_DSN` set, so the projection tests run) and `make helm-lint`. All green, or fixed.
- [ ] **Step 2: Build and push the image** the way the repository's release path does (`make image` / the GitHub workflow; the tag is the short SHA). Confirm the owner's go-ahead before any deploy; another session may hold the talos release (check with `ListAgents` and ask).
- [ ] **Step 3: Deploy:** `helm get values clustarr -o yaml` from `admin@talos`, set `cluster-plex.image.tag`, `helm upgrade clustarr … -f values.yaml`. Plex pods restart one at a time.
- [ ] **Step 4: Prove, in order, with the live database and Plex Web:**
  1. the first resync adopts every existing row: `SELECT count(*) FROM plex.media_items WHERE extra_data LIKE '%cp:mediaFile%' AND deleted_at IS NULL` equals the number of MediaFiles under the mapped roots, and `Unknown` in the manager's log is the count of files clustarr does not know;
  2. Scan Library Files on Movies finishes within 30 s and the manager logs `applied 0 deleted 0 … unknown N`;
  3. a transcode swap (`.mkv` → `.mp4`) on one episode: the same `media_items.id` carries the new path within a minute, Plex Web shows one version, nothing in the trash;
  4. a deleted MediaFile (delete through clustarr's UI with `records`): the media row is trashed, the episode is trashed, show and season stay;
  5. a new import: the row appears with the custom or `plex://` guid, Plex refreshes it and shows poster and summary;
  6. `kubectl exec` a pod and run `Plex Media Scanner --scan --section 2` by hand: the output is the reconcile line and the exit code 0.
- [ ] **Step 5: Record** what was observed in the capture doc's new "As built" heading and commit it.

---

## Self-review notes

- **Spec coverage:** §4 → Tasks 5, 7, 11; §5 → Tasks 10, 12; §6 → Tasks 6, 7 (and 4 for streams); §7 → Task 8; §8 → Task 9 (plus the unresolved-item case added from Task 10); §9 → Tasks 10 (queue), 12 (holder), 4 (analysis version); §10 → Task 1; §11 → every task's tests plus Task 13 and Task 15; §12 → Task 13; ADR → Task 14.
- **Type consistency:** `plexseed.Target{Part, Media, Item}` (Task 4) is what Task 7's `upsertMedia` returns; `plexproject.Change` (Task 7) gains `Unresolved bool` in Task 10 and `Node` gains `PriorGUID string` — both added where Task 10 says, and `Reconcile` (Task 9) gets the matching branch and test there; `Watcher.ReconcileSection` (Task 10) is the `Reconciler` Task 12 consumes; `SectionIDs` is declared once, in `clustarrwatch`.
- **Review Focus:** 1 → Task 6 and Task 10's `TestAnItemsPlexIDArrivingRefreshesAndReKeys`; 2 → Task 8; 3 → Task 10; 4 → Tasks 3 and 12; 5 → Task 12.
