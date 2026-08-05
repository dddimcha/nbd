# Design

## Data model

```
Device
  base  []byte             // immutable, shared, never copied
  dirty map[int64][]byte   // block index -> 4096-byte copy
```

- `ReadAt` serves each block from `dirty` if present, else from `base`.
- `WriteAt` copies incoming bytes into per-block buffers (never aliases the
  caller's slice) and stores them under the block index.
- Cost: O(blocks touched) per op; memory is proportional to the delta, which is
  the point of copy-on-write.

## Wire format

All integers little-endian. Records are sorted by block index, making
serialization deterministic (byte-identical for identical state).

```
header:  magic "BDEV" (4) | version (1) | tier (1) | blockCount (8) | headerCRC32 (4)
L0 rec:  index (8) | data (4096)
L1 rec:  index (8) | data (4096) | crc32(index|data) (4)
L2H rec: index (8) | data (4096) | ecc (512) | crc32 (4)
```

Records are self-describing: the index lives inside the record, so ordering,
gaps and truncation never confuse the decoder — a truncated blob still parses
an integral number of records.

## Tiers and trade-offs

The spec asks for the smallest possible overhead, so the bare format is the
default and integrity is opt-in via the header tier byte.

- **L0** — 8 bytes/block. Nothing else. This is the spec-minimal answer.
- **L1** — CRC32 per record + protected header. Detects corruption and turns it
  into *erasures*: bad records are skipped, good ones applied, and the caller
  gets `*PartialRecoveryError{BadBlocks}`. Reads of a bad block fall back to
  base data — graceful degradation instead of total failure.
- **L2 Hamming** — word-wise SECDED Hamming(72,64): 8 parity bits per 64-bit
  word, 512 B per block. Corrects one bit flip per word (up to 512 scattered
  flips per block), detects double flips. CRC stays on top as the arbiter
  against miscorrection. Zero dependencies. Protects against bit rot, not
  against lost records.
- **L2 Reed–Solomon** — the delta is split into N data shards + K parity
  (klauspost/reedsolomon). Per-shard CRC marks bad shards as erasures;
  `Reconstruct` recovers up to K fully lost shards. This is the MinIO/RAID
  model in miniature: it survives truncation and torn ranges, which Hamming
  cannot. The only external dependency, isolated in its own package.

## Why not one tier

Bit flips and lost ranges are different failure classes: Hamming repairs the
former cheaply in-place, erasure coding repairs the latter but costs K/N.
Stacking both by default would contradict the smallest-overhead requirement,
so the format makes integrity a per-blob choice recorded in the header.

## Testing strategy

Round-trip and exact-size assertions per tier; a corruption matrix (bad header,
bad record, truncation at every boundary, shuffled records, single/double bit
flips); fuzzing on `Deserialize` — mutated input must never panic; benchmarks
for all four operations.
