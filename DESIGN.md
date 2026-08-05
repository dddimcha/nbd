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
L2:      L1 payload split into N data shards + K parity shards
         shard header: shardIndex (2) | shardSize (4) | crc32 (4)
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
- **L2 Reed–Solomon** — the L1 payload is split into N data shards + K parity
  shards (klauspost/reedsolomon). Per-shard CRC marks bad shards as erasures;
  `Reconstruct` recovers up to K fully lost or corrupt shards, then the result
  decodes as ordinary L1. This is the MinIO/RAID model in miniature: it
  survives bit rot, truncation and torn ranges alike. The only external
  dependency, isolated in its own file behind the tier switch.

Word-wise Hamming SECDED was considered as a zero-dependency middle tier and
rejected: at 12.5% overhead it only repairs scattered bit flips, while the
realistic failure classes for a serialized delta (truncation, lost ranges) need
erasure coding anyway. RS at 10+2 costs 20% but recovers the loss of 20% of
the blob outright.

## Why not one tier

Integrity always costs overhead and the spec demands the smallest possible
format, so protection is a per-blob choice recorded in the header, not a
default tax.

## Testing strategy

Round-trip and exact-size assertions per tier; a corruption matrix (bad header,
bad record, truncation at every boundary, shuffled records, single/double bit
flips); fuzzing on `Deserialize` — mutated input must never panic; benchmarks
for all four operations.
