# Running the R1 node locally

The node is one binary (`bin/kb`) with the web app embedded. It needs Postgres 16
and a passphrase for the node key; everything else has defaults for a laptop.

## 1. Build

```sh
make web     # web/ → internal/web/dist (Node 22)
make build   # Rust Loro shim + Go binary with the web app embedded → bin/kb
```

## 2. Postgres

Any Postgres 16 with the `ltree` and `pg_trgm` extensions available. For example,
with Docker:

```sh
docker run -d --name kb-pg -e POSTGRES_USER=postgres -e POSTGRES_HOST_AUTH_METHOD=trust -p 55432:5432 postgres:16
psql -h 127.0.0.1 -p 55432 -U postgres -c "CREATE DATABASE kb;"
```

Migrations run on start when `KB_MIGRATE=true`.

## 3. Run

```sh
export KB_PUBLIC_URL=http://127.0.0.1:8080
export KB_LISTEN=127.0.0.1:8080
export KB_DEV=true                      # plain-HTTP cookies on loopback
export KB_MIGRATE=true
export KB_DATABASE_URL=postgres://postgres@127.0.0.1:55432/kb?sslmode=disable
export KB_FGA_DATABASE_URL=$KB_DATABASE_URL     # not used yet (M6)
export KB_RIVER_DATABASE_URL=$KB_DATABASE_URL   # not used yet (M5)
export KB_NATS_URL=nats://127.0.0.1:4222        # not used yet (M5)
export KB_DATA_DIR=$PWD/.data
export KB_OBJECT_STORE_DIR=$PWD/.data/objects
export KB_NODE_KEY_PASSPHRASE='a long random passphrase'
./bin/kb
```

Then open <http://127.0.0.1:8080/app/>.

- **Sign in** with "Create a key in this browser": an Ed25519 key is generated
  and kept in this browser's IndexedDB (non-extractable when the browser supports
  WebCrypto Ed25519). Your identity is the `did:key:…` shown under the button.
- **Create a workspace**; it comes with a default project.
- **Documents** lists pages; **Work** lists numbered work items (`KB-1`, …).
  "Today's journal" opens the dated page.
- **Editing**: every page is a Loro document. Type Markdown; `Enter` splits a
  block, `Backspace` at the start merges it into the previous one, `Tab` /
  `Shift-Tab` indent and outdent, `Escape` shows the rendered block, `Cmd/Ctrl-Z`
  undoes. Open the same page in a second tab to see live collaboration and
  presence.
- **Offline**: edits keep working when the connection drops (the pill turns to
  "Offline · n pending") and are pushed when it comes back. Docs you opened are
  cached, encrypted, in IndexedDB.
- **Inviting someone**: open **Members** in the sidebar, paste their DID (they
  see it on their sign-in and workspace screens) and send them the accept link.

## Tests

```sh
make test            # Go (needs the test Postgres; see internal/testutil)
cd web && npm test   # sync client protocol tests, block tree, identity
make e2e             # Playwright against bin/kb: sign-in, two-tab collaboration, invites
```

`make e2e` starts `bin/kb` on port 8787 with a fresh database on the local test
cluster (`postgres://postgres@127.0.0.1:55432`); set `KB_E2E_ADMIN_DSN` to use
another cluster or `KB_E2E_BASE_URL` to test a node you started yourself.
