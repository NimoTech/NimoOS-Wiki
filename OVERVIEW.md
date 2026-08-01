# NimoOS-Wiki

NimoOS's "visible long-term memory" service — maintains a `.wiki.md` navigation map inside the user's storage space, so both the user and the Agent can directly read "what's on the NAS, where it is, what topic it belongs to." Current version `v1.9.0-alpha1` (`common/constants.go`, versioned in lockstep with the rest of the NimoOS suite).

Binds to localhost, forwarded by Gateway, API prefix `/v1/wiki`. See the internal design doc `2026-05-13-wiki-design.md` for details.

---

## Position in the overall architecture

```
                External request (via Gateway, /v1/wiki/*)
                            │
                            ▼
           ┌────────────────────────────────────┐
           │   nimoos-wiki.service (Go,Echo)   │
           │                                    │
           │   Watcher  ─┐                      │
           │   Recon.   ─┼─▶ file_events ─▶ EventProcessor │
           │             │                      ▼          │
           │             │   wiki_nodes ◀── markNearest    │
           │             │      │                          │
           │             │      ▼  (dirty=1)               │
           │             │   WikiWriter ──▶ .wiki.md       │
           │             │                  (Chtimes+rename)│
           │             └◀───── reverse sync ◀── external edit │
           └────────────────────────────────────┘
                       │              ▲
                       ▼              │
          /var/lib/nimoos/wiki/wiki.db (SQLite)
                       │
                       ▼
                  MessageBus
              (Wiki:NodeUpdated /
               Wiki:RecentChanged / ...)
```

Wiki only builds a **navigation map**; **content search** (vector store / full-text index) is implemented by other services in the RAG stack, hooked up via `_internal` interfaces. These interfaces already have real consumers: NimoOS-Parser's WikiConsumer consumes `_internal/file-events`, NimoOS-AI's `wiki_summary_worker` consumes `_internal/needs-summary` / `node-evidence` / `summary`, and NimoOS-Search consumes `_internal/user-roots` — see "Relationship with other services" below.

---

## API routes (`/v1/wiki`)

