# blockdevice

In-memory copy-on-write block device backend in Go.

A device is initialized with immutable base data (the shared filesystem image).
Writes land in a dirty-block overlay; reads merge the overlay over the base.
The overlay serializes to a compact delta and a device can be rebuilt from the
base plus that delta — the pause/resume model for sandboxes sharing one base
filesystem.

## API

```go
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
failing the whole device. See [DESIGN.md](DESIGN.md) for the exact formats and
trade-offs.

## Development

```
go test -race ./...
go vet ./...
go test -fuzz=FuzzDeserialize -fuzztime=30s ./...
```
