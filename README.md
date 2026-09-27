# Knit Spool for Go

A small, cgo-free Go service implementing the Knit Spool Protocol v1. It stores opaque encrypted frames and attachments and relays them to subscribed clients. The server does not hold decryption keys.

## Protocol source

The wire contract is the normative [Knit Spool Protocol](https://github.com/getknit/knit/blob/main/docs/SPOOL_PROTOCOL.md), protocol v1, revision `2026-08-30`. The implementation records the official Kotlin spool and conformance CLI snapshot at commit [`0c51be517db5a0ff90587c6e5045bdd019fe4b40`](https://github.com/getknit/spool/commit/0c51be517db5a0ff90587c6e5045bdd019fe4b40). Protocol behavior follows the specification; implementation-specific choices are cross-checked against that Kotlin reference in CI.

## Build and run

Requirements: Go 1.25 or later. The SQLite driver is pure Go, so no C toolchain or system SQLite library is needed.

```sh
go test ./...
go build -trimpath -o bin/knit-spool ./cmd/knit-spool
./bin/knit-spool check
./bin/knit-spool run
```

The default listener is `127.0.0.1:9470`, the database is `./data/spool.db`, and proof of work is enabled for unknown scopes. For a local smoke check:

```sh
SPOOL_POW_BITS=0 ./bin/knit-spool run
curl http://127.0.0.1:9470/healthz
```

The WebSocket route is `/spool/v1`. When a connection token is configured, clients send it as the `k` query parameter. `/healthz` is a liveness/readiness check, `/source` identifies the build and corresponding source, and `/metrics` emits Prometheus text. Metrics use `SPOOL_METRICS_TOKEN` when set, otherwise either accepted connection token; with no connection token, metrics are open.

## Configuration

Settings are environment variables. Invalid or conflicting settings stop startup.

| Variable | Default | Purpose |
| --- | --- | --- |
| `SPOOL_LISTEN` | `127.0.0.1:9470` | Listener address. A non-loopback listener requires TLS files. |
| `SPOOL_TLS_CERT`, `SPOOL_TLS_KEY` | unset | TLS certificate and private-key paths; configure both together. |
| `SPOOL_DATA_PATH` | `./data/spool.db` | SQLite database file. Use this or `SPOOL_DATA_DIR`, not both. |
| `SPOOL_DATA_DIR` | unset | Private data directory; database file is `spool.db`. |
| `SPOOL_SOURCE_URL` | this repository | Corresponding source link returned by `/source`; set it for modified builds. |
| `SPOOL_TOKEN` | unset | Optional WebSocket bearer token. An unset token makes the local service public to reachable clients. |
| `SPOOL_TOKEN_NEXT` | unset | Optional second token for rotation; requires a different `SPOOL_TOKEN`. |
| `SPOOL_METRICS_TOKEN` | unset | Separate optional metrics credential. |
| `SPOOL_POW_BITS` | `20` | PoW difficulty for unknown scopes (`0` disables the gate). |
| `SPOOL_MAX_BLOB` | `65536` | Hard frame size cap in bytes. |
| `SPOOL_MAX_RECORD` | `131072` | Maximum CBOR record size. Must fit the configured payload cap. |
| `SPOOL_MAX_SCOPES` | `64` | Maximum retained scopes. |
| `SPOOL_MAX_FRAMES` | `1000` | Per-scope frame-cap ceiling. |
| `SPOOL_MAX_TTL_MS` | `604800000` | Per-scope TTL ceiling (7 days). |
| `SPOOL_MAX_PULL` | `64` | Maximum blob IDs handled in one pull. |
| `SPOOL_MAX_ATTACH_BYTES` | `16777216` | Per-scope attachment storage budget; `0` disables attachments. |
| `SPOOL_MAX_A_CHUNK` | `49221` | Maximum attachment chunk payload. |
| `SPOOL_MAX_AGET` | `32` | Maximum chunks returned for one attachment read. |
| `SPOOL_MAX_BYTES` | `268435456` | Global payload watermark; least-recently-active scopes are shed to 90% after crossing it. `0` is unlimited. |
| `SPOOL_SWEEP_MS` | `60000` | Expiry and tombstone sweep interval; `0` disables the background sweep. |
| `SPOOL_STATUS_MS` | `300000` | Periodic status log interval; `0` disables it. |
| `SPOOL_MAX_CONNS` | `0` | Concurrent connection cap; `0` is unlimited. |
| `SPOOL_MAX_CONNS_PER_IP` | `16` | Connection cap per client address; IPv6 addresses are grouped by `/64`. |
| `SPOOL_RATE_RECORDS` | `50` | Protocol records per second per connection (burst is four times the rate). |
| `SPOOL_RATE_PUSHES` | `10` | Frame pushes per second per connection (burst is four times the rate). |
| `SPOOL_RATE_NEW_SCOPES` | `6` | New scopes per minute per client address (burst is four times the rate). |
| `SPOOL_TRUST_PROXY` | `false` | Trust the proxy-appended `X-Forwarded-For` address for per-IP limits. Enable only behind a trusted proxy. |
| `SPOOL_REQUIRE_MODERATION` | `false` | Advertise a client moderation request. The relay cannot verify client-side screening. |
| `SPOOL_COMMONS_ID` | unset | Operator-pinned commons scope ID as 64 hex characters. Obtain an invite with `knit-spool commons-invite`. |
| `SPOOL_COMMONS_NAME` | unset | Optional commons display name. |
| `SPOOL_COMMONS_MAX_FRAMES` | `500` | Pinned commons frame limit. |
| `SPOOL_COMMONS_TTL_MS` | `86400000` | Pinned commons frame TTL (24 hours). |
| `SPOOL_COMMONS_MAX_BLOB` | `SPOOL_MAX_BLOB` | Pinned commons blob-size limit. |
| `SPOOL_COMMONS_ATTACH` | `false` | Enable attachment storage in commons. |
| `SPOOL_COMMONS_RATE_PUSHES` | `20` | Commons-wide frame and attachment write rate per second (four-times burst). |

Data directories and database files are created with owner-only permissions. Back up a persistent store after stopping the service so SQLite can checkpoint its WAL.

### Credentials and commons

Store tokens in a root-readable environment file or secret manager. Avoid command-line arguments and shell history for credentials. The token is supplied during WebSocket upgrade as `?k=...`; terminate TLS before traffic leaves a trusted host. Direct public listeners must configure `SPOOL_TLS_CERT` and `SPOOL_TLS_KEY`.

Generate a commons invite once and provide it only to people authorized to join that shared scope:

```sh
./bin/knit-spool commons-invite
```

The command prints an invite secret and its derived `SPOOL_COMMONS_ID`. The server stores only the ID and never advertises the scope ID in `hello`. Keep the invite private; it is the capability clients use to discover the shared scope.

## Operations

The included systemd unit runs under a dedicated account with a private state directory. Create the service account, install the binary and unit, and make the optional environment file readable only by root:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin knit-spool
sudo install -o root -g root -m 0755 ./bin/knit-spool /usr/local/bin/knit-spool
sudo install -o root -g root -m 0644 deploy/systemd/knit-spool.service /etc/systemd/system/knit-spool.service
sudo install -d -o root -g root -m 0755 /etc/knit-spool
sudo install -o root -g root -m 0600 /dev/null /etc/knit-spool/knit-spool.env
sudo systemctl daemon-reload
sudo systemctl enable --now knit-spool
```

Edit `/etc/knit-spool/knit-spool.env` for deployment-specific values. Keep it at mode `0600`.

For upgrades, stop the service, make a database backup, replace the binary, then start it again. Startup checks SQLite integrity and creates missing tables/indexes. The service drains WebSocket connections on shutdown; clients should reconnect and resubscribe. Keep the prior binary and backup until health and client reconnection are confirmed.

## Conformance and CI

The repository tests protocol vectors, storage boundaries, persistence and file permissions, connection lifecycle, authentication, rate limits, and HTTP operations. CI also builds the daemon without cgo for Linux amd64 and ARMv6 (`GOARM=6`), runs the official Knit Kotlin conformance CLI, and compares edge-case transcripts against the pinned Kotlin reference daemon.

To run the official reference checks locally, provide the pinned [`getknit/spool`](https://github.com/getknit/spool) checkout:

```sh
JAVA_HOME=/path/to/jdk-21 scripts/verify-reference.sh /path/to/getknit-spool
```

The script builds the reference daemon and conformance CLI, starts both services on loopback with disposable stores, runs the CLI against both, and performs differential checks for eviction, tombstones, digest rejection, pull results, attachment idempotence/conflict, and unknown-record handling. It stops both services and removes its temporary data on exit.

## Maturity and license

This is an early service implementation. It has no independent security audit and should be deployed only by operators who can monitor and update it. The relay stores ciphertext and transport metadata; it does not provide end-to-end key management, moderation, user identity, or availability guarantees. See [SECURITY.md](SECURITY.md) for vulnerability reporting guidance.

Licensed under [GNU Affero General Public License v3.0 or later](LICENSE).
