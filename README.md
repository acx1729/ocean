# kb — self-hosted knowledge platform (R1)

One Go binary serving a collaborative, block-based knowledge base: Postgres for
truth and projections, Loro CRDT documents synchronised over WebSockets, DID-based
identity (browser keys or wallets), and an embedded web app at `/app`.

- Specification: [`docs/r1-specification.html`](docs/r1-specification.html)
- Running it: [`docs/running-locally.md`](docs/running-locally.md)
- Testing it: [`docs/testing.md`](docs/testing.md)
- Architecture handbook for new engineers: [`docs/architecture.md`](docs/architecture.md)
- Layout: `cmd/kb` (entry point), `internal/` (server packages), `proto/kb/v1`
  (Connect API, generated into `gen/`), `rust/loro-cabi` (Loro C ABI shim used
  through cgo), `web/` (React app and the framework-free sync client in
  `web/src/lib/sync-client`).

```sh
make web && make build && ./bin/kb   # see docs/running-locally.md for the environment
```
