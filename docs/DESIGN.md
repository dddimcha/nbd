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
L2:      rsMeta: dataShards N (2) | parityShards K (2) | shardSize (4) |
                 payloadLen (8) | crc32 (4)
         then (N+K) shards, each: shardIndex (2) | crc32(shard data) (4) |
                                  shardSize bytes
```

The 6-byte shard header carries no size field: shardSize is global, stored
once in rsMeta.

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

  Default geometry scales with the delta: N = min(10, max(1, payloadLen/4096))
  data shards, K = 2 parity shards. Ten or more dirty blocks get the full
  10+2 (20% parity overhead); small deltas keep N low so they are not sliced
  into slivers — but the parity cost is then proportionally large: a single
  dirty block is 1+2 shards, i.e. ~200% overhead (plus the 6-byte shard
  headers and 20-byte rsMeta). L2 only pays off for deltas of several blocks.

### Shard geometry bounds

`reedsolomon.New` does O(shards³)-ish matrix setup regardless of payload
size, so shard counts must be justified by the payload or a tiny hostile blob
becomes a CPU multiplier (a ~6 KB blob claiming 254+2 shards cost tens of
milliseconds to reject, pre-bound). Both `SerializeRS` (encode) and
`parseRSMeta` (decode — so `Deserialize` and `Inspect` alike) enforce the
same rule, keeping every encodable blob decodable:

```
dataShards   >= 1
dataShards   <= max(1, ceil(payloadLen/128))   // >=128 payload bytes per data shard
parityShards <= max(2, dataShards)
dataShards + parityShards <= 256               // classic RS limit
```

Violations are `ErrUnsupportedTier` on encode and `ErrCorrupt` on decode,
raised before any allocation or matrix work.

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

## Concurrency

A `Device` is documented as not safe for concurrent use — callers synchronize
— rather than carrying an internal mutex, following the `bytes.Buffer`
precedent for low-level buffer types. The type is a thin overlay over a map;
its expected embedding (one device per sandbox, driven by one I/O path) never
shares an instance across goroutines, so an internal `RWMutex` would tax every
`ReadAt`/`WriteAt` in the common single-caller case to protect against a usage
pattern the design doesn't have, and it is far easier for a caller that does
need sharing to add a lock than for one that doesn't to remove ours. Note that
the contract covers *all* methods: `Serialize` reads the same dirty map
`WriteAt` mutates, so even a "read-only" snapshot concurrent with a write is a
data race. The internal goroutine fan-out in `writeRecords` /
`writeRecordsSharded` needs no caller-visible locking: workers only read the
dirty map and each writes a disjoint, position-determined output span, joined
before the method returns — `TestSerializeParallelByteIdentity` exercises this
path under `-race` in the standard gates.
