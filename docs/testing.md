# Testing locally

Everything delivered so far (milestones 1–3, 7 and 8: skeleton, identity,
truth layer, sync rooms, web app) is covered by tests that run on a laptop
with no cloud services. This page lists the toolchain, the one external
dependency (Postgres) and every test command, with what each one proves.

## Toolchain

| Tool | Version | Used by |
| --- | --- | --- |
| Go | 1.26 (`go.mod`; `GOTOOLCHAIN=auto` downloads it) | server, tests |
| Rust + cargo | stable (1.94 in the Dockerfile) | `rust/loro-cabi`, the Loro C ABI shim linked through cgo |
| Postgres | 16, with `ltree` and `pg_trgm` (both ship with the server) | every database-backed test, the e2e run |
| Node.js | 22 | web app build, unit tests, Playwright |
| Chromium for Playwright | `npx playwright install chromium` once (already present in the hosted dev container) | `make e2e` |

`buf`, `protoc-gen-go` and `protoc-gen-connect-go` are only needed to change
the API (`make tools`, `make proto`); generated code is committed.

## Postgres for tests

The Go tests create one fresh, migrated database per test through the
maintenance connection `KB_TEST_ADMIN_DSN` (default
`postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable`) and drop it
afterwards, so tests are isolated and can run in parallel.

```sh
make dev-db          # scripts/dev-postgres.sh start
make dev-db-stop
scripts/dev-postgres.sh reset   # wipe and recreate
```

The script starts a disposable cluster on 127.0.0.1:55432 with a
passwordless `postgres` superuser: with `pg_ctl` when Postgres 16 is
installed locally (data under `$TMPDIR/kb-pg`), otherwise as a Docker
container named `kb-dev-postgres`. It is tuned for tests (`fsync=off`).

Without a reachable Postgres, database-backed tests **skip** (they print
`postgres not reachable`). `make test` sets `KB_TEST_REQUIRE_DB=1` so that a
missing database fails the run instead; CI does the same.

## Go: `make lint` and `make test`

```sh
make lint            # gofmt and go vet
make dev-db
make test            # builds the Loro shim, then go test -race ./...
```

What runs, by milestone:

| Package | Milestone | Proves |
| --- | --- | --- |
| `internal/config`, `internal/db`, `internal/audit` | M1 | environment parsing and validation, migrations, RLS-aware transactions, hash-chained audit log |
| `internal/did`, `internal/keyring`, `internal/auth` | M2 | did:key and did:pkh encoding, node key sealing and workspace key wrapping, challenge/verify for Ed25519 and SIWE, PASETO sessions, refresh rotation with reuse detection, agent tokens, rate limits |
| `internal/loro`, `internal/truth`, `internal/schema`, `internal/api` | M3 | Loro op batches and snapshots through the C ABI, encrypted append-only update log, materializer and version preconditions, idempotent writes, pages and blocks services including boundaries, keys and trash |
| `internal/sync` | M7 | WebSocket protocol end to end against a real database: hello, open with snapshot or tail, push and ack, fan-out, pull after gaps, awareness relay, error codes, the Connect transport |
| `internal/web` | M8 | embedded app handler: SPA fallback, config injection, CSP, asset caching |
| `internal/policy` | M6 (groundwork) | scheme validation and compilation to OpenFGA models, golden model for the default scheme |

`make test-short` skips nothing by itself; it only sets `-short`, and
database tests still skip when Postgres is down.

## Web app: unit tests, lint, budgets

```sh
cd web
npm ci
npm run lint         # eslint + prettier
npm run typecheck
npm test             # vitest
npm run build && npm run budget
```

`npm test` covers the sync client protocol against an in-memory server
(convergence between clients, coalescing, the offline queue and its flush,
re-sending pending updates with the same client_seq, gap recovery through
pull, reopening from the encrypted cache and resuming with a tail, token
refresh after `unauthenticated`, awareness relay and departure, server-side
close), the block tree helpers (server node layout, indent/outdent, split and
merge) and did:key encoding against the W3C test vector.

## End to end: `make e2e`

```sh
make web             # or: cd web && npm ci && npm run build
make e2e             # builds bin/kb, then runs Playwright against it
```

`web/scripts/e2e-server.mjs` creates a fresh database on the dev cluster,
starts `bin/kb` on 127.0.0.1:8787 in dev mode with a throwaway data
directory and raised per-IP rate limits, and the three scenarios in
`web/e2e/app.spec.ts` drive real browsers:

1. sign in with a browser key, create a workspace and a page, type in two tabs
   and watch edits and presence cross, cut the connection and see queued edits
   flush, indent a block and reload;
2. invite a second identity from another browser profile, accept through the
   link, edit together, and see the guest's presence disappear when it leaves;
3. open today's journal and create a work item that receives a key.

Set `KB_E2E_BASE_URL` to run the same suite against a node you started
yourself, `KB_E2E_ADMIN_DSN` to use another Postgres, and `KB_E2E_PORT` to
change the port.

## One command for everything

```sh
make lint && make dev-db && make test && (cd web && npm ci && npm run lint && npm test) && make web && make e2e
```

CI (`.github/workflows/ci.yaml`) runs the same steps except the e2e suite,
against a Postgres service container.