All public routes go through the JWT middleware, with a **localhost-origin exemption** (determined by `RealIP`; for requests forwarded by Gateway, the RealIP has already been rewritten to the real client IP, so it doesn't hit the local branch). This policy was introduced on 2026-05-14 for the Agent integration, to let the Python Agent connect directly — same pattern as the Photos service. Exception: the `_internal` group is forced through `LocalhostOnly` middleware requiring RealIP=loopback, so it's unreachable even via Gateway.

### Group A — Root management

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/wiki/roots` | List all Roots |
| POST | `/v1/wiki/roots` | Enable a Root: `{path, level, watch_mode?, storage_mode?}` |
| DELETE | `/v1/wiki/roots/:id` | Disable a Root, optionally `?purge_files=true` to delete the on-disk `.wiki.md` |
| POST | `/v1/wiki/roots/:id/rescan` | Force a reconcile scan (clears last_scan_at) |
| GET | `/v1/wiki/candidates` | Enumerate mount-point candidates reported by LocalStorage |

Enabling a Root does a **write test** (creates and deletes `.nimoos-wiki-write-test` under the path); failure returns 409.

> ⚠ `storage_mode=mirror` is **currently not implemented**. `POST /roots` accepts this field and `wiki_roots.storage_mode` does persist it, but `WikiWriter` always writes `.wiki.md` to `nodePath/.wiki.md` — it's never routed to `/var/lib/nimoos/wiki/mirror/<root_id>/...`. There's currently no fallback for non-writable paths — the only options are to give up registering it, or to make the directory writable at the user level first. This gap is listed in the "Implementation status" section below.

### Group B — Wiki content

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/wiki/tree?root_id=&depth=` | Returns the Wiki node skeleton (without summary) |
| GET | `/v1/wiki/node?path=...` | Read a single node (structured JSON, includes etag) |
| GET | `/v1/wiki/raw?path=...` | Read the raw `.wiki.md` text |
| PUT | `/v1/wiki/user-notes?path=...` | Write User Notes, requires `If-Match: <etag>` optimistic locking |
| GET | `/v1/wiki/recent-changes?root_id=&since_ms=&limit=` | Pull recent changes (`since_ms` is the cursor, `limit` defaults to 50, capped at 200); **an empty `root_id` = a global feed across all Roots** (2026-05-25, `FileEventsRepo.ListSince` supports an empty root_id, `service/repo/file_events.go`) |

### Group C — Internal interfaces (localhost only, not registered with Gateway)

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/wiki/_internal/file-events?root_id=&since=&limit=` | For Parser's WikiConsumer to pull file changes; an empty `root_id` = across all Roots (Parser uses a single global cursor) |
| GET | `/v1/wiki/_internal/needs-summary?limit=` | For the AI summary worker to pull the pending-summary node queue, ordered by `last_modified DESC` (`route/v1/internal.go`) |
| GET | `/v1/wiki/_internal/node-evidence?path=&text_limit=&pdf_limit=` | For the summary worker to pick "evidence": from `file_index`, selects text files (≤50KB) / PDFs (≤5MB) / direct children / a sample of skipped items within the node's subtree |
| POST | `/v1/wiki/_internal/summary` | The summary worker reports `{path, ai_label(≤80B), summary(≤600B), based_on_last_modified_ms}`; writes to `wiki_summaries` + `SetAILabel` (the order matters, see below) |
| POST | `/v1/wiki/_internal/index-status` | Parser worker reports parse status (**still a 503 stub**) |
| GET | `/v1/wiki/_internal/user-roots?user_id=` | For NimoOS-Search to query the set of root_ids visible to a user; MVP ignores user_id and returns all enabled roots (a forward-compatible scope interface) |

The `FileEvent` JSON returned by `file-events` uses **snake_case** field names (`root_id` / `detected_at` / `is_dir` ..., explicit tags on `service/repo/models.go`) — this is a cross-repo wire contract with NimoOS-Parser's WikiConsumer (Python), fixed on 2026-05-25, don't remove it.

### Summary pipeline (shipped 2026-05-21)

The `Summary` section is no longer always a placeholder. Data flow: EventProcessor advances `wiki_nodes.last_modified` with `MAX` on file events (`SetDirtyAndTouch`, never regresses on out-of-order events); NimoOS-AI's `wiki_summary_worker` (a separate process, see "Relationship with other services") polls `needs-summary` (condition: ai_label is empty / no summary row / `based_on_last_modified < last_modified`) → gathers evidence via `node-evidence` → LLM generates a summary → `POST /_internal/summary`; on its next flush, WikiWriter pulls the body from `wiki_summaries` and renders it into `.wiki.md`'s `## Summary`.

- **Write-order invariant** (comment in `route/v1/internal.go`): must Upsert `wiki_summaries` **before** `SetAILabel` (the latter sets `dirty=1`); doing it the other way round, WikiWriter could race ahead, read an empty summary, render a blank one, and clear dirty.
- **The freshness key is `based_on_last_modified`** (the `last_modified` snapshot the worker saw when it generated the summary), not `generated_at` — this avoids missing a re-summarize when "the file changed again while summarizing" races.
- `SetAILabel` deliberately **does not touch** `last_modified` (only EventProcessor advances it, on real file events), otherwise it would self-trigger an infinite re-summarize loop (`service/repo/wiki_nodes.go`).
- `ai_label` is already returned by `GET /node` / `GET /tree`; but the `summary` field in the `/node` response still currently returns `null` (`route/v1/wiki.go`) — the summary body only appears in the on-disk `.wiki.md` (readable via `/raw`).

### MessageBus events

| Event | Status | Notes |
|---|---|---|
| `Wiki:NodeUpdated` | ✅ Emitted | After a node is flushed to disk, `{path, root_id}` |
| `Wiki:RecentChanged` | ✅ Emitted (batch-aggregated) | `{root_id}`, published at most once per root within a single `ProcessBatch`, to avoid flooding on bulk operations |
| `Wiki:PendingChanged` | ❌ Not emitted | An event planned in the spec, nothing in the code sends it |
| `Wiki:RootEnabled` / `Wiki:RootDisabled` | ✅ Emitted | Sent after a successful Create/Delete, payload `{root_id, path, level}` |
| `Wiki:WriteFailed` | ❌ Not emitted | The spec envisions alerting after 5 consecutive failures, the code just logs |

---

## Key modules

| Package | Responsibility |
|---|---|
| `main.go` | Startup, random port, writes `wiki.url`, registers routes with Gateway, systemd notify, signal handling + graceful flush |
| `pkg/config/` | Viper INI config (`/etc/nimoos/wiki.conf`), auto-writes a sample and applies defaults |
| `pkg/db/` | SQLite open + PRAGMAs (`WAL` / `case_sensitive_like=ON` / `busy_timeout=5000`) + migrations |
| `pkg/pathutil/` | Case-sensitive path utilities (`Clean`, `IsUnder`, `Parent`) |
| `pkg/ignore/` | Container-directory (node_modules / .git etc.) + system-noise ignore rules. **A built-in, non-configurable baseline**: Synology `@eaDir`/`#recycle`/`@__thumb`, macOS `.AppleDouble`/`__MACOSX` etc., `lost+found`/`.snapshots`, `immich`, plus the prefix-matched `.Trash-*`; user config can only add to this, never remove from it |
| `pkg/wikimd/` | `.wiki.md` rendering + parsing (extracting the user-notes region) |
| `pkg/childmap/` | Child Map rendering: aggregation + Top-N collapsing |
| `service/repo/` | SQLite CRUD: wiki_roots / wiki_nodes / file_index / file_events / parse_status / **wiki_summaries**. **Every path-prefix LIKE uses EscapeLikeArg + ESCAPE '\\'** |
| `service/scanner/` | Watcher (fsnotify, routed into 3 paths) + Reconciler (load-to-map reconcile; **silently clears** leftover rows under a "recently turned opaque" directory without emitting delete events) |
| `service/processor/` | EventProcessor: debounce + MOVED_FROM/MOVED_TO pairing + cascading UPDATE on directory rename + reverse sync of user-notes |
| `service/writer/` | WikiWriter: strict ordering of Chtimes-locked mtime + DB commit + atomic rename |
| `service/roots/` | Root lifecycle + write test + FS-type detection (nfs/cifs/fuse auto-downgrade to scan_only) |
| `service/eventbus/` | Wraps the NimoOS-Common MessageBus, with a Noop fallback |
| `route/` | JWT middleware + LocalhostOnly middleware |
| `route/v1/` | Handlers for groups A/B/C (group C is in `internal.go`: file-events / needs-summary / node-evidence / summary / user-roots) |
| `tests/integration/` | E2E integration tests (build tag `integration`), covering all acceptance items in spec §13 |

---

## Data model

SQLite database `/var/lib/nimoos/wiki/wiki.db` (CGO_ENABLED=1). All timestamp fields are uniformly stored as **Unix milliseconds, INTEGER**.

| Table | Purpose |
|---|---|
| `wiki_roots` | Registered Roots: path / level / watch_mode / storage_mode / enabled / scan_interval_s. Each row is written by `POST /v1/wiki/roots` |
| `wiki_nodes` | **Each row corresponds to an explicitly registered Root** (and is **only seeded when a Root is registered**, see below). `user_notes` content is stored directly here — the DB is the authoritative source, `.wiki.md` is a rendered artifact; `last_flushed_mtime` is used by the Watcher to distinguish "written by us" vs. "externally edited." Since 2026-05-21, three columns have real maintainers: `last_modified` is advanced with `MAX` by EventProcessor on events (`SetDirtyAndTouch`), `ai_label` is written by the summary worker via `POST /_internal/summary`, and `child_count` is backfilled by WikiWriter on flush from the direct-child count in `file_index` (best-effort) |
| `file_index` | Every file within the monitored scope + container-directory placeholder entries; used by the Reconciler for reconciliation. `is_opaque=1` marks container directories (node_modules etc.), which aren't recursed into |
| `file_events` | The unified file-event table (merging the old design's pending_events + recent_changes). Three consumption modes: the `processed_at IS NULL` queue, `since=` incremental queries (`root_id` can be empty = across all roots), and `ORDER BY detected_at DESC` for rendering recent changes. JSON serialization is snake_case (the Parser contract) |
| `parse_status` | For the Parser worker to report parse/index status; this service only inserts `pending` on create events and never reads it (the reporting endpoint `index-status` is still a stub) |
| `wiki_summaries` | AI summary body (added 2026-05-21): `path` (FK → wiki_nodes, ON DELETE CASCADE) / `summary` / `generated_at` / `based_on_last_modified` / `generator_version`. `ai_label` is not here — that's stored in `wiki_nodes` |

**On how a wiki_node comes into existence (an easy thing to trip over):**

The `wiki_nodes` table **only** has a row for directories explicitly registered via `POST /v1/wiki/roots`. The single place in the whole repo that inserts into `wiki_nodes` is `service/roots/manager.go:Create` (seeding the root node). The Processor's `markNearestWikiNodeDirty` searches **upward** for the nearest existing wiki_node to mark dirty — it **never** auto-creates nodes for subdirectories.

In other words: Wiki grows one wiki_node for every Root the user registers — it's not a mirror of the file tree. To have `/DATA` as a space and `/DATA/Projects/nimoos` as a project, both Roots need to be POSTed separately. The schema's `wiki_nodes.level` reserves three tiers — `system`/`space`/`project` — but **the `system` singleton node (root_id=NULL) has no seed path anywhere in the code**; in practice only `space` and `project` are ever used. The "map" in the Agent integration also only renders two levels.

**Key correctness invariants:**

1. `PRAGMA case_sensitive_like = ON` — Linux filesystems are case-sensitive, but LIKE is case-insensitive by default; without this, a directory rename's `WHERE path LIKE '/DATA/ProjectA/%'` would wrongly hit files under `/DATA/projecta`.
2. Every path-prefix LIKE escapes `_` and `%` via `EscapeLikeArg` + `ESCAPE '\\'` — otherwise a `_` in a path would be treated as a SQLite wildcard.
3. The cascading UPDATE for a directory rename synchronously updates `file_index`, `wiki_nodes`, and unprocessed `file_events` within a **single transaction**.
4. WikiWriter's flush order: write to tmp → `os.Chtimes(tmp, T, T)` → **DB commit `last_flushed_mtime = T`** → atomic rename. The DB commit must happen before the rename; otherwise an fsnotify event could arrive first, the Watcher would read the old mtime, misclassify it as an external edit, and trigger a pointless reverse sync.

---

## Data storage and runtime

```
/etc/nimoos/wiki.conf              Config (INI, a sample ships with the package)
/var/lib/nimoos/wiki/
  └── wiki.db                      SQLite (WAL mode)
/var/run/nimoos/
  ├── wiki.url                     Service-discovery address
  └── wiki.pid                     systemd PID file
/var/log/nimoos/nimoos-wiki.log    zap logs
```

`.wiki.md` always lands at the corresponding wiki_node's `<root>/.wiki.md` (inline mode). The `mirror/<root_id>/...` directory layout planned in the spec is **currently not implemented**, see "Implementation status."

---

## Config sample (`build/sysroot/etc/nimoos/wiki.conf.sample`)

```ini
[common]
RuntimePath = /var/run/nimoos
DataPath = /var/lib/nimoos/wiki
LogPath = /var/log/nimoos

[wiki]
DefaultScanIntervalSec = 21600          # periodic reconcile every 6 hours
ScanOnlyScanIntervalSec = 600           # 10 minutes in scan_only mode
WikiWriteDebounceSec = 5                # at most one rewrite per node every 5 seconds
EventDebounceMs = 200                   # coalesce events for the same (root,path,op) within 200ms
ChildMapAggregateThreshold = 50         # aggregate display when >50 files share the same extension
RecentChangesKeep = 20                  # keep at most 20 entries in the Recent Changes section
RecentChangesRetentionDays = 90         # archived=1 after 90 days, physically deleted after 180
ShutdownFlushTimeoutSec = 5             # flush time budget on SIGTERM
ContainerDirs = node_modules,.git,__pycache__,venv,.venv,...
```

If `/etc/nimoos/wiki.conf` doesn't exist at startup, the sample is written out as a starting point.

---

## Startup order and dependencies

systemd dependency graph (`build/sysroot/usr/lib/systemd/system/nimoos-wiki.service`):

```
nimoos-gateway.service ──┐
nimoos-message-bus.service ─┼──▶ nimoos-wiki.service
nimoos-local-storage.service ┘
```

`Type=notify`; only considered started once `SdNotify(Ready)` is called.

main.go startup order (key steps):

1. Open SQLite + create data/log directories
2. **Run a Reconciler pass for every enabled Root** (catches up on any file changes missed during the restart)
3. Start the Watcher (fsnotify) + EventProcessor + WikiWriter + periodic Reconciler + archive job
4. Bind a random port + write `wiki.url`
5. Register `/v1/wiki` and `/doc/v1/wiki` with Gateway
6. SdNotify(Ready)
7. Start the HTTP server

On SIGTERM: WikiWriter tries to flush all dirty nodes (capped at `ShutdownFlushTimeoutSec`, 5s by default), then cancels all goroutines + calls http.Shutdown.

---

## Build and deploy

```bash
# Standard service build (CGO=1, needs gcc)
cd NimoOS-Wiki && CGO_ENABLED=1 go build -o nimoos-wiki .

# Multi-arch release (amd64 + arm64)
goreleaser release --snapshot --clean

# One-shot install onto the system (systemd unit + conf + binary + enable)
sudo bash scripts/install-wiki.sh --start

# Update an already-deployed service (swaps just the binary + restarts)
bash scripts/deploy.sh wiki

# Run tests
go test ./...                              # unit + repo
go test -tags integration ./tests/...      # E2E (~10s, exercises fsnotify)
```

`build/scripts/` holds the setup/migration/cleanup trio used by the official `.deb` install package (numeric prefix `08`; NimoOS=03, AI=07), packed alongside `build/sysroot/` into the archive by goreleaser.

---

## Relationship with other services

- **Depends on UserService**: reads the public key from `/var/run/nimoos/` to verify JWTs.
- **Depends on Gateway**: registers `/v1/wiki` and `/doc/v1/wiki` at startup via `POST /v1/gateway/routes`.
- **Depends on LocalStorage** (optional): for `GET /v1/wiki/candidates`, reads the address from `/var/run/nimoos/local-storage.url` and calls `GET /v1/storage` to list mount points. Returns an empty list when unavailable.
- **Depends on MessageBus**: sends `Wiki:*` events over the Unix socket `/tmp/message-bus.sock`; degrades silently when unavailable, without affecting the main flow.
- **Consumed by NimoOS-Parser**: Parser's WikiConsumer (`NimoOS-Parser/parser/wiki_consumer.py`) polls `_internal/file-events?root_id=` (empty = all roots) with a single global cursor, driving docling parsing + vector indexing. The snake_case JSON tags on `FileEvent` are the wire contract for this pipeline.
- **Consumed by NimoOS-AI's summary worker**: `NimoOS-AI/wiki_summary_worker/` (Python, a separate process) is driven by the systemd timer `nimoos-wiki-summary.timer` (`OnUnitInactiveSec=5min`); each round goes needs-summary → node-evidence → LLM (via nimoos-ai's `/v1/ai/_internal/chat/completions`) → POST summary. Config is read from the `[wiki-summary]` section of `/etc/nimoos/wiki.conf` (`Enabled` / `BatchSize` default 3 / `MaxPerHour` default 100 / `Model` etc.). **Watch the resource cost**: every round can trigger LLM inference; if it goes through local Ollama (especially on CPU-only machines) it will periodically max out compute — can be turned off with `[wiki-summary] Enabled=false` or by directly running `systemctl disable --now nimoos-wiki-summary.timer`, without affecting the Wiki service itself (the Summary section just stays a placeholder).
- **Consumed by NimoOS-Search**: Search calls `_internal/user-roots` to get the scope of roots visible to a user (connects directly to `wiki.url`, bypassing Gateway — Gateway blocks `/_internal/`).
- **Indirectly depended on by the NimoOS-AI Agent**: the Agent uses Wiki to locate the "topic map" of a user's data (the MCP tools `wiki_get_node` / `wiki_list_full_tree` / `wiki_recent_changes` are also on this path), then decides whether to go through Search for specific content.

---

## Design notes

1. **The DB is the authoritative source for user-notes, not the file.** When rendering, WikiWriter takes content directly from `wiki_nodes.user_notes` and splices it in — it no longer "reads the existing .wiki.md first to extract the preserved region." This eliminates the risk of WikiWriter overwriting a user's notes during concurrent SMB edits. External edits are reverse-synced into the DB by Watcher path 2, and WikiWriter then re-flushes to disk.

2. **fsnotify events are routed into three paths** (`service/scanner/watcher.go`):
   - **Path 1**: `.wiki.md.tmp` / system noise → dropped directly
   - **Path 2**: `.wiki.md` → handled by op; on CREATE/MODIFY, compares `mtime` against `wiki_nodes.last_flushed_mtime` — a match means it's what we just wrote (ignored), a mismatch means an external edit (triggers reverse sync); DELETE/MOVED_FROM directly marks dirty so WikiWriter rebuilds it
   - **Path 3**: ordinary files/directories → goes into file_events; a newly created directory that's a container (node_modules etc.) gets a single opaque entry and isn't recursed into, otherwise it's recursively backfilled

3. **The cascading UPDATE for a directory rename must be case-sensitive + escape-safe.** `PRAGMA case_sensitive_like = ON` is the baseline requirement; every prefix LIKE must also go through `EscapeLikeArg` to handle `_` and `%` + `ESCAPE '\\'`, otherwise an underscore in a user's directory name would corrupt the data.

4. **MOVED_FROM/MOVED_TO pairing happens in EventProcessor**, not in the Watcher. Linux's `fsnotify` library doesn't expose the inotify cookie, so the Watcher emits Rename(src) and Create(dst) as independent events; EventProcessor pairs a Rename+Create with the same root and Create.IsDir=true within a 1-second time window into a single rename event, then runs the cascading UPDATE. A Rename that can't be paired within the window degrades into a Delete.

5. **Large-directory aggregation and container-directory skipping are two different things.** Container directories (node_modules) aren't indexed at all, and the Child Map just shows a single line "N files (skipped)"; **large, homogeneous directories with genuine value** (a photo folder) still have every file recorded in file_index (for a future vector store to use), but the Child Map rendering aggregates by extension ("10342 .jpg, 124 .raw").

6. **No content search.** Wiki is a map, not an evidence store. Any query about "where in the actual content is X mentioned" goes through NimoOS-Search / NimoOS-Parser (parsing + full-text/vector indexing); Wiki only provides two feed interfaces: `_internal/file-events` (the event stream) and `_internal/user-roots` (scope).

7. **The Reconciler's silent clearing is one-directional** (`service/scanner/reconciler.go`). When a directory is **newly** deemed opaque (e.g. `immich` was just added to the baseline), its already-indexed leftover rows in `file_index` are deleted directly, **without emitting delete events** — this avoids flooding Recent Changes from a single baseline change. Conversely, when a user genuinely deletes an opaque directory, delete events are still emitted in full as usual (downstream cache invalidation needs to see them) — the noise is an accepted, deliberate tradeoff.

---

## Known limitations / what's next

See spec §12 "Open items left for future work" for details:

1. **AI Summary Worker** — ✅ shipped (2026-05-21, three `_internal` endpoints on the Wiki side + the `wiki_summaries` table; the worker lives in the NimoOS-AI repo). The `Summary` section is generated by the worker and rendered into `.wiki.md`; the **`Key Sources` section is still a placeholder**, `_Not generated yet_` (`pkg/wikimd/render.go`). The `/node` API's `summary` field also still returns null.
2. **Parser Worker + vector store** — ✅ wired up (Parser's WikiConsumer consumes `_internal/file-events`); but `_internal/index-status` is still a 503 stub, the `parse_status` table is write-only, and the Pending Index section's count has no real reporting side.
3. **Cross-Root symlinks** — currently not followed; if the target is inside another Root, that Root scans it on its own.
4. **Event reliability on NFS/CIFS** — auto-downgrades to `scan_only`, latency depends on `ScanIntervalSec`.
5. **UI** — currently no frontend, all interaction goes through the API.

---

## Implementation status vs. the design spec (aligned 2026-07-07)

To keep the docs from drifting away from the code and misleading readers, this section lists every place where "the design calls for X but the code doesn't do it (or only half-does it)." Fix priorities below are just my own suggestion.

### A. Designed but completely unimplemented in code

| Item | Spec source | Current code behavior | Impact |
|---|---|---|---|
| `storage_mode=mirror` | 2026-05-13 §5 (line 317), §8.1 | `WikiWriter` always writes `<root>/.wiki.md`, no mirror branch | Non-writable Roots have no fallback; the write-test failure message is misleading users |
| Degrading on hitting the inotify watch limit | 2026-05-13 §9 line 577 | Doesn't read `/proc/sys/fs/inotify/max_user_watches`; silently drops the watch once the limit is hit | On large NAS setups, some directories lose real-time events and can only rely on the 6h periodic reconcile as a fallback |
| `Wiki:PendingChanged` / `Wiki:WriteFailed` events | 2026-05-13 §7.4 | Nobody publishes them at all | The UI / monitoring can't see pending-count changes or write-failure alerts (`Wiki:RootEnabled`/`Wiki:RootDisabled` were shipped on 2026-05-20) |
| Auto-setting `enabled=0` when a Root's path disappears at runtime | 2026-05-13 §9 line 574 | Watcher errors are only logged, `enabled` is untouched | After a drive is pulled, the wiki_root stays enabled and keeps repeatedly failing to write |
| The `wiki_nodes.level='system'` singleton top-level node | 2026-05-13 §4.1 schema | No seed path anywhere, completely dead code | In practice there are only two tiers (space / project); the three-tier structure is fictional |
| Backing up a user's hand-edited system region before overwriting it | 2026-05-13 §9 line 576 | WikiWriter never reads the existing file; a corrupted system region is simply overwritten by the next flush, no backup | Anything a user carelessly writes into the system region can silently vanish |
| inotify cookie / cross-watch rename tracking | 2026-05-13 §6.1, last paragraph | EventProcessor does best-effort pairing within a 1s time window, degrading to delete on failure | An mv across watches loses the prefix cascade and becomes two independent delete+create events |

### B. Designed, partially implemented / with logic gaps

| Item | Current defect |
|---|---|
| Reverse sync of user-notes | `Watcher.SyncOut` channel capacity is 64; once full it drops via `default` → once one external-edit event is dropped, that edit's notes never make it into the DB until the user edits again |
| Ordering between WikiWriter and reverse-sync for user_notes | ~~spec §6.5 stresses the ordering of mtime + DB commit~~ **fixed 2026-05-20**: `pkg/nodelock` provides a per-path mutex; Writer.FlushOne and Processor.SyncUserNotesFromDisk serialize on the same nodePath via a shared `*nodelock.Locks` injected from `main.go` |
| External edits to `.wiki.md` while the service is down | **fixed 2026-05-20**: `Processor.BootSyncRoot` runs at main.go startup (after `bootReconcile`, before watcher registration), doing a stat-vs-`last_flushed_mtime` comparison for every enabled root's wiki_nodes, and calling SyncUserNotesFromDisk directly for anything that changed |
| The window between boot reconcile and Watcher registration | main.go does "reconcile first, then `wch.Watch(...)`" — both steps do a WalkDir, so file changes happening in between aren't seen by either, and only get caught by the next periodic reconcile |
| Reconciler change detection | Only relies on `(mtime, size)` to decide modify; `touch` / `cp -p` etc. ("content changed but metadata didn't") is completely undetectable |
| Leftover wiki_node after a Root directory is deleted | Handling the fsnotify delete only touches `file_index`, not `wiki_nodes`. After an entire Root directory is deleted, the DB is left with an orphaned dirty node, and WikiWriter keeps retrying and hitting ENOENT |
| The `mtime <= LastFlushedMtime` check | At millisecond granularity, a user edit that happens to land in the same millisecond as a WikiWriter flush → gets treated as its own echo and dropped; low probability but non-zero, and more likely to trigger on network filesystems |
| MessageBus event aggregation | **fixed 2026-05-20**: `ProcessBatch` dedupes by `root_id` at the end and publishes, at most once per root per `Wiki:RecentChanged` within a single batch |
| Internal-interface stubs | **mostly implemented as of 2026-05-21/22**: `needs-summary` / `summary` / `node-evidence` / `user-roots` are all real implementations; only `index-status` still returns 503 |

### C. Design and code disagree (doc drift)

| Item | Old design | Current code |
|---|---|---|
| JWT middleware | 2026-05-13 §7 says "all routes enforce JWT" | 2026-05-14 v2 design §4.2 changed it to a localhost exemption + trusting `X-NimoOS-User-ID`, **the code runs on v2** — this doc has been corrected |
| `recent-changes` query params | The old description only had `root_id` + `limit` | The code supports the `since_ms` cursor, added in 2026-05-14 v2 design §4.5 — this doc has been corrected |
| Subwikis semantics | The spec's `/node` response has a `subwikis` field, and the design context implies a nested wiki structure | In practice it's found by directly matching path prefixes for nodes that happen to be **exactly one level directly below** in the current wiki_nodes table; Roots spanning levels (e.g. `/DATA` and `/DATA/Projects/nimoos`, with `/DATA/Projects` never registered in between) don't get linked to each other |

### D. Suggested fix priority (my own ordering)

**Already done (2026-05-20):**
- ✅ Section B: WikiWriter ↔ reverse-sync ordering, external edits during service downtime, event aggregation → fixed (see the internal design doc for details)
- ✅ Section A: `Wiki:RootEnabled` / `Wiki:RootDisabled` events → shipped

**Already done (2026-05-21 ~ 2026-05-25):**
- ✅ The full summary pipeline: the `wiki_summaries` table + the three endpoints `needs-summary` / `node-evidence` / `summary` + WikiWriter rendering the Summary section + maintenance of `last_modified` / `child_count` (see "Summary pipeline" above)
- ✅ `_internal/user-roots` (the root-scope interface for Search, 2026-05-22)
- ✅ The two-part Parser contract: `FileEvent` snake_case JSON tags + `ListSince` supporting an empty root_id across all roots (2026-05-25)
- ✅ The ignore baseline (built-in NAS/OS noise directories) + the Reconciler's silent clearing of leftover rows under newly-opaque directories (2026-05-20)

**Still to do (by priority):**

1. **Section A, item 1, `mirror` mode**: either implement it, or remove it completely from the API + error messages so users don't think there's a fallback.
2. **Section B, item 1, `Watcher.SyncOut` dropping when full**: untouched this round, still the chan-capacity-64 drop path — notes can still be lost under a burst of external edits.
3. **Section A, item 5, `level='system'` dead code**: collapse the schema enum down to `{space, project}`, and clarify the semantics of the `Subwikis` field in Render.
4. **Section A, item 2, degrading on the inotify limit**: a real reliability issue on large NAS setups.
5. The rest (reconciler relying only on (mtime,size), orphaned wiki_nodes after deleting a root directory, the `Wiki:PendingChanged`/`WriteFailed` events): lower priority.
