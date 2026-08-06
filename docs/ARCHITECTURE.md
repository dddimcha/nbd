# Architecture

## Layers

    cmd/bdev  (package main — CLI, flag parsing, exit codes, file I/O)
        │  imports only the public API
        ▼
    blockdevice  (single public library package)
        │  blockdevice.go — Device: base + dirty overlay, ReadAt/WriteAt
        │  inspect.go     — Inspect: header/CRC report without a base
        │  format.go      — wire format: header, L0/L1 records, Serialize/Deserialize
        │  rs.go          — L2 tier: Reed-Solomon sharding over the L1 payload
        ▼
    github.com/klauspost/reedsolomon  (only external dependency, reached
    only through rs.go and inspect.go, behind the tier switch)

The CLI never touches format internals: everything it prints comes from the
exported API (`Inspect`, `Deserialize`, `SerializeTier`, typed errors).

## Data flow: pause / resume

Pause:  live image ──WriteAt──▶ Device{base, dirty} ──SerializeTier──▶ delta blob
Resume: delta blob + shared base ──Deserialize──▶ Device ──ReadAt──▶ live image

`bdev diff` reproduces the pause path from two on-disk images (block compare →
WriteAt of changed blocks → serialize); `bdev apply` is the resume path
materialized to a file; `bdev inspect` is the read-only diagnostic on the blob.

## Why a single library package

The library is one cohesive concern — a copy-on-write overlay and its wire
format — whose parts share unexported vocabulary (record layouts, tier
constants, shard headers). Splitting format.go/rs.go into sub-packages would
force exporting those internals or duplicating them; keeping one package keeps
the format private, the API surface minimal (Device, Tier, Inspect/Info, four
sentinel errors plus PartialRecoveryError), and the only external dependency
confined to rs.go and inspect.go. The CLI needs
no `internal/` package: it is a single main package with no importable surface
to protect.

## Extension points

- **cmd/nbd-server** — a future NBD network server is another `cmd/` main
  package over the same public API; nothing in the library changes.
- **New tiers** — add a `TierL3` constant, a case in `SerializeTier` /
  `Deserialize`, and a new file beside rs.go; records stay self-describing so
  old decoders reject unknown tiers cleanly via `ErrUnsupportedTier`.
- **Streaming I/O** — `Serialize`'s sorted-record layout permits a future
  `io.Reader`/`io.Writer` pair without format changes.
