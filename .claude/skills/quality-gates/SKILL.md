---
name: quality-gates
description: Use when asked to "verify", run "gates", check code "перед коммитом" / "before commit", or before declaring any change to this repo done — runs the repo's mandatory quality gate sequence and checks its review invariants.
---

# Quality Gates

## Overview

Every change to this repo must pass the full gate sequence before it is called done. `scripts/gates.sh` runs the fast gates; `scripts/gates.sh --full` adds fuzz and bench.

## Gate sequence (exact commands)

Run from the repo root, in this order:

| # | Gate | Command | Pass condition |
|---|------|---------|----------------|
| 1 | Format | `gofmt -l .` | Output is EMPTY |
| 2 | Vet | `go vet ./...` | Exit 0 |
| 3 | Tests + race | `go test -race ./...` | Exit 0 |
| 4 | Fuzz | `go test -run='^$' -fuzz=FuzzDeserialize -fuzztime=30s .` | No crasher in 30s |
| 5 | Bench smoke | `go test -run='^$' -bench=. -benchtime=1x .` | Exit 0 (compiles + runs, not perf) |

Shortcut:

```bash
scripts/gates.sh          # fast: fmt + vet + race tests
scripts/gates.sh --full   # + fuzz (30s) + bench smoke
```

Fast gates suffice mid-iteration; run `--full` before commit.

## Repo invariants (what reviews enforce)

Gates catch regressions; reviews enforce these — check your diff against each:

- **No caller-slice aliasing.** Never retain or return slices that alias caller-provided buffers (e.g. `WriteAt` input, `Deserialize` blob). Copy on the boundary.
- **Base immutability.** The base image passed to `New` is never mutated; all writes go to the overlay.
- **Deterministic Serialize.** Same device state must produce byte-identical `Serialize`/`SerializeTier` output — no map-iteration order, timestamps, or randomness in the format.
- **No panics on untrusted input.** `Deserialize` must return errors, never panic, on arbitrary blobs — that is exactly what `FuzzDeserialize` proves; any new parsing path needs fuzz coverage.
- **Allocations bounded by `len(blob)`, not header fields.** Never allocate from a count/length read out of the blob header; a hostile header must not cause OOM. Validate against actual blob size first.
- **`PartialRecoveryError` never empty.** If it's returned, it must carry at least one recovered/lost block — never a zero-value error alongside a device.

## Common mistakes

- Declaring done after `go test ./...` alone — race, fuzz, and vet catch different bug classes here.
- Running the fuzz gate without `-run='^$'` (re-runs the whole suite first, slow).
- Treating the bench gate as a perf check — `-benchtime=1x` is a compile/run smoke only.
