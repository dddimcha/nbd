# blockdevice

In-memory copy-on-write block device backend in Go.

A device is initialized with immutable base data (the shared filesystem image).
Writes land in a dirty-block overlay; reads merge the overlay over the base.
The overlay serializes to a compact delta and a device can be rebuilt from the
base plus that delta — the pause/resume model for sandboxes sharing one base
filesystem.

## API

```go
import "github.com/dddimcha/nbd/blockdevice"

dev := blockdevice.New(base)              // base length divisible by 4096
dev.ReadAt(p, off)                        // off, len(p) block-aligned
dev.WriteAt(p, off)
blob, _ := dev.Serialize()                // only changed blocks
dev2, _ := blockdevice.Deserialize(blob, base)
```

## Serialization tiers

| Tier | Guarantees | Overhead per 4096-byte block |
|------|------------|------------------------------|
| L0 (default) | none — smallest possible | 8 B (0.2%) |
| L1 | corruption detection, partial recovery | 12 B (0.3%) |
| L2 | Reed–Solomon: reconstructs up to K lost/corrupt shards | K/N of the blob |

Every record carries its own block index, so a decoder survives reordering,
gaps and truncation; a corrupt block degrades to the base content instead of
failing the whole device. See [DESIGN.md](docs/DESIGN.md) for the exact formats and
trade-offs.

## Layout

```
blockdevice/        the Go package (core, wire format, RS tier, tests)
cmd/bdev/           bdev CLI: diff / apply / inspect
docs/               DESIGN.md — formats, trade-offs, concurrency contract;
                    ARCHITECTURE.md — layers, data flow, extension points
scripts/            gates.sh — quality-gate runner
build/              Dockerfile — reproducible build & test stages
.github/workflows/  CI: fmt, vet, race tests, fuzz + bench smoke
.claude/            agent roles and vendored engineering skills
```

## Development

```
make verify        # fast gates: fmt, vet, race tests
make test          # + 30s fuzz + bench smoke
make bench         # benchmarks with allocation stats
make docker-test   # full gates in a clean container (no local Go needed)
```
