# R1 architecture handbook

*For engineers and architects joining the project. Read this before the
specification: it explains what the product is, which decisions are already
fixed in code, where every subsystem lives, and how the remaining milestones
(4–6 and 9–14) are meant to be built on top of what exists.*

Companion documents: the full specification
([`r1-specification.html`](r1-specification.html), rev 36, 2026-09-17), how to
run a node ([`running-locally.md`](running-locally.md)) and how to test it
([`testing.md`](testing.md)). Section numbers written as §n refer to the
specification.

---

## 1. The product in one page

**What it is.** A self-hosted knowledge platform: one Go binary, PostgreSQL,
NATS and an in-process OpenFGA, exposing pages, blocks, permissions,
publishing, CEL search and MCP through one API, plus a minimal web app for
live collaborative editing. Everything is reachable through the API; the web
app is one client of it. The desktop release (R2) is the same binary packaged
with Electron, so nothing in R1 may be desktop-specific.

**Who it is for.** Teams that want a Logseq/Obsidian-class block editor and
knowledge graph they can run themselves, with agents (MCP) as first-class
users acting under a person's permissions, and open formats end to end
(Markdown, mdast JSON, Loro's update format). Sizing targets: 500 users on
Docker Compose, 10K users and 600K blocks on Kubernetes; the R1 web app is
sized for 10 concurrent editors over 20 docs.

**One primitive, one store, three surfaces.** The *block* is the primitive
(id, tree position, Markdown content, property bag, type pointer). Postgres
holds everything durable except bytes; an S3-compatible store holds assets,
cold snapshots and backups, all as node-produced ciphertext. Surfaces are
navigation entries over the same blocks: *Documents* (pages), *Work* (blocks
whose type extends the shipped base type `work`, keyed `KB-123`) and, in R2,
*Canvas*.

**Non-negotiable invariants** (§3, all enforced by code today):

1. The per-doc CRDT log (`doc_updates`) is the only source of truth for
   content; every other table is a projection that can be rebuilt.
2. A write is acknowledged only after its update row commits; the ack carries
   the database-assigned `seq`.
3. Updates are immutable and commute; `seq` is for cursors, gaps and history,
   never for merge order.
4. Block ids are UUIDv7, assigned once, permanent across moves, boundaries and
   restores.
5. `workspace_id` is the first column of every primary key and index; there
   are no cross-workspace joins, and row-level security backs the mandatory
   predicate.
6. Everything above the key ring is plaintext; ciphertext exists only in
   `doc_updates`, `doc_snapshots` and object storage; nothing outside the node
   ever holds a workspace key.
7. A permission boundary is a doc boundary: blocks inherit access from their
   doc, docs from their parent doc unless the parent tuple is absent.

**Explicit non-goals for R1** (§1): Electron packaging, Canvas, WYSIWYG marks,
views/boards, workflows and automations, comments, git/folder sync, MDX
evaluation, webhooks, federation, custom domains, and client-held workspace
keys (that last one is a different product, not a later phase).

---

## 2. System shape

### 2.1 One binary, four roles

`KB_ROLE` selects what a process runs: `all` (Compose, one container), or
`api`, `sync`, `worker` (Kubernetes Deployments). There is no CLI; migrations
run at startup under an advisory lock and every operator action is an
`AdminService` RPC (the operator token is derived from the node key so it works
before any workspace exists). The web app is embedded and served at `/app`.

| Component | Role | State | Status today |
| --- | --- | --- | --- |
| Connect API (`/rpc/kb.v1.*`) | api | none | services listed in §2.3 |
| Sync rooms (`/ws/sync`, `SyncService`) | sync | in-memory doc tails rebuilt from the log | done (M7) |
| Materializer | inside api and worker | warm decrypted docs, LRU | done (M3) |
| Web app (`/app`) | served by api | none server-side; encrypted IndexedDB in the browser | done (M8) |
| Workers (River jobs), outbox poller | worker | none; jobs in Postgres | M5 |
| OpenFGA | library in every process, `fga` database | shared store | M6 (compiler done) |
| MCP server + OAuth AS | inside api | none | M12 |
| NATS | external | events and room membership only | M5 |
| Object storage | S3 API; SeaweedFS bundled | ciphertext only | package done, wiring M11 |

Reading order of a structured write (§2, "Request lifecycle"): client →
API (auth, idempotency key, permission checks) → materializer (advisory lock
per doc, apply mutation to the live Loro doc, encrypt the delta) → Postgres
(`UPDATE docs`, `INSERT doc_updates`, `INSERT outbox`, idempotency record, one
transaction) → ack with `version = seq` → sync fan-out and, later,
projections. The web app takes a different door: its Loro doc pushes updates
through sync rooms, which append to the same log and feed the same outbox.

### 2.2 Repository map

```
cmd/kb                     entry point: config from env, app.New, serve
internal/app               wiring of every subsystem per role (wire.go is the dependency graph)
internal/server            HTTP surface: Connect handlers, /healthz /readyz /metrics, middleware
internal/config            KB_* environment, limits, validation
internal/db                pgx pool, tenant transactions (RLS), migrations (goose, embedded SQL)
internal/db/migrations     00001_init.sql: the complete R1 schema, RLS policies, kb_app role
internal/did               did:key (Ed25519) and did:pkh (EIP-155) parsing and formatting
internal/auth              challenges, SIWE and Ed25519 verification, PASETO access tokens,
                           rotating refresh tokens, devices, agent tokens, rate limits, interceptor
internal/keyring           node key (sealed file, Argon2id), HPKE-wrapped workspace keys,
                           XChaCha20-Poly1305 with AAD, key cache with zeroize
internal/loro              cgo binding to rust/loro-cabi: op batches, snapshots, version vectors,
                           tree state parsing
rust/loro-cabi             Rust C ABI over the loro crate (staticlib), op validation
internal/truth             encrypted append-only update log, snapshots, materializer,
                           multi-doc mutations, outbox events
internal/schema            block types, property and relation definitions, defaults, validation
internal/projection        synchronous index of a doc into pages/blocks/properties/edges
internal/authz             Guard interface; RoleGuard (workspace roles) until FGA lands
internal/policy            permission scheme → OpenFGA model compiler, Explain annotation
internal/audit             hash-chained audit log
internal/api               Workspaces, Projects, Members, Schema, Pages, Blocks services,
                           idempotency, boundaries, keys, expansion of portal docs on read
internal/sync              rooms, WebSocket and Connect transports, awareness, debounced indexer
internal/web               embedded SPA handler (config injection, CSP, SPA fallback)
internal/query             CEL environment, in-process evaluation, SQL generator internals (M9)
internal/objectstore       EncryptedObjectStore: envelope encryption, S3 and filesystem backends
internal/markdown          mdast node types, Walk/Equal, entity helpers, CommonMark cases (M4)
internal/telemetry, logging, version
internal/testutil          per-test migrated databases
proto/kb/v1, gen/          the API (18 services), generated Go and Connect code (committed)
web/                       React/Vite app; web/src/lib/sync-client is the framework-free client
deploy/compose             Compose bundle: kb, postgres, nats, seaweedfs, pgbackrest (profile)
docs/                      specification, running, testing, this handbook
```

### 2.3 API surface: implemented and pending

All 18 services are defined in `proto/kb/v1` and have generated handlers;
`internal/app/wire.go` registers only the implemented ones. A method that
exists in proto but not in code returns `Unimplemented`.

| Service | State |
| --- | --- |
| AuthService, AgentsService | implemented (M2) |
| WorkspacesService (Create/Get/List/Update; RotateKey, Export, Delete pending), ProjectsService, MembersService, SchemaService | implemented (M3) |
| PagesService (Create/Get/List/Update/Move/Trash/Restore/Purge/Journal/Convert/versions; ImportFolder, GetImportReport, ExportMarkdown pending) | implemented (M3), import/export in M11 |
| BlocksService (all methods including Batch, Restrict, Unrestrict, Duplicate) | implemented (M3) |
| SyncService | implemented (M7) |
| GroupsService, ShareLinksService | proto only → M10 |
| PermissionsService, PolicyService | proto only → M6 |
| QueryService | proto only → M9 |
| PublicationsService | proto only → M10 |
| AssetsService | proto only → M11 |
| EventsService | proto only → M5 (Subscribe) and M14 (audit RPCs) |
| AdminService | proto only → M5 (Reindex, Compact), M13 (Verify, keys, jobs) |

### 2.4 Data model at a glance

Truth: `docs` (one row per CRDT log, `current_seq`, `indexed_seq`, kind
`page` or `boundary`, `page_id`), `doc_updates` (ciphertext per seq, AAD-bound,
`client_id`/`client_seq` for idempotent pushes), `doc_snapshots`,
`workspace_keys`, `outbox` (partitioned by day), `idempotency_keys`,
`audit_log` (partitioned by month).

Projections: `pages`, `blocks` (ltree `wbs_path`, `depth`, `rank`, `key`,
`has_acl`, `portal_doc_id`, generated `tsv`), `block_properties`,
`block_edges`, `page_aliases`, `doc_access`, `principal_sets`,
`view_programs`, `publications`, `publication_redirects`.

Configuration and identity: `workspaces`, `projects`, `principals`,
`workspace_members`, `invites`, `groups`, `group_members`, `share_links`,
`sessions`, `auth_challenges`, `auth_failures`, `agent_tokens`,
`rate_limits`, `property_definitions`, `block_types`, `relation_types`,
`project_counters`, `block_keys`, `schemes`, `assets`, `uploads`,
`import_jobs`, `oauth_clients`, `oauth_codes`, `oauth_refresh_tokens`,
`node`.

Every tenant table has RLS with the single policy
`workspace_id = current_setting('app.workspace_id')::uuid`;
`db.Tx` sets the role and the settings with `SET LOCAL` per transaction (so
pgbouncer transaction pooling works), `db.System` runs as the owner for
cross-workspace maintenance.

---

## 3. Decisions already made, and why

These are the load-bearing decisions. Each is implemented; changing one is a
redesign, not a refactor.

### 3.1 Truth is a CRDT log per doc; everything else is disposable

Every page and every boundary is a Loro document. Structured API writes and
collaborative edits are both appended to `doc_updates` as encrypted Loro
updates; the tree of blocks (`pages`, `blocks`, `block_properties`,
`block_edges`) is derived and rebuildable (`AdminService.Reindex`). This is
what makes offline editing, live collaboration, history and agents coexist
without a second data model: the API and the editor write to the same log.

Consequences you must respect:

- Never write projections without an update behind them. The only exception
  is data that is not content (keys, membership, publications).
- Reads that need freshness use `wait_for_seq` and `indexed_seq`; the API
  returns `version = seq` on writes and callers pass `if_version` for
  linearizable semantics. Concurrency correctness comes from the CRDT plus
  preconditions evaluated under the per-doc advisory lock, not from global
  locks.
- The Loro document layout is fixed and shared by Go and TypeScript (§3,
  "CRDT document layout"): root map `meta` (`title`, `icon`, `format`,
  `journal_date`, `type_id`, `props` map) and root tree `blocks` with the
  fractional index enabled with zero jitter; node data holds `id`, `type_id`,
  `props` (a `LoroMap`; relation sets are `LoroMap<target_id, true>`),
  `content` (a `LoroText` with the block's Markdown source), `portal_doc_id`,
  `created_at`, `created_by`. `internal/loro/ops.go` (Go) and
  `web/src/lib/blocks.ts` (TypeScript) both write exactly this shape; the
  Rust shim validates op batches against a shadow tree.

### 3.2 Loro, reached from Go through a Rust C ABI

Loro was chosen over Yjs (decision 2026-09-17) for its native movable tree
(concurrent moves converge, cycles are impossible, sibling order is a
fractional index) and its Rust core. Go reaches it through `rust/loro-cabi`, a
small `staticlib` exposing a JSON op-batch API, linked with cgo
(`internal/loro`). The web app uses the official `loro-crdt` WASM build.

Consequences: one pinned Loro version per release on both sides (the crate in
`rust/loro-cabi/Cargo.toml` and the npm package must produce mutually
importable bytes; the FFI cross-version test in §14 is still to be written);
`make loro` (cargo) must run before `go build`; a wazero-hosted WASM build is
the documented fallback for CGO-free targets and is not implemented.

### 3.3 Encryption at the node edge, plaintext projections

Six rules (§5): one XChaCha20-Poly1305 key per workspace (versioned), held by
the node, HPKE-wrapped to the node's X25519 key; the node key lives in an
Argon2id-sealed file (Compose) or a Secret/KMS (Kubernetes); ciphertext is
AAD-bound to `workspace ‖ doc ‖ seq` (updates and snapshots) or
`workspace ‖ object ‖ chunk` (objects); projections are plaintext inside
Postgres by design (search and agents need them); devices get TLS, a session
and an AES-GCM device key for their IndexedDB cache. Rotation is rare and
versioned. `internal/keyring` is the only code that unwraps keys; unwrapped
keys live in an LRU with idle TTL and are zeroized.

The security statement is therefore exact and must stay exact in every doc
and UI string: history and assets are encrypted with a per-workspace key held
by the node; the index relies on volume encryption; the node is trusted.

### 3.4 Postgres is the only durable store, and it is used carefully

Rules already in force (§3 physical design, §8 performance rules): keyset
pagination only, never `OFFSET`; no total counts by default; every query
names its columns; `workspace_id` leads every index; `doc_updates`
ciphertext columns use `STORAGE EXTERNAL`; `outbox` and `audit_log` are
partitioned so retention is `DROP PARTITION`; the app role has
`statement_timeout`, `lock_timeout` and
`idle_in_transaction_session_timeout` set per transaction (`db.TxOptions`).
The outbox pattern (a row written in the same transaction as the update)
is how anything asynchronous learns about a write; NATS is never on the hot
path.

### 3.5 Identity is a DID; agents act as two principals

There are no passwords. A person is `did:key` (Ed25519 key generated in the
browser) or `did:pkh` (EIP-155 wallet, SIWE message signed with EIP-191, with
an EIP-1271 fallback when `KB_EVM_RPC_URL` is set). Sessions are 15-minute
PASETO v4.local access tokens keyed from the node key plus rotating refresh
tokens with reuse detection; web clients get the refresh token as an
HttpOnly cookie scoped to the auth service path and must send
`X-KB-Client: web` (CSRF defence by custom header). Agent tokens (`kba_…`)
are principals owned by a user with scopes and a tool allowlist; every check
for an agent runs twice, as the agent and as the owner (`auth.Identity`
carries both).

### 3.6 Authorization is a compiled scheme, and the doc is the permission unit

Roles are data: a workspace-owned JSON scheme (permissions, roles with grants
and inheritance, assignable levels workspace/project/doc, CEL conditions,
exclusions) compiles to an OpenFGA schema 1.1 model (`internal/policy`, done,
golden model under `testdata/default.fga`). Block-level permissions come from
making a block's subtree its own doc (`BlocksService.Restrict`, already
implemented as a *boundary* doc with a portal node in the parent). Queries
are access-filtered through the `doc_access` projection; direct `Check`
gates every write and every sync open. Until M6 lands, `authz.RoleGuard`
answers with workspace membership roles only.

### 3.7 Sync is seq-cursored, commutative, and coalesced

The protocol (§7, `proto/kb/v1/sync.proto`) is deliberately small: `hello`,
`open(since_seq)` returning a snapshot or a tail, `push(client_id,
client_seq, update)` acknowledged with server seqs and deduplicated by
`(client_id, client_seq)`, `update` fan-out, `pull(since_seq)` for gaps, and
`awareness` bytes with a 30 s TTL that are never persisted. Because Loro
updates commute, the client can import in any order and only needs `seq` to
know it has a contiguous prefix. The client coalesces local commits for
50 ms and exports one update per push using a peer-scoped version vector,
which keeps the server log at one row per burst instead of one per keystroke.
Agents and integrations never use sync; they use structured writes.

### 3.8 The web app is a thin, honest client

React, Vite, Connect-web clients generated from the same protos, TanStack
Query for lists, CodeMirror 6 for the focused block only, sanitized Markdown
for the rest. `web/src/lib/sync-client` is framework-free and meant to be
published for third parties. Two local decisions worth knowing: the editor
uses its own two-way CodeMirror↔LoroText binding (`web/src/app/editor/binding.ts`)
because `loro-codemirror`'s sync plugin drops text events that arrive in the
same import batch as tree events, which is exactly our doc shape; and remote
carets are exchanged as stable Loro cursors, not offsets. Size budgets are
enforced by `web/scripts/budget.mjs` (initial JS is 362 KB gzip against a
480 KB ceiling; the specification's 300 KB target is an M14 item).

### 3.9 API conventions

Connect RPC, binary or JSON, one proto package `kb.v1` (`/rpc/kb.v1.<Service>/<Method>`,
also mounted at the root for plain gRPC). Errors use the closed model in
`internal/apierr` with typed details (`VersionConflict`, `SchemaViolation`,
`PermissionDenied`, `CelError`). Mutations accept an `Idempotency-Key` header
(`api.idempotent` stores the response for 24 h). Every block carries
`uri = kb://{workspace_id}/{block_id}` for life. Generated code is committed
so the Go build never needs Node or buf.

---

## 4. What exists in detail: packages, interfaces, extension points

This section is the map for anyone extending the system. The interfaces named
here are the seams the remaining milestones plug into.

### 4.1 Truth layer (`internal/truth`, `internal/loro`)

- `truth.Store`: `CreateDoc`, `GetDoc`, `AppendUpdate` (encrypts, assigns seq
  under the row lock, records `client_id`/`client_seq`), `Updates(after,
  until, limit)` (decrypts), `LatestSnapshot`/`SaveSnapshot`, `SetDeleted`,
  `PurgeDoc`, `InsertOutbox`.
- `truth.Materializer`: warm cache keyed by doc, Postgres advisory locks,
  `Mutate`/`MutateOne`/`CreateAndMutate`/`MutateWithNew` (multi-doc
  mutations in one transaction, e.g. boundaries and cross-page moves),
  `Read`/`ReadAt(seq)`/`Snapshot`, `ImportUpdate` for sync pushes
  (validates the header, imports into the warm doc, appends).
  `Options.OnCommit` runs projections and idempotency inside the same
  transaction; `Options.IfVersion` implements preconditions.
- Events: `truth.Event` rows land in `outbox` with the kinds in
  `truth.go` (`page.*`, `block.*`, `props.changed`, `type.changed`,
  `boundary.*`, `doc.synced`). Nothing consumes the outbox yet; that is M5.
- `loro.State` is the parsed tree (`Meta`, `Roots`, `ByID`, `ByTreeID`);
  `loro.Apply(ops)` returns the update bytes, created ids and whether
  anything changed.

### 4.2 API layer (`internal/api`)

`api.Deps` is the seam list: `Guard authz.Guard`, `Schema schema.Repo`,
`Splitter` (Markdown → block specs and back; `NaiveSplitter` today, the M4
engine replaces it), `Provisioner` (FGA tuples on workspace/project/doc
creation; `NoopProvisioner` today, M6 replaces it), `Notify` (fan-out hook
used by sync rooms). Reads expand boundary docs into the parent tree
(`expand`/`xnode` in `docs.go`) after a permission check per boundary;
`toBoundary`/`Unrestrict` split and merge subtrees; work keys are allocated in
the write transaction from `project_counters`.

### 4.3 Projection (`internal/projection`)

`projection.IndexDoc(ctx, tx, Input{…, State, Seq, Snap, Analyzer})` upserts
`pages`, `blocks`, `block_properties`, `block_edges` for one doc with `unnest`
statements, computes `wbs_path` (ltree of hyphen-less UUIDs), resolves links
by normalized title and aliases, and handles boundary docs (root path
continues from the portal's parent). `projection.Analyzer` is the seam for
the Markdown engine: it turns a block's source into plain text, inline refs
and inline properties. `PlainAnalyzer` is the placeholder. The API calls
`IndexDoc` synchronously inside write transactions; sync pushes are indexed
by `internal/sync/indexer.go`, a per-doc debouncer that the worker's job
queue is meant to take over.

### 4.4 Authorization seams (`internal/authz`, `internal/policy`)

`authz.Guard` (`Check`, `Role`, `ViewSets`) is called by every service;
`Resource` carries type, id, project and doc. `policy.Compile(scheme)` returns
the DSL, the validated model, `RoleRelations` (where grant tuples go),
`PermissionRelations` (which relation answers which permission) and
`PublicDocRelations` (which doc relations accept `user:*`);
`policy.AnnotateExpand` maps an Expand tree back to scheme roles.

### 4.5 Sync (`internal/sync`)

`Hub` owns rooms keyed by `(workspace, doc)`; `Handler()` serves the
WebSocket, `NewService(hub)` the Connect transport; `Broadcast` is the API's
fan-out entry; `CloseDoc` disconnects subscribers when a doc is trashed or
access is revoked (the caller for revocation arrives with M6). Subscribers
join the room *before* the open read and stay gated until the `opened` frame
is out, so no update committed during the read is lost. Departing peers are
announced to the room so cursors vanish immediately.

### 4.6 Web app (`web/`)

`src/lib/sync-client` (client, persistence, awareness, frames; 11 protocol
tests against an in-memory server), `src/lib` (config injection, identity,
wallet, session, api, auth, loro init, markdown rendering, block tree
helpers), `src/app` (routes, screens, editor). The e2e suite
(`web/e2e/app.spec.ts`) drives the real binary. Out of the M8 scope table but
still R1 (§11): `[[page]]` and `KB-123` autocomplete, backlinks panel, search
box, share dialog with share links, publish toggle, version history. They are
small once M9/M10 provide the RPCs and are listed under M14 below.

### 4.7 Ready-made building blocks for later milestones

- `internal/objectstore`: `EncryptedObjectStore` with envelope encryption
  (per-object DEK wrapped by the workspace key through a `DEKWrapper`),
  1 MiB chunks with the chunk index in the AAD, an authenticated trailer,
  S3 (`aws-sdk-go-v2`, path style) and filesystem backends, opaque keys
  `{workspace}/{purpose}/{uuid}`. It is compiled and vetted but has no tests
  or callers yet.
- `internal/query`: CEL environment per project schema, in-process
  `Program` with `Eval`/`Explain`, the SQL generator internals (`sqlGen`)
  that already handle identifiers, literals, comparisons with two-valued
  semantics, property semi-joins, comprehensions and tree functions. There is
  no exported `Compile` entry point, no cursor codec and no tests yet.
- `internal/markdown`: mdast node vocabulary with `Walk` and structural
  `Equal`, entity/escape helpers, CommonMark 0.31.2 spec cases in
  `testdata/`. No parser or serializer yet.
- Schema tables that are empty today: `doc_access`, `principal_sets`,
  `view_programs`, `publications`, `publication_redirects`, `assets`,
  `uploads`, `import_jobs`, `schemes`, `groups`, `group_members`,
  `share_links`, `oauth_*`.

---

## 5. Editing and collaboration surface

**Decision: one document model, several lenses.** A page is a Loro tree of
blocks whose content is Markdown in a LoroText. Every view is a lens over that
text, and every edit, however native it looks, is a text edit on one block.
Collaboration, presence, undo, history and offline therefore behave
identically in every lens, because they only know text positions and block
ids. The specification placed WYSIWYG editing in R2; this design is additive
(model, sync client, projections and server are untouched, and Source mode is
what exists today), so it runs as its own track right after M4.

### 5.1 Lenses

| Lens | What the user sees | How editing works |
| --- | --- | --- |
| **Native** (default) | Notion-like page: rendered blocks, hidden Markdown syntax, block handles, slash menu, floating format bubble | Live preview in CodeMirror 6: syntax is hidden by decorations except around the caret; bold, headings, checkboxes, links, callouts render in place. Formatting commands are Markdown transforms (toggle bold on a range inserts or removes `**`). |
| **Source** | The block's Markdown, monospace, syntax highlighted | Plain CodeMirror, the editor that exists today |
| **Split** | Source on the left, rendered page on the right, aligned by block | The left pane edits; the right pane is read-only rendering with the active block highlighted and scroll locked by block id, never by line ratio |
| **Read** | Rendered page, no editing affordances | What viewer roles and published pages get; presence of editors stays visible |

The mode is a segmented control in the page header (Native · Source · Split),
cycled with Ctrl/Cmd-E, remembered per page per user, shareable through
`?view=split`. Read is automatic for viewer roles.

### 5.2 Why CodeMirror live preview for Native

One editor engine, one Loro binding (already written and tested), one position
space, almost nothing added to the bundle. Obsidian's live preview proves the
feel can be fully native. A ProseMirror or Milkdown lens can be added later as
a fourth lens without touching the model; it would cost roughly 150 KB gzip
and a re-parse-on-remote-change path that is much harder to test.

### 5.3 Presenting collaboration

Presence already carries a block id plus stable Loro cursors, so it maps into
every lens exactly.

- **Header.** Stacked avatars of everyone on the page, colored from their DID;
  hover shows name and mode; click follows them (the view keeps their block in
  sight until you move).
- **Block rail.** A thin colored bar on the left edge of any block a peer is
  in, with a name chip on hover; the same rail in Native and Source, both
  panes in Split.
- **Inside the focused block.** Remote carets with name flags and translucent
  selections.
- **Preview pane.** A peer's caret maps through the parser's source map to the
  rendered node and shows as a colored underline on that node; node
  granularity is enough there.
- **Sync state.** One pill: Saved, Saving, Offline with pending count. No modal
  ever interrupts editing; reconnection converges silently.
- **No locks and no "someone is editing" warnings.** The CRDT makes them
  unnecessary; the rail is information, not a gate.

```
+ sidebar --+----------------------------------------------------------+
| Projects  |  < Documents        Native . Source . Split   (o)(o) Saved|
| Pages     |                                                          |
| Work      |  # Title                                                 |
|           |  | paragraph rendered, syntax hidden...      (Ada is here)|
|           |    [ ] task item                                         |
|           |  | > callout                                             |
+-----------+----------------------------------------------------------+
```

### 5.4 Visual language

Minimal chrome: no persistent toolbar, a contextual bubble on selection, a
slash menu on `/`, a command palette on Ctrl/Cmd-K, a status bar with word
count and sync state. A 720 px prose column, Inter or the system font for
prose and a monospace face for source, calm color tokens with full dark mode,
motion of 150 ms at most. The tokens already live in `web/src/styles.css`.

### 5.5 Test strategy

Almost all behavior sits in pure functions, so the tests are cheap and exact.

1. **Transform layer (pure, TypeScript and Go).** Every Native command is a
   function from Markdown plus a range to Markdown plus a new range. Property
   tests (fast-check): apply a command and parse the result, assert the
   structural change; apply twice, assert idempotence or exact inverse;
   serialize and re-parse, assert identity. The shared Go and TypeScript
   fixtures from M4 keep rendering rules identical to the server.
2. **Source maps.** For every fixture, every rendered node maps back to a span
   and every span maps to exactly one node; presence in the preview pane and
   scroll alignment depend on this.
3. **Editor extensions.** Vitest with jsdom over CodeMirror state: which ranges
   are hidden for a given caret position, that decorations never swallow the
   caret, that keymaps produce the expected transforms.
4. **Collaboration end to end.** Playwright with browser contexts in different
   modes (one Native, one Source, one Split): edits cross, presence appears in
   the right place in each lens, offline edits flush, mode switches preserve
   caret and scroll position.
5. **Visual regression.** A dev-only gallery route renders every block kind in
   every lens in light and dark; Playwright screenshots gate changes to
   typography, spacing and color.
6. **Budgets and accessibility.** Bundle size, 2,000-block page open time,
   typing latency under 16 ms per keystroke, axe checks and a keyboard-only
   editing pass in CI.

### 5.6 The risk to name

Markdown as truth means two people formatting the same characters at the same
moment can produce interleaved markers. Block-level granularity and the
parse-serialize normalization on each side keep that rare and self-healing,
and the property tests cover it.

---

## 6. Product scope for v1

R1 as specified is the platform. v1 is the product on top of it: a complete
Logseq, a Notion replacement for knowledge work, cross-workspace references,
governed metadata, and two deployment profiles. This section is the contract
for that scope; section 7 lists the platform milestones it depends on.

### 6.1 Outcomes

A v1 user can:

- Sign in with a browser key or a wallet and create or join workspaces by DID.
- Write in an outliner or a document, in Native, Source or Split (section 5),
  alone or live with others, offline, on a laptop, in the browser and in the
  macOS app.
- Reference anything: pages, blocks, sub-documents and other workspaces, with
  live transclusion, linked and unlinked references, and a graph view.
- Model knowledge with types, typed properties, typed edges with metadata,
  collections and saved views, and find it with full-text search or CEL.
- Govern attributes: constraints, change rules, derived attributes and
  reactions declared as data over attributes and virtual collections and
  evaluated with CEL (section 6.7).
- Share at workspace, project, page or block level, with groups, links and
  publications, and see why anyone has access.
- Import a Logseq graph, an Obsidian vault or a Notion export losslessly, and
  export everything back as Markdown plus JSON.
- Let agents work through MCP under their own permissions, audited.

A v1 operator can install with one Compose file or one Helm chart, upgrade
with one command, restore from backup within the RTO, and read every SLO on
a bundled dashboard.

### 6.2 Deployment profiles and scale requirements

| Requirement | Self-host (Docker Compose) | Enterprise (Helm on Kubernetes) |
| --- | --- | --- |
| Shape | one container `KB_ROLE=all`, postgres:16, nats:2, SeaweedFS, pgBackRest | api, sync and worker Deployments with HPA; worker also scaled by KEDA on queue depth |
| Reference sizing | 4 vCPU, 8 GB | api 3 x (1 vCPU, 1 GB), sync 2 x (1 vCPU, 2 GB), worker 3 x (2 vCPU, 2 GB), NATS 3 nodes, Postgres 8 vCPU / 32 GB with one streaming replica |
| Users | up to 500 registered, 50 concurrent | 10K registered, 1K concurrent |
| Blocks per workspace | 100K comfortably | 600K at the reference sizing; workspace sharding beyond |
| Latency (p95) | write under 50 ms, query under 100 ms | the same at 200 qps |
| Ingest | 1K block updates per second | 5K block updates per second on one primary |
| Collaboration | 10 concurrent editors across 20 docs | 10K to 20K WebSocket connections per sync pod, rooms sharded on a ring |
| Sync propagation | remote keystroke visible p95 under 200 ms on a LAN, under 500 ms over a WAN | the same |
| Availability | single node; restart on failure | 99.9 percent monthly for API and sync; rolling upgrades worker, api, sync |
| Durability | RPO 5 min (WAL archiving), RTO 30 min, nightly base backup, 14-day retention | the same with CloudNativePG and Barman Cloud; quarterly restore drill |
| Identity | DIDs (browser keys, wallets) | DIDs plus an OIDC bridge that issues a custodial `did:key` per federated user (v1.1, enterprise ask) |
| Isolation | RLS plus the workspace predicate; per-workspace keys | the same, plus pgbouncer transaction pooling, replicas for reads, quotas per workspace |
| Observability | `/metrics`, JSON logs, optional OTLP | bundled Grafana dashboards and alert rules on the SLOs of specification section 13 |

Scale is achieved by mechanisms already in the design, not by later
rewrites: workspace-first keys on every table and no cross-workspace joins
(sharding path), keyset pagination only, projections rebuildable from the
log, the outbox for everything asynchronous, role-split processes, the sync
ring, GIN and trigram indexes, FGA and plan caches, per-principal and
per-workspace limits, and the k6 suites that fail CI on regression.

### 6.3 Front-end stack

One codebase for the web UI and the macOS app.

| Layer | Choice |
| --- | --- |
| UI kit | shadcn/ui on Tailwind v4 and Radix primitives, lucide icons; owned components, no design-vendor runtime |
| App | Vite, React, TypeScript, TanStack Query, react-router, CodeMirror 6, Loro, the sync client (what exists today; only the CSS layer changes) |
| Data-heavy views | TanStack Table (table view), dnd-kit (board and drag reorder), react-resizable-panels (Split lens), react-day-picker (calendar view), cmdk (command palette), virtualized lists everywhere |
| Desktop | Tauri 2 with the Go binary as a sidecar: same bundle, native window chrome, menu bar, keychain for the node key, `kb://` deep links, updater, notifications; the specification's "Electron spawns one child" becomes "Tauri spawns one sidecar" with identical behavior |

Layout follows the Plane reference: an icon rail, a contextual sidebar, a
breadcrumb header, a view switcher (list, board, calendar, table, timeline)
and a filter button; cards show type icon and key, title, priority, status
pill, date chip and avatars. Consequences: Playwright runs a WebKit project
next to Chromium (Tauri renders through WebKit on macOS), and the Go binary
is built per Apple architecture on macOS runners because of cgo.

### 6.4 Knowledge packs

**K1 Subdocs and transclusion.** Every block is addressable. `((id))` renders
the source block live inline with a reference-count badge on the source;
`{{embed ((id))}}` and `{{embed [[page]]}}` render the source subtree
editable in place (a nested editor bound to the source doc; edits go to the
source; presence crosses). Zoom into any block as a page with breadcrumbs;
shift-click opens it in a right-sidebar pane. Model: the portal node written
for boundaries becomes the general window onto another doc, one canonical
portal (the boundary: permissions, moves, breadcrumbs) and any number of
reference portals (embeds); the projection gains `portal_block_id`. Linked
references grouped by page with breadcrumb context, unlinked references from
search minus linked, both live. A reference renders only if the viewer can
view the target doc, otherwise a placeholder showing nothing but the id;
writing through an embed requires edit on the target.

**K2 Logseq completeness.** Outliner operations (collapse state as a block
property, multi-select, move up and down, drag, zoom), pages (aliases,
`a/b/c` namespaces with namespace pages, page properties, tags, all pages,
favorites, recents), journals (scheduled and deadline with repeaters, future
dates, the scheduled-and-deadlines section), task markers and priorities,
typed properties with autocomplete and property pages, simple queries in full
with a table view and a query builder, advanced Datalog queries translated
for the common patterns with the raw text preserved, templates with
variables, right-sidebar panes, global and local graph view (sampled beyond a
few thousand nodes), full-text and fuzzy search, keyboard-first everything,
lossless Logseq graph import and export. Out of v1 by the specification:
whiteboards (Canvas, R2), flashcards, PDF annotations, plugins.

**K3 Notion replacement.** Nested page tree with icons and covers, rich
blocks (toggle, callout, code, math, table, image, file, embed, table of
contents; synced blocks are embeds), databases as types with typed
properties, views (table, board, list, calendar, gallery, timeline) with a
filter, sort and group builder compiling to CEL, inline view blocks,
relations, rollups and formulas as derivations over attributes and collection
members (section 6.7), per-type templates, Notion
export import (pages, databases to types and views, assets), a responsive
layout for reading and basic editing on phones. Comments, mentions and
notifications ship as v1.1: they need the M5 pipeline and their own model.

**K4 Organization and cross-workspace references.** Five orthogonal axes,
all queryable and all in the sidebar: hierarchy (workspace, project, page
tree, namespaces), classification (types with inheritance, tags), attributes
(typed properties and edges), virtual collections (a block is in every
collection whose predicate it matches, section 6.7), and views (saved CEL
queries with a layout, placeable as blocks and pinnable as smart folders);
the reference graph is the emergent sixth.
Cross-workspace references keep the invariants: no cross-workspace joins or
foreign keys, RLS unchanged, no content copied between workspaces.
Addressing by `kb://{workspace}/{block}`; wikilinks accept a qualifier such
as `[[acme::Page]]`. The source workspace stores the target as a URI in an
external-edges table; backlinks in the target workspace are opt-in per
workspace pair and shown only after a view check on the source; titles
resolve at read time under the viewer's permission. Rendering and editing
open the target doc through a sync connection to the target workspace (the
client pools connections per workspace; the node holds every key). Search
and views fan out per accessible workspace under each workspace's access
filter, merged by sort key with composed keyset cursors; CEL gains
`workspace in [...]`. Moving a page between workspaces is an export and
import with a redirect table; publishing renders a foreign embed only when
that content is published too; agent tokens need scopes in both workspaces.

**K5 Permissions surface.** Scheme editor (roles, permissions, conditions), a
share dialog on every page and block (people, groups, roles, an inherit
toggle mapping to `Restrict(inherit)`, share links, publish), guests through
doc-level grants without membership, groups administration, "who can see this
and why" backed by Explain, an audit viewer, revocation closing open editors
within seconds, agent consent screens, a cross-workspace grants view.

**K6 Importers and exporters.** Logseq (ids, properties, queries, assets,
config), Obsidian (wikilinks, aliases, callouts, frontmatter, embeds), Notion
(HTML or Markdown plus CSV zip); export as a Logseq-compatible graph, plain
Markdown and JSON, deterministic across runs.

**K7 Graph view and sidebar panes.** Global and local graphs from
`block_edges` and `pages`, sampled and filterable by type, tag and date;
right-sidebar panes for pages, blocks, references and the graph.

**K8 Comments and notifications (v1.1).** Block-anchored threads, mentions,
resolution, a notification inbox fed by the events pipeline.

### 6.5 Edges, metadata and CEL search

Confirmed: the model supports rich edges and metadata, CEL searches over both,
and CEL governs attribute changes. Edges and search are here; governance is
section 6.7, because it rests on one more idea, the virtual collection.

**Edges with metadata.** Today an edge is `(source, target, kind,
relation_type_id, origin)` projected from a relation property stored in the
doc as a map of target id to `true`. v1 makes the map value a nested map of
edge properties (`weight`, `role`, `since`, `until`, `note`, anything the
relation type declares), so edge metadata is truth in the CRDT and projects
to `block_edges.props` (JSONB with a GIN index). Relation types gain an
`edge_props` schema (typed like block properties), keep their inverse names,
symmetric and DAG flags, and can restrict source and target types. When an
edge needs to be a first-class thing (comments, history, its own permissions),
it is a block of a join type with two relations, which the model already
allows.

**CEL over edges and metadata.** The query environment adds
`edges_out(rel)` and `edges_in(rel)` as lists of `Edge{target, props}` so
predicates such as `edges_out('depends_on').exists(e, e.props.weight > 3 &&
e.target.props.status != 'done')` compile to EXISTS semi-joins over
`block_edges` with JSONB predicates, and `has_edge(rel, id)` stays. Property
and edge metadata, system metadata (`created_by`, `updated_at`, `key`,
`uri`, `path`, `depth`), full-text `search()` and cross-workspace scopes are
all one environment; saved views, MCP `search_blocks` and the rules of
section 6.7 share it. Every expression compiles to parameterized SQL under the
cost caps of specification section 8.

### 6.6 Sequencing and size

| Track | Weeks | Depends on |
| --- | --- | --- |
| Platform M4 to M14 | 21 | as planned |
| UI-0 shadcn foundation and Tauri shell | 2 | none |
| E1 editing surface | 3 | M4 |
| K1 subdocs, transclusion, reference panels | 3 | M4, M5 |
| K2 Logseq completeness | 4 | K1, M9 |
| K3 databases and views | 6 | M9, R1 |
| K4 organization and cross-workspace references | 3 | M6, M9 |
| K5 permissions surface | 2 | M6, M10 |
| K6 importers | 3 | M11 |
| K7 graph view and sidebar panes | 1 | K1 |
| R1 virtual collections, rules and the resolver (section 6.7) | 5 | M5, M9 |
| K8 comments and notifications (v1.1) | 2 | M5 |

About 55 engineer-weeks by the specification's estimating style; platform,
editor and knowledge tracks run in parallel. The order that shows value
earliest: UI-0, M4, E1, K1, M5, M6, K5, M9, R1, K3, K2, K4, M10, M11 with K6,
M12, K7, M13, M14.

### 6.7 Work items, virtual collections and the resolver

**Two nouns.** There are work items, and there are virtual collections;
nothing else exists. A work item is a block: a type, attributes (the schema
calls them properties, the API `props`), Markdown content and edges.
`status` is an attribute like `due` or `assignee`; no part of the platform
knows what a status is. A virtual collection is a block too, whose type
declares a membership predicate: a CEL expression evaluated per candidate
item, with `props` the item's attributes and `self` the collection, that
says which work items are in it. Membership is never stored, it is computed.
An epic is a custom collection type: a block with a title, an owner and a
target date, and the predicate `props.epic == self.id`. A sprint, an area, a
release, a team backlog, an objective and an overdue list are the same thing
with a different predicate. Everything below is rules over attributes and
collections.

**No hierarchy construct.** Nesting emerges from attributes. An epic is in an
initiative because the epic's `initiative` attribute points to it; the
initiative's members are epics, the epic's members are stories. Any depth,
many at once (delivery: initiative, epic, story; ownership: team, person;
planning: quarter, sprint), and an item is in every collection whose
predicate it matches. Moving an item between collections is changing an
attribute, so it is governed like any other change. The one structural
hierarchy the platform keeps is the document outline, the CRDT tree that
text editing needs; rules see it as `parent` and `path`, and page trees and
namespaces (K2, K4) are built on it. Everything else is a collection.

**Anonymous groups.** A view that groups by an attribute (`status` columns
on a board, `area` sections in a list) produces one anonymous collection per
distinct value at render time: no block, no attributes, no rules. When a
value needs identity, an owner, a description or a rollup, it is promoted to
a collection block with the predicate `props.area == 'sync'`. Two levels,
never a third.

**Keyed and query-time collections.** The schema compiler classifies every
collection type. A predicate is keyed when it contains an equality between an
attribute and `self.id` or a constant that an index can serve
(`props.epic == self.id`, `props.area == 'sync' && props.status != 'done'`);
membership is then a lookup in `block_edges` or `block_properties`, and
rollups over it are maintained incrementally. Any other predicate
(`props.due < now && props.status != 'done'`) is query-time: membership and
rollups are computed when read, through the M9 compiler, and never
materialized. The schema editor shows the class beside the type, and a
materialized rollup over a query-time collection is refused at save time
with the reason.

**Rules.** Rules bind to attributes, never to a kind of attribute. One
language (the M9 CEL environment plus `self`, `old`, `change`, `principal`,
`members`, `in(rel)`, `members_of(id)` and `resolved`) and four moments to
run, which are the four kinds:

| Kind | Declared as | Runs | On failure |
| --- | --- | --- | --- |
| Constraint | `when: type == 'task'`, `assert: has(props.due) \|\| props.status != 'in_progress'` | before commit, on the post-write state | write rejected with `SchemaViolation` naming the rule and the attribute |
| Change | `on: change('status')`, `from: ['todo']`, `to: ['in_progress', 'canceled']`, `guard: has(props.assignee)`, `require: ['due']`; `from` and `to` are predicates over the old and new value of any attribute | before commit, when the named attribute changes | `FailedPrecondition` listing the values this principal may set, so the UI offers exactly those |
| Derivation | on a collection type: `set: progress`, `expr: avg(members.map(m, m.resolved.progress))`; on an item type: `set: area`, `expr: props.area ?? in('epic').resolved.area` | by the indexer (materialized) or at read time (query-time collections), never in the write path | cannot fail: type errors are caught at save, an evaluation error yields null with a provenance note |
| Reaction | `on: change('status')`, `when: props.status == 'done'`, `do: [set('completed_at', now), add_edge('done_by', me())]` | after commit, by the worker, as the automation principal | logged and retried; loops are cut by a depth limit |

Two more change rules show that nothing is special: `on: change('epic')`,
`guard: in('epic').props.area == props.area` keeps a story in its area when
it moves between epics; `on: change('priority')`, `to: ['p0']`, `guard:
principal.role in ['lead', 'admin']` reserves P0. A workflow is the set of
change rules on `status`, and the "allowed transitions" menu is
`RulesService.AllowedValues(block, attr)`, which answers for any attribute.
Actions in reactions are a closed set: set or unset an attribute, add or
remove an edge, set the type, move, append a block from a template, publish,
notify; external webhooks stay R2 as the specification says.

**Rules over collections.** A rule on a collection type sees `members`; a
rule on an item sees the collections it is in through its relation
attributes (`in('epic')`, `in('sprint')`: the block for a single-valued
relation, a list otherwise) and can name any collection with
`members_of(id)`. Rollups are derivations on collection types, inheritance
is a derivation on an item type reading `in(rel).resolved.x`, and a
cross-collection constraint is `assert: props.status != 'done' ||
members.all(m, m.resolved.status == 'done')` on the epic. Because
collections nest through attributes, an initiative's progress reads its
epics' resolved progress, itself a rollup: derivations form a dependency
graph over `(type, attribute)` pairs, which the compiler stratifies,
rejecting a cycle at save time with the cycle named.

**Resolved attributes.** Truth is `props`, in the CRDT. The resolver adds
`resolved`: every attribute of truth plus the derived ones, computed by the
indexer into `blocks.resolved` (JSONB) with provenance in
`resolution_provenance(block_id, attr, rule_id, sources, computed_at)`.
Reads return both, the query environment exposes `resolved.x`, rules read
`resolved` (so a constraint may rely on a rollup) and write only their own
derived attribute, or, for reactions, truth through `SetProperties`.

**Evaluation is incremental and confluent.** Derivations are pure functions
of truth and the rule set, so the result never depends on the order in which
changes arrive. When attribute `a` of item `x` changes, the index job
recomputes the derivations on `x` that read `a`; the derivations that read
`a` over members on the keyed collections `x` was in before and is in after
the change (both, since membership may have moved), then their dependents up
the graph; and, when `x` is itself a collection, the derivations on its
members that inherit from it. Fan-out is bounded per job (10,000 blocks);
past the bound the job marks the collection `stale` and a follow-up job
finishes. `AdminService.Recompute(project | rule)` rebuilds `resolved` from
truth, and the confluence test below holds the incremental path to it.

**Governed attributes and collaborative edits.** A CRDT edit cannot be
refused after the fact without diverging clients, so an attribute that has a
constraint or change rule is governed: it is written only through
`BlocksService.SetProperties` (the property panel, slash commands, MCP and
reactions call it), which runs the pre-commit rules under the per-doc
advisory lock on the freshest state the node has, so two concurrent writers
cannot both pass a guard only one may pass. Lenses show governed attributes
inline and read-only. Ungoverned attributes may be typed inline as `key::
value`; the indexer validates them, sets `compliant = false` with
`invalid_props` when a constraint fails and emits `rule.violated`, so a view
or an agent can act. Authorization conditions in the scheme stay where they
are: they are the fifth place CEL runs, not a rule.

**Dry run and resolve.** `RulesService.DryRun(scope, rules, changes)`
evaluates a rule set, the current one or a candidate, against a scope (a
collection id, a CEL predicate or a list of ids), after applying optional
hypothetical attribute changes, on a repeatable-read snapshot, and streams a
report: constraint violations by rule and item, change rules that would
refuse, every resolved attribute that would differ (before, after,
provenance) and the reactions that would fire, actions listed. Nothing is
written; the run is stored in `rule_runs` with its rule-set version so the
editor can diff two runs. The same call with the current rules and no
changes is Resolve: a collection's members with their effective attributes
and provenance; `Explain(block, attr)` is the single-item form.

**Permissions.** A collection block is shared like any block; seeing it
grants its rollups. Materialized rollups count every member in the project
whatever the reader may open (a progress bar includes stories the reader
cannot see); a type may declare `rollup: visible_to_reader`, which makes its
rollups query-time and access-filtered. Provenance never lists a source the
reader cannot view, it counts it. Reactions run as the project's automation
principal and cannot write outside the project. No predicate crosses a
workspace (K4 forbids cross-workspace joins); a federated collection is a
per-workspace union at read time, never materialized.

**Interfaces.** `RulesService` (`ListRules`, `PutRules` versioned, `DryRun`
streaming, `Explain`, `AllowedValues`); collection types are ordinary
`SchemaService` types with a `collection` clause (`members`, `rollup`);
`Block.resolved_props`; CEL functions `members`, `members_of`, `in` and the
`resolved` map; tables `rules`, `rule_runs`, `resolution_provenance` and the
column `blocks.resolved`. There is no collections service and no workflow
service: a collection is a block whose members are a query, and a workflow
is rules on an attribute.

**Limits.** 500 rules per project; the expression cost caps of specification
section 8; 16 derivation strata; 10,000 blocks of fan-out per index job;
100,000 items per dry run, streamed; `members` pages like any list.

**Tests that define done.** Confluence: random rule sets and random write
sequences over a seeded project, and after every step the incremental
`resolved` equals `Recompute` from scratch. Safety: no committed state
violates an enabled constraint, and a refused change leaves no trace in
truth, projection or outbox. Membership: for every keyed collection the
index lookup equals the predicate evaluated in-process. Dry-run fidelity:
applying a report's hypothetical changes for real reproduces the report.
Isolation: no resolved value or provenance source crosses a project.
Performance: a status change on a story under an initiative with 10,000
descendants updates the rollup chain within the M5 index-lag SLO.

**Why this is the clean design.** One noun exists (the work item), one is
derived (the collection, a predicate), one language (CEL), one strict write
path (`SetProperties`), one projection (`resolved`) and one report (the dry
run). Nothing in the platform knows what an epic, a sprint or a status is;
only the schema a project wrote does.

---

## 7. The remaining milestones

Each section gives the goal, what already exists, the design that should be
followed, the interfaces to produce, the tests that define done, and the
traps. Effort figures are the specification's estimates for one engineer.

### M4 — Tree materialization and the Markdown engine (3 weeks)

**Goal.** A deterministic Markdown engine (§9): parser producing an
mdast-compatible AST, canonical serializer, byte-exact raw nodes for anything
unmodelled, page-format rules (markdown vs outliner), and a shared conformance
fixture set proving the Go and TypeScript sides agree. Plus the tree
materialization suite: random concurrent create/move/delete sequences on the
Loro tree materialize to the same block tree in Go and TypeScript.

**What exists.** `internal/markdown/ast.go` (node types, `Walk`, `Equal`),
`unescape.go`, the CommonMark JSON cases; `api.NaiveSplitter` (line-based
page-format rules) and `projection.PlainAnalyzer` as the two seams to
replace; in the web app, `remark`/`rehype` render blocks and a small plugin
turns `[[Title]]` and `#tag` into links.

**Design to follow.**

- Parser: goldmark with extensions is the specification's choice, and it is
  the right one for CommonMark/GFM conformance; write the extensions for the
  Logseq/Obsidian dialects (`key:: value`, `[[page]]` with `|alias` and
  `#heading`, `((block))`, `{{embed}}`, `{{query}}`, `^block-id`, callouts,
  `TODO/DOING` markers, `SCHEDULED:/DEADLINE:`, math, YAML frontmatter) and a
  converter from goldmark's AST to the mdast types in `ast.go`. MDX nodes
  (`mdx_esm`, `mdx_jsx_flow`, `mdx_jsx_text`, `mdx_expression`) are parsed
  into raw-source-carrying nodes; nothing evaluates JSX. An earlier attempt at
  this lives outside the repo and did not compile; start from `ast.go` and
  keep the goldmark dependency isolated in one subpackage.
- Raw nodes: any construct the converter does not model becomes a `raw`
  node holding its exact source bytes (including surrounding blank lines) so
  parse → serialize is byte-identical for it. This is the property the
  round-trip suite tests, and it is what makes imports lossless.
- Serializer: one canonical form per node (ATX headings, `-` bullets,
  backtick fences, aligned pipe tables, `key:: value` properties after the
  first line in outliner format, frontmatter in markdown format). Determinism
  is a test, not a hope: serialize twice and compare.
- Page formats: `markdown` (every top-level node is a block; headings nest
  content by level) and `outliner` (every list item is a block; nested items
  are children). Implement them as functions from an mdast document to
  `[]*api.BlockSpec` and back, and make `api.Splitter` an adapter over them.
- Analyzer: implement `projection.Analyzer` on the same parser: plain text
  for `tsv`, inline refs (wikilinks, block refs, tags, embeds) as
  `projection.Ref`s, and `key:: value` lines as inline properties. Replace
  `PlainAnalyzer` in `internal/app/wire.go`.
- Shared fixtures: a directory of `.md` inputs with expected mdast JSON and
  expected canonical output, consumed by Go tests and by a vitest suite in
  `web/` (the web app's remark pipeline must produce the same block split
  and the same link set; rendering details may differ).
- Tree materialization suite: a generator of random op sequences applied
  through `internal/loro` and through `loro-crdt` in Node, comparing the
  resulting block trees (order, parents, content) after exchanging updates.
  This is also the FFI cross-version test once a second Loro version is
  pinned.

**Done when.** CommonMark 0.31, GFM and MDX suites pass parse → serialize →
parse identity; a Logseq/Obsidian corpus round-trips; `PagesService.Create`
with `markdown` splits into blocks by the format rules; the indexer extracts
refs and properties; the materialization suite is green in both runtimes.

**Traps.** goldmark's AST loses source positions for some inline constructs;
capture byte spans while converting. Do not store the AST; `content` stays
Markdown source. Headings inside outliner pages are ordinary blocks.

### E1 — Editing and collaboration surface (3 weeks, after M4)

**Goal.** The lenses and presence design of section 5: a Native lens with live
preview, the Split lens with block-aligned scroll, the Read lens, mode
switching, the presence rail and follow mode, and the test harness that keeps
them honest.

**What exists.** The Source lens (`web/src/app/editor/BlockEditor.tsx`), the
CodeMirror to LoroText binding, remote carets from stable Loro cursors,
per-block presence chips, the sync pill, the sanitized Markdown renderer, the
design tokens.

**Deliverables.**

- A `transforms` module (pure functions, shared fixtures with Go): toggle
  emphasis and strong, set heading level, toggle task, wrap in quote or
  callout, set list kind, insert link or wikilink, with property tests.
- CodeMirror extensions for live preview: syntax hiding around the caret,
  inline widgets (checkbox, link, image, inline code), block widgets (table,
  math, code with language), all mapped through the M4 source maps.
- Slash menu, selection bubble, block handles with drag reorder (moves are
  `tree.move`), Ctrl/Cmd-K palette.
- Split lens with block-keyed scroll sync; Read lens for viewers; mode switch
  with per-page memory and `?view=`.
- Presence rail in every lens, follow mode, caret mapping into the preview
  pane through source maps.
- Dev gallery route and Playwright visual regression in light and dark; the
  mixed-mode collaboration scenario in the e2e suite; budgets in CI.

**Done when.** A page can be written entirely in Native without seeing
Markdown syntax, the same page edits identically in Source, Split keeps both
panes aligned under remote edits, the visual snapshots are stable, and the
initial JS budget still passes.

**Traps.** Never let a lens hold state the model does not have (collapsed
state, for example, belongs to the doc as a block property or to the user's
local view, never to the DOM). Keep every command a text transform; the moment
a command edits the AST directly, the two lenses diverge.

### M5 — Projections and the worker (3 weeks)

**Goal.** Move projection work off the request path onto a durable pipeline
(§7 "Projection pipeline"): outbox poller (leader-elected through NATS KV),
NATS publishing of events, River jobs for indexing (coalesced per doc),
compaction (snapshots, shallow snapshots, pruning), quarantine of unimportable
updates, retention jobs, `AdminService.Reindex`, and `EventsService.Subscribe`
with replay from the outbox.

**What exists.** `outbox` rows written in every write transaction;
`projection.IndexDoc` (synchronous, idempotent, boundary-aware); the sync
hub's debounced indexer; `docs.indexed_seq`; snapshot APIs in `truth`;
`KB_RIVER_DATABASE_URL` and `KB_NATS_URL` in config (validated, unused).

**Design to follow.**

- Poller: `SELECT … FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT n
  FOR UPDATE SKIP LOCKED`, publish to NATS subject `events.<workspace_id>`,
  enqueue River jobs, mark published. One leader through a NATS KV lease;
  every process can run it, one does.
- Index job: keyed by `(workspace, doc)` with River's unique-job option so a
  burst yields one run; loads the live doc through the materializer (advisory
  lock), calls `IndexDoc`, writes `indexed_seq`. Keep the synchronous index in
  the API for now (reads after writes stay consistent for the writer) and
  make the sync hub hand over to the queue when `Role.RunsWorker()` is true;
  removing the synchronous path is a later optimisation measured against the
  write-latency SLO.
- Compactor: when `current_seq - snapshot_seq > 500` or the tail exceeds
  2 MB, export a snapshot (full daily, shallow otherwise), then prune
  `doc_updates` older than 30 days that are covered, in batches of 1,000 by
  primary-key range. An update the compactor cannot import is marked
  `quarantined_at` and skipped, with an alert metric; later updates still
  apply.
- Retention: `outbox` (7 days after publish), `idempotency_keys` (24 h),
  trash purge (30 days: `truth.PurgeDocs` + projections), snapshot thinning
  (one per day for 90 days, named forever). Partition maintenance for
  `outbox` and `audit_log` (create tomorrow's partition, drop expired ones).
- Events: `EventsService.Subscribe(project_id?, kinds[], since_cursor?)`
  streams from NATS with replay from the outbox for up to 7 days; heartbeat
  every 15 s. Cursors are outbox ids.
- Reflected relations (M6 dependency): the index job is where
  `block:X#assignee@user:U` tuples are written when `props.assignee` changes;
  design the job so it emits property deltas even before FGA exists.
- Resolver (section 6.7): the same index job recomputes `blocks.resolved` and
  its provenance from the property deltas; the lookup from a changed
  attribute to the derivations that read it lives in the compiled rule set,
  not in SQL, and fan-out beyond 10,000 blocks continues in a follow-up job.

**Done when.** Index lag p95 < 1 s under the k6 write suite; a killed worker
loses nothing (jobs are transactional); compaction keeps every retained seq
decryptable; `Reindex` rebuilds a workspace from the logs; the isolation suite
still shows zero cross-workspace rows.

**Traps.** River and the app share Postgres but not the RLS role: run River
as the owner role in the `river` database. Never index from a room's in-memory
doc; always from the materializer under the lock.

### M6 — Authorization with OpenFGA (3 weeks)

**Goal.** Replace `RoleGuard` with an FGA-backed guard (§6): one OpenFGA
store per workspace with the compiled model, tuples written for every
membership, project, doc, boundary, grant and publication event, `doc_access`
and `principal_sets` maintained from the FGA changelog, `PermissionsService`,
`PolicyService` (scheme versions, `Explain`), conditions with check-time
context, and access revocation closing sync subscriptions.

**What exists.** `internal/policy` (compiler, validated by the OpenFGA
typesystem; default scheme; Explain annotation), `authz.Guard` with the
`Provisioner` and `ViewSets` seams, `schemes` and `doc_access` tables,
boundaries in the API (`toBoundary` writes the portal node and the boundary
doc; `has_acl` in projections), `Hub.CloseDoc`.

**Design to follow.**

- Embed OpenFGA as a library (`github.com/openfga/openfga`, already a
  dependency) with its Postgres datastore on `KB_FGA_DATABASE_URL`; run its
  migrations at startup like ours. One store per workspace, created by
  `Provisioner.ProvisionWorkspace` with the compiled default scheme; store id
  and model id are stored on `workspaces` (`current scheme_version`).
- Tuple writes follow the table in §6 "Tuples the platform writes" and happen
  in the same code paths that emit the corresponding events; write them
  after the database transaction commits and make them idempotent (FGA
  rejects duplicate writes; treat that as success). A reconciler job
  (`AdminService.Verify`) compares tuples against `workspace_members`,
  `docs.parent`, `publications` and repairs drift.
- Guard: `Check` maps `authz.Permission` and `Resource` to
  `Compiled.PermissionRelations[type][permission]` and calls FGA with the
  agent and, for agent tokens, the owner (both must pass). Block resources
  check `block:X` when `has_acl` else the doc. Keep a short positive cache
  keyed by `(principal, object, relation, model_id)` and invalidate it on ACL
  events for the workspace.
- Conditions: the API supplies the context roots (`block.props.*`,
  `block.type`, `doc.kind`, `principal.kind`, `principal.did`, `now`) from
  projections at check time; `policy` already declares them as `map<any>`
  parameters (the DSL text is parsed with a placeholder and the model
  widened, because the OpenFGA DSL grammar has no `any` type; keep that
  workaround until upstream adds it).
- `doc_access`: a worker tails FGA `ReadChanges` per store and recomputes,
  for every affected doc, the usersets that grant view and edit
  (`project:P#viewer`, `doc:D#editor`, `user:U`, `public`). `principal_sets`
  is computed at session start and refreshed on ACL events. `Guard.ViewSets`
  returns the caller's set ids; the query compiler adds
  `doc_id IN (SELECT doc_id FROM doc_access WHERE set_id = ANY($sets) AND
  permission = 'view')`.
- `PolicyService.UpdateScheme(scheme, if_version)`: validate and compile,
  write the new model to the store, store the scheme version, migrate role
  tuples whose relation names changed, refuse anything that removes the last
  admin. `Explain` calls FGA Expand and `policy.AnnotateExpand`.
- Revocation: on grant removal or membership removal, publish an ACL event;
  the hub calls `CloseDoc` for affected docs, the web client's `closed` event
  already handles it.

**Done when.** The scheme-compiler fixtures (200 schemes, positive and
negative) pass; the access-projection test (10K random grants,
`doc_access` equals `Check` for sampled pairs) passes; the isolation suite
covers grants, boundaries and revocation; a revoked viewer's open page closes
within seconds.

**Traps.** FGA changelog lag makes `doc_access` eventually consistent; keep
`Check` as the gate for writes and sync opens and alert when projection lag
exceeds 5 s. Do not model per-block relations beyond boundaries and reflected
properties; the doc is the unit.

### M9 — Query engine (2 weeks)

**Goal.** `QueryService.Query/Validate/Explain` (§8): CEL over the block
projection compiled to one parameterized SQL statement, access-filtered,
keyset-paginated, cost-capped, with a differential test against in-process
evaluation, saved views (`view_programs`), and the Logseq simple-query
translator used by the importer.

**What exists.** `internal/query`: environment and type provider per project
schema, macros restricted to `has/exists/all`, in-process `Program` with
`Eval` and `Explain`, and the SQL generator internals covering the symbol
table of §8 with the two-valued semantics documented in `doc.go`. Missing: an
exported compile entry returning SQL plus parameters, `order_by` and cursor
handling, the cost estimate, the plan cache, the service, tests.

**Design to follow.**

- Public API: `Env.Compile(src, CompileOptions{ProjectID, Caller, Now,
  ViewSets, OrderBy, PageSize, Cursor}) (*Statement, error)` returning SQL,
  args, the resolved ids and a cost; `Statement.Next(lastRow)` producing the
  next cursor. Cursors encode the sort keys and id, are opaque (base64 of a
  small proto or JSON) and carry the schema version so they expire with it.
- Service: parse and check per request behind a 10-minute cache keyed by
  `(project_id, schema_version, cel)`; wrap with the access predicate; `SET
  LOCAL statement_timeout` 2 s (5 s for admins); run on the replica DSN when
  configured; return `Block` rows through the same expansion used by
  `PagesService.Get`, `next_cursor`, `indexed_seq` (minimum across docs) and
  `stats`. `include: EDGES` joins `block_edges`.
- Limits (§8): 200 AST nodes, `in` lists ≤ 1000, page size ≤ 500, cost ≤ 50
  units, 8 concurrent queries per principal (`rate_limits` or a semaphore).
- Collections and resolved attributes (section 6.7): `members`,
  `members_of(id)`, `in(rel)` and `resolved.x` belong to the environment from
  the start; `members` of a keyed collection compiles to a lookup on
  `block_edges` or `block_properties`, of a query-time collection to a
  subquery of its predicate under the same cost caps.
- Differential test: seed a workspace (start with a few thousand blocks;
  the 100K corpus belongs to the nightly suite), evaluate every fixture
  expression in-process over all blocks and compare the set of matching ids
  with the SQL result. Include expressions written to trigger CEL error
  absorption; the package's semantics section explains why both paths agree.
- `view_programs`: a view block (type `view`) stores its CEL; the indexer
  compiles it and stores the checked AST and SQL template; a schema change
  re-checks stored programs and marks `needs_recheck`.
- Logseq translator: `{{query …}}` clauses to CEL per the table in §8; an
  untranslatable clause keeps the raw text and marks the view partial.
- EXPLAIN fixtures: commit `EXPLAIN (ANALYZE, BUFFERS)` output for every
  generated shape against the seeded workspace and fail CI on a sequential
  scan of `blocks`, `block_properties` or `block_edges`.

**Done when.** The differential test and the EXPLAIN fixtures pass; the web
app's Documents and Work lists move from `PagesService.List` to `Query`
(`is_page`, `is_a('work')`), and the search box uses `search()`.

**Traps.** Never build SQL by concatenating user text, even for identifiers:
property ids are resolved at compile time and bound as parameters. `_rank`
sorts only with `search()`.

### M10 — Sharing and publishing (2 weeks)

**Goal.** Groups, share links, publications and public routes (§9
"Publishing", §4 "Invitations and share links"), and the corresponding web app
surfaces (share dialog, publish toggle).

**What exists.** Invites (`MembersService.Invite/AcceptInvite`, web accept
route), `groups`, `group_members`, `share_links`, `publications`,
`publication_redirects` tables, proto for `GroupsService`,
`ShareLinksService`, `PublicationsService`, `Redeem` already on the public
list of the auth interceptor, the M4 engine for HTML rendering.

**Design to follow.**

- Groups: CRUD plus `group:G#member@user:U` tuples; a group can hold a
  role at any level like a user (`group#member` is already in every direct
  assignment list of the compiled model).
- Share links: a token (`kbs_…`, hashed at rest) naming a resource and role
  with expiry and use count; `Redeem` mints a short session for a
  `link` principal (kind `link`, owner = creator) and writes the grant tuple
  for that principal; `allow_anonymous` decides whether a signed-in DID is
  required. Revocation deletes the tuple and the sessions.
- Publications: `Create(page_id, slug, mode, include_children, expires_at)`
  writes `doc:D#viewer@user:*` for the page doc and each included child doc
  (restricted boundaries excluded unless they carry their own public
  grant); `snapshot` mode pins `pinned_seq` per doc; slug changes keep a 301
  for 90 days; an hourly job revokes expired publications.
- Public routes on the API role, unauthenticated, edge-cacheable:
  `/p/{workspace_slug}/{slug}` (HTML from a fixed template with `b-{block_id}`
  anchors, canonical link to `.md`), `.md`, `.json`, `/r/{block_id}`
  (302 when published, else 401 unless authenticated), `/a/{asset_id}`.
  Every response carries `X-KB-Seq`. HTML is rendered from the mdast with an
  allowlist; raw HTML nodes are escaped; CSP `default-src 'none'; img-src
  'self'`. Live publications read the projection (through `wait_for_seq`
  semantics or a 60 s cache); snapshots read `ReadAt(pinned_seq)`.
- Web app: share dialog (invite by DID, share link creation, grants list),
  publish toggle showing the public URL.

**Done when.** A published page renders without JavaScript, the `.md` form
is the canonical serialization, revocation returns 404 within the cache TTL,
share links grant exactly the role and expire; the isolation suite includes
public routes.

### M11 — Import and export (2 weeks)

**Goal.** `PagesService.ImportFolder` as a job with a fidelity report,
`ExportMarkdown` and `ProjectsService.Export`, `AssetsService` over the
encrypted object store, and asset upload from the web app (§9 "Import",
"Export").

**What exists.** `internal/objectstore` (untested), `uploads`, `assets`,
`import_jobs` tables, `KB_OBJECT_STORE_DIR` and `KB_S3_*` config, the M4
engine and the M5 job queue as dependencies.

**Design to follow.**

- Wire `objectstore` with the key ring as its `DEKWrapper`; add the tests
  the package lacks (round trip, range reads, tamper detection, both
  backends) and the CI plaintext scan (read raw objects behind every write
  path and fail on recognizable plaintext).
- Uploads: `AssetsService.CreateUpload` returns an upload id and a
  same-origin `PUT /upload/{id}` route (the body limit middleware already
  exempts `/upload/`); the handler streams into the store under purpose
  `asset` (or `import` for archives), records sha256 and size, and
  deduplicates assets by content hash per workspace.
- Import job: unzip from the store, classify files, create property
  definitions on demand with inferred kinds, create pages by file name and
  Logseq namespace rules, split with the page-format rules (outliner only
  when the body is a single top-level list unless forced), honor `id::` and
  `^id` as block ids, upload assets and rewrite links, second pass to resolve
  `[[links]]`/`((refs))` and create missing pages, translate simple queries,
  publish `public:: true` pages when asked. A file that fails to parse
  becomes one raw block and is flagged. Write the report per file into
  `import_jobs`.
- Export: deterministic zip in the importer's layout (`pages/`, `journals/`,
  `work/`, `assets/`, `views/`), one `.md` per doc-owning block with
  frontmatter `type:` and `key:`; property ids written as names.
- Web app: drag-and-drop image upload in the editor inserting
  `![](kb-asset:…)`, rendered through `/a/{asset_id}`.

**Done when.** A Logseq vault and an Obsidian vault import with a
reproducible report and export byte-identically twice; the 10K-file
benchmark stays under 10 minutes (nightly); every object in the bucket has the
envelope header.

### M12 — MCP and OAuth 2.1 (2 weeks)

**Goal.** An MCP server at `/mcp` (streamable HTTP) with the tool set of §10
generated from the protobuf messages, OAuth 2.1 authorization server with
consent (SIWE or local key in the browser), agent-token authentication, scope
filtering, response caps, audit and throttling.

**What exists.** Agent tokens with scopes and tools (`AgentsService`,
`auth.KnownTools`), the double-principal `Identity`, `oauth_clients`,
`oauth_codes`, `oauth_refresh_tokens` tables, audit log, rate limits.

**Design to follow.**

- Tool schemas: generate JSON Schema from the `kb.v1` request messages
  (protoreflect at startup) for the tools in §10; each tool is a thin
  adapter calling the same service implementations in-process with an
  `auth.Identity` for the agent, so permissions, idempotency and events are
  identical to the API. Write tools are enabled only when the token's scopes
  include a write role and `tools[]` lists them.
- Transport: JSON-RPC over POST with SSE responses per the MCP streamable
  HTTP spec; `resources/list` enumerates viewable projects and root pages;
  `kb://{workspace}/{block}` resources return Markdown; subscriptions bridge
  to `EventsService`.
- OAuth AS inside api: `/.well-known/oauth-protected-resource/mcp` and
  `/.well-known/oauth-authorization-server`, `/oauth/authorize` (minimal
  HTML: sign in with a wallet or the local key, then consent listing
  workspace, projects, roles and tools), `/oauth/token` with PKCE, RFC 8707
  resource indicators, tokens audience-bound to this node's MCP URL, Client ID
  Metadata Documents plus dynamic registration for older clients. Consent
  mints an agent principal owned by the user; revoking the agent revokes the
  grant. Reuse `auth.TokenIssuer` for access tokens (1 h) and the refresh
  store for refresh tokens (30 d).
- Safety: 1 MB response cap with truncation cursors, `X-Request-Id`,
  audit rows with token id, owner, tool, resource ids, seq and duration,
  per-token rate limit, 10 failures per minute disables the token for 15
  minutes and emits `agent.throttled`.

**Done when.** Claude Desktop, Cursor and a scripted client complete the
OAuth flow and use `search_blocks`, `get_page`, `append_blocks`; an agent
never sees a block its owner cannot; every call is in the audit chain.

### M13 — Kubernetes (2 weeks)

**Goal.** A Helm chart with CloudNativePG, NATS and SeaweedFS sub-charts
(each switchable to external), role Deployments with HPA (api, sync, worker)
and a KEDA scaler on River queue depth, pgbouncer in transaction mode,
Ingress with WebSocket support, PodDisruptionBudgets, dashboards and alert
rules, backups (Barman Cloud with SSE-C keyed from the node key) and a
scripted restore drill (§13).

**What exists.** The Compose bundle (`deploy/compose`), a Dockerfile producing
one image, role selection by `KB_ROLE`, `/healthz`, `/readyz`, `/metrics`,
OpenTelemetry wiring.

**Design to follow.**

- The sync ring: on Kubernetes each sync pod owns a slice of a consistent
  hash ring published in NATS KV; `OpenResponse.sync_url` (already in the
  proto and returned by the hub as `KB_SYNC_URL`) tells the browser which pod
  to connect to; a pod change disconnects clients, which reconnect and pull.
  Implement ring membership in `internal/sync` behind an interface with a
  single-node default so Compose stays unchanged.
- `AdminService`: `Health`, `Stats`, `Verify` (audit chain, snapshot decrypt
  sample, object envelope sample, FGA consistency), `RotateNodeKey`,
  `ExportNodeKey`, `ListJobs`, `RetryJob`, all under the node-operator token.
- Reference values for 10K users / 600K blocks are in §13; ship them as
  `values-reference.yaml` and run the k6 suites against them in the nightly
  pipeline.

**Done when.** `helm install` on a kind cluster passes the e2e suite and the
restore drill script restores a point in time and passes `Verify`.

### M14 — Hardening, remaining web features, docs (2 weeks)

**Goal.** Meet the acceptance criteria of §1 with the k6 suites, review the
security controls of §13 one by one, finish the R1 web scope left out of M8,
and complete the documentation.

**Contents.**

- k6 suites: Compose reference (4 vCPU: 50 concurrent users, p95 write
  < 50 ms, p95 query < 100 ms, 5K block updates/s) and the Kubernetes
  reference; the Playwright collaboration suite at full size (10 contexts,
  20 docs, 3 offline for 5 minutes, identical Markdown afterwards).
- Security review: walk the threat table in §13 and record for each control
  where it is enforced and which test proves it (several already exist:
  RLS isolation, AAD binding, refresh reuse detection, CSP on `/app`,
  sanitizer on rendering).
- Web app leftovers: `[[page]]` and `KB-123` autocomplete, backlinks panel
  (`BlocksService.ListEdges` with direction IN), search box (M9),
  version history (`ListVersions`, `GetVersion`, `RestoreVersion` already
  exist server-side), share dialog and publish toggle (M10), drag reorder,
  the specification's 300 KB initial-JS budget (move CodeMirror language
  support and the wallet path behind lazy imports), the "stale tab reloads
  after flushing" behaviour on version change.
- Nightly suites: materializer concurrency (50 writers per page across 3
  nodes), compaction and quarantine, import benchmark, access projection,
  restore drill.
- Docs: operator guide (install, upgrade, backup, restore, key export),
  API guide, MCP guide, security statement; commit the specification's final
  revision.

---

## 8. Engineering rules that keep the system honest

These are conventions the existing code follows; new code should too.

- **Tenant transactions.** Every request-path query runs inside `db.Tx` with
  a `db.Tenant`; the RLS policy is defence in depth, the explicit
  `workspace_id` predicate is mandatory. Maintenance uses `db.System`.
- **Writes go through the materializer.** No service writes `doc_updates` or
  projections directly; `Options.OnCommit` is where projections and
  idempotency records join the transaction.
- **Events for every mutation.** Emit the `truth.Event` kind that matches
  the change (`mu.Emit`); the M5 pipeline depends on it.
- **Errors are typed.** Use `internal/apierr`; never leak SQL or key
  material in messages; `NotFound` when the caller may not learn a resource
  exists.
- **Permissions before work.** Call `Guard.Check` with the narrowest
  resource; agents are two principals.
- **Generated code is committed** (`gen/`, `web/src/gen`); change protos
  with `make proto` / `npm run gen` and commit the output.
- **Tests create their own database** (`testutil.NewDB`) and never share
  state; database tests fail, not skip, under `KB_TEST_REQUIRE_DB=1`
  (`make test`, CI).
- **Migrations are forward-only SQL** in `internal/db/migrations`, applied at
  startup under an advisory lock; add a new numbered file, never edit an
  applied one.
- **Budgets are tests**: web bundle size (`npm run budget`), limits in
  `config.Limits`, e2e timings.
- **No CLI.** Anything an operator needs is an `AdminService` RPC.
- **Commit hygiene.** One milestone or one coherent fix per commit, with the
  reasoning in the message; CI must be green on every push.

---

## 9. Known gaps, open decisions and risks

Open decisions carried from §14: contract-wallet support without an RPC
URL (currently refused), audit retention default (forever vs one year with
export), the product name behind `kb.v1` and `kb://`. New in this handbook:
the default rollup visibility (every member in the project, or
`visible_to_reader`; section 6.7).

Gaps and risks observed while building M1–3, 7 and 8:

| Item | Where | Note |
| --- | --- | --- |
| OpenFGA DSL has no `map<any>` | `internal/policy` | conditions are parsed with a placeholder type and the model is widened; the DSL text is right, only the parser lags; revisit when upstream adds `any` |
| Loro version coupling | `rust/loro-cabi`, `web/package.json` | both pinned to the 1.16 line; the FFI cross-version test (M4) must exist before the first upgrade |
| Sync single-node assumption | `internal/sync` | rooms are process-local; the Kubernetes ring is M13 |
| Synchronous indexing on the API path | `internal/api`, `internal/sync/indexer.go` | correct today, a latency cost under load; M5 decides how much moves to the queue |
| Per-IP auth rate limits and automation | `internal/auth` | the e2e runner raises `KB_LIMITS_RATE_CHALLENGE_PER_MINUTE`; production defaults are strict on purpose |
| Initial JS budget | `web/` | 362 KB gzip today against the specification's 300 KB target; M14 |
| Browser cache key | `web/src/lib/sync-client/persistence.ts` | non-extractable is an API property, not a storage one; the desktop keychain makes it real in R2 |
| `internal/query` has no public compile entry or tests | M9 | the generator internals exist; treat them as a draft to be tested, not as finished |
| `internal/objectstore` untested | M11 | do the tests before wiring |

Risks from §14 that remain live: Loro's youth and cgo build complexity;
SeaweedFS governance (the S3 API is the contract, so a store swap is a copy);
FGA changelog lag on `doc_access`; the web app growing past its scope table;
losing the node key loses backups (`ExportNodeKey` must exist before the first
production install).

---

## 10. A reading plan for the first week

1. Run it: `docs/running-locally.md`, then create a workspace and a page,
   open it in two tabs. Read `cmd/kb/main.go` and `internal/app/wire.go`
   while it runs; that file is the whole dependency graph.
2. Follow one write: `internal/api/blocks.go` (`Create`) →
   `internal/truth/materializer.go` (`Mutate`) → `internal/loro/ops.go` →
   `internal/projection/index.go`. Then follow one keystroke:
   `web/src/app/editor/binding.ts` → `web/src/lib/sync-client/client.ts` →
   `internal/sync/ws.go` → `truth.ImportUpdate`.
3. Read §3, §5, §6 and §7 of the specification with the code open; the
   invariants in section 1 of this handbook are the checklist.
4. Read `internal/policy/compile_test.go` and `testdata/default.fga` to see
   the authorization model that M6 will load into OpenFGA.
5. Pick the milestone you own and start from its "What exists" list above;
   every seam it names is already in the code.
