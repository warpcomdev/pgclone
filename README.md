# pgclone

> Copy PostgreSQL tables — including TimescaleDB hypertables — from one database to another, in batches, with parallel writes, throttling, and resume support.

`pgclone` is a small, single-binary CLI designed for database operators who need to move large tables between PostgreSQL instances reliably. It detects TimescaleDB hypertables, copies their chunks individually, retries on transient failures, and lets you resume long copies from the last successful chunk.

---

## Features

- **Batch copying** — reads and writes rows in configurable batches to keep memory usage low.
- **TimescaleDB aware** — detects hypertables and copies chunks one by one in `range_start` order.
- **Parallel writes** — uses multiple target connections to speed up ingestion.
- **Throughput throttling** — cap read speed with `-mbps` so you do not saturate the source.
- **Resume support** — restart a hypertable copy from a specific chunk with `-skip-until-chunk`.
- **Conflict handling** — choose `ON CONFLICT DO NOTHING` (default) or `ON CONFLICT DO UPDATE SET` (`-update`).
- **Retry with backoff** — automatic retries on connection, timeout, or "too many connections" errors.
- **Zero runtime dependencies** — single static Linux binary.

---

## Installation

The fastest way to install the latest release on Linux (`amd64`) is:

```sh
curl -fsSL "https://raw.githubusercontent.com/warpcomdev/pgclone/main/hacks/install.sh" | sh
```

This downloads the release archive, verifies its checksum, and installs `pgclone` into `/usr/local/bin`.

To install somewhere else:

```sh
INSTALLATION_PATH="$HOME/.local/bin" curl -fsSL "https://raw.githubusercontent.com/warpcomdev/pgclone/main/hacks/install.sh" | sh
```

### From source

If you have Go 1.25 or newer:

```sh
go install github.com/warpcomdev/pgclone/cmd@latest
```

Or clone the repository and build locally:

```sh
git clone https://github.com/warpcomdev/pgclone.git
cd pgclone
go build -o pgclone ./cmd/pgclone.go
```

---

## Quick start

Copy a single table from a source database to a target database:

```sh
pgclone \
  -source "postgres://user:pass@source.example.com:5432/source_db?sslmode=require" \
  -target "postgres://user:pass@target.example.com:5432/target_db?sslmode=require" \
  -schema public \
  metrics
```

Copy several tables at once with verbose output:

```sh
pgclone \
  -verbose \
  -source "postgres://reader:secret@source:5432/db?sslmode=require" \
  -target "postgres://writer:secret@target:5432/db?sslmode=require" \
  -schema public \
  table_a table_b table_c
```

---

## Usage

```text
pgclone -help
```

### Environment

`pgclone` does not read environment variables directly; pass connection details via the `-source` and `-target` DSN flags.

---

## TimescaleDB

For normal PostgreSQL tables, `pgclone` reads rows ordered by primary key and inserts them into the target in batches.

For TimescaleDB hypertables it additionally:

1. Queries `timescaledb_information.hypertables` and `timescaledb_information.chunks`.
2. Lists chunks in `range_start` order.
3. Copies each chunk independently, but inserts into the parent hypertable so the target TimescaleDB instance routes rows to the correct chunk automatically.

### Resume a failed copy

If a copy is interrupted, find the last successfully copied chunk from the logs and resume from the next one:

```sh
pgclone \
  -source "postgres://user:pass@source:5432/db?sslmode=require" \
  -target "postgres://user:pass@target:5432/db?sslmode=require" \
  -schema public \
  -skip-until-chunk "_hyper_1_42_chunk" \
  conditions
```

`pgclone` will skip every chunk before `_hyper_1_42_chunk` and continue from there.

---

## Conflict handling

By default `pgclone` uses `INSERT ... ON CONFLICT DO NOTHING`, which silently skips duplicate primary keys.

With `-update` it tries to use `ON CONFLICT DO UPDATE SET`, overwriting existing rows. This mode only works for tables with a single-column primary key. For composite primary keys it falls back to `DO NOTHING` and logs a warning.

---

## Limitations

- **Linux `amd64` only** for pre-built binaries. The installer supports only Linux on `x86_64`/`amd64`.
- **Primary key required** on every table you copy.
- **No config file** — all options are passed as flags.
- **DSN credentials** are shown in process listings, so avoid passing passwords inline on shared machines; use `PGPASSFILE` or `.pgpass` via `lib/pq` instead.
- **TimescaleDB chunk resume** is the only resumption mechanism; ordinary table copies currently resume only via `-offset`.

---

## Development

Build the binary:

```sh
go build -o pgclone ./cmd/pgclone.go
```

Run the test build with GoReleaser (snapshot, no publish):

```sh
goreleaser release --snapshot --clean
```

Release a new version (requires a GitHub token and push permissions):

```sh
goreleaser release --clean
```

Or using gh cli:

```sh
GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
```

---

## License

MIT License — see [LICENSE](LICENSE).

Copyright 2026 Warpcom España.
