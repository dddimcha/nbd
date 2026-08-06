package blockdevice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"runtime"
	"slices"
	"sync"
)

// Tier selects the serialization integrity level.
type Tier uint8

// Serialization tiers. See DESIGN.md for the trade-offs.
const (
	// TierL0 is the bare format: 8 bytes of overhead per block, no
	// integrity protection beyond the header CRC.
	TierL0 Tier = 0
	// TierL1 adds a CRC32 per record: corruption is detected and degraded
	// to base data, reported via *PartialRecoveryError.
	TierL1 Tier = 1
	// TierL2 wraps the L1 payload in Reed-Solomon shards: up to K lost or
	// corrupt shards are reconstructed transparently. See rs.go.
	TierL2 Tier = 2
)

const (
	formatVersion = 1
	headerSize    = 4 + 1 + 1 + 8 + 4 // magic | version | tier | blockCount | headerCRC32
	l0RecordSize  = 8 + BlockSize
	l1RecordSize  = 8 + BlockSize + 4
)

var magic = [4]byte{'B', 'D', 'E', 'V'}

// Typed errors returned by serialization.
var (
	// ErrCorrupt is returned when a blob cannot be decoded at all.
	ErrCorrupt = errors.New("blockdevice: corrupt blob")
	// ErrUnsupportedTier is returned for tiers not (yet) implemented.
	ErrUnsupportedTier = errors.New("blockdevice: unsupported tier")
)

// PartialRecoveryError reports that a blob decoded only partially: the
// returned device is usable, but the listed blocks could not be recovered and
// read as base data.
type PartialRecoveryError struct {
	// BadBlocks lists the unrecoverable block indices, sorted ascending and
	// deduplicated. A block that was ultimately recovered (e.g. a duplicate
	// record with an intact copy) is never listed.
	BadBlocks []int64
	// Truncated reports that data was lost without a nameable block index:
	// a cut-off tail whose index bytes are unreadable, surplus bytes beyond
	// the header's blockCount, an unverifiable header, or shard regions lost
	// beyond repair. Every non-nil PartialRecoveryError has at least one
	// BadBlocks entry or Truncated set (or both).
	Truncated bool
}

// Error implements the error interface.
func (e *PartialRecoveryError) Error() string {
	return fmt.Sprintf("blockdevice: partial recovery, %d bad block(s): %v (truncated: %v)",
		len(e.BadBlocks), e.BadBlocks, e.Truncated)
}

// finalizeBad post-processes a bad-block list: entries whose block was in the
// end successfully applied are dropped, duplicates are removed, and the
// result is sorted ascending.
func finalizeBad(dev *Device, bad []int64) []int64 {
	if len(bad) == 0 {
		return nil // happy path: no map/sort churn
	}
	seen := make(map[int64]bool, len(bad))
	out := bad[:0]
	for _, b := range bad {
		if seen[b] {
			continue
		}
		seen[b] = true
		if _, applied := dev.dirty[b]; applied {
			continue
		}
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

// sortedIndices returns the dirty block indices in ascending order.
func (d *Device) sortedIndices() []int64 {
	idxs := make([]int64, 0, len(d.dirty))
	for i := range d.dirty {
		idxs = append(idxs, i)
	}
	slices.Sort(idxs)
	return idxs
}

// Serialize encodes the dirty overlay at the default tier (L0).
func (d *Device) Serialize() ([]byte, error) {
	return d.SerializeTier(TierL0)
}

// SerializeTier encodes the dirty overlay at the given tier. Records are
// sorted by block index, so identical state yields byte-identical output.
func (d *Device) SerializeTier(t Tier) ([]byte, error) {
	var recSize int
	switch t {
	case TierL0:
		recSize = l0RecordSize
	case TierL1:
		recSize = l1RecordSize
	case TierL2:
		return d.SerializeRS(0, 0) // defaults, see rs.go
	default:
		return nil, ErrUnsupportedTier
	}

	idxs := d.sortedIndices()
	blob := make([]byte, headerSize+len(idxs)*recSize)

	copy(blob[0:4], magic[:])
	blob[4] = formatVersion
	blob[5] = byte(t)
	binary.LittleEndian.PutUint64(blob[6:14], uint64(len(idxs)))
	binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))

	d.writeRecords(blob[headerSize:], idxs, recSize, t == TierL1)
	return blob, nil
}

// writeRecordRange serializes the records for idxs into dst at recSize stride,
// appending a per-record CRC32 when withCRC is set. dst must be exactly
// len(idxs)*recSize bytes.
func (d *Device) writeRecordRange(dst []byte, idxs []int64, recSize int, withCRC bool) {
	off := 0
	for _, idx := range idxs {
		binary.LittleEndian.PutUint64(dst[off:off+8], uint64(idx))
		copy(dst[off+8:off+8+BlockSize], d.dirty[idx])
		if withCRC {
			crc := crc32.ChecksumIEEE(dst[off : off+8+BlockSize])
			binary.LittleEndian.PutUint32(dst[off+8+BlockSize:off+recSize], crc)
		}
		off += recSize
	}
}

// writeRecords is writeRecordRange fanned out over goroutines for large
// deltas. Each worker owns a disjoint, position-determined slice of dst, so
// the output bytes are identical to the sequential path regardless of
// scheduling. Worker count is GOMAXPROCS capped so every worker serializes at
// least serialMinPerWorker records — below that the spawn/join overhead
// outweighs the win and the sequential path runs instead.
const serialMinPerWorker = 128

func (d *Device) writeRecords(dst []byte, idxs []int64, recSize int, withCRC bool) {
	workers := runtime.GOMAXPROCS(0)
	if max := len(idxs) / serialMinPerWorker; workers > max {
		workers = max
	}
	if workers <= 1 {
		d.writeRecordRange(dst, idxs, recSize, withCRC)
		return
	}
	chunk := (len(idxs) + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < len(idxs); lo += chunk {
		hi := lo + chunk
		if hi > len(idxs) {
			hi = len(idxs)
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			d.writeRecordRange(dst[lo*recSize:hi*recSize], idxs[lo:hi], recSize, withCRC)
		}(lo, hi)
	}
	wg.Wait()
}

// Deserialize rebuilds a Device from base plus a serialized delta.
//
// The blob's header is validated; L0 records are applied as-is, L1 records
// are applied only if their CRC verifies. Records that fail CRC, fall out of
// base bounds, or are cut off by a truncated tail are skipped and reported
// via *PartialRecoveryError alongside the (still usable) device; the affected
// blocks read as base data. Exactly blockCount (from the header) records are
// applied; surplus records or bytes appended beyond that are treated as
// corruption and reported via *PartialRecoveryError with Truncated set. If
// the header CRC fails on an L1-record-sized blob whose tier byte still reads
// TierL1 (or is out of range), Deserialize falls back to scanning fixed-stride
// L1 records and applies those that self-verify. Untrusted input never panics.
func Deserialize(blob, base []byte) (*Device, error) {
	if len(base)%BlockSize != 0 {
		return nil, fmt.Errorf("blockdevice: base length %d not a multiple of block size %d: %w", len(base), BlockSize, ErrCorrupt)
	}
	if len(blob) < headerSize {
		return nil, fmt.Errorf("blockdevice: blob length %d shorter than header %d: %w", len(blob), headerSize, ErrCorrupt)
	}
	if [4]byte(blob[0:4]) != magic || blob[4] != formatVersion {
		return nil, fmt.Errorf("blockdevice: bad magic or format version: %w", ErrCorrupt)
	}
	wantCRC := binary.LittleEndian.Uint32(blob[14:18])
	if crc32.ChecksumIEEE(blob[:14]) != wantCRC {
		// The header failed its CRC, so its fields are untrustworthy. Only
		// attempt the fixed-stride L1 fallback scan when the tier byte still
		// reads TierL1, or is itself out of the known range (i.e. plausibly
		// the corrupted field): scanning an L0 or L2 body at L1 stride could
		// only "recover" records via CRC32 collisions. CRC32 accepts a random
		// candidate with probability 2^-32, so an m-record scan expects
		// m/2^32 false accepts — negligible for realistic blob sizes, but
		// not cryptographic protection.
		if t := Tier(blob[5]); t == TierL1 || t > TierL2 {
			return deserializeHeaderless(blob, base)
		}
		return nil, fmt.Errorf("blockdevice: header CRC mismatch: %w", ErrCorrupt)
	}

	tier := Tier(blob[5])
	blockCount := binary.LittleEndian.Uint64(blob[6:14])
	body := blob[headerSize:]

	switch tier {
	case TierL0, TierL1:
		return applyRecords(body, tier, blockCount, base)
	case TierL2:
		return deserializeRS(body, blockCount, base)
	default:
		return nil, ErrUnsupportedTier
	}
}

// applyRecords decodes a section of L0/L1 records onto a fresh device over
// base. It is shared by the L0/L1 paths and by the L2 layer, which feeds it
// the Reed-Solomon-recovered record payload.
func applyRecords(body []byte, tier Tier, blockCount uint64, base []byte) (*Device, error) {
	recSize := l0RecordSize
	if tier == TierL1 {
		recSize = l1RecordSize
	}

	dev := New(base)
	maxBlock := int64(len(base) / BlockSize)
	var bad []int64
	partial := false      // anything was skipped, lost, or surplus
	unattributed := false // loss with no nameable block index

	// The header CRC covers layout, not content, so blockCount is an upper
	// bound the body must honor: exactly blockCount records are applied and
	// any surplus bytes are treated as corruption (reported via
	// *PartialRecoveryError with Truncated set — the device built from the
	// first blockCount records is still returned).
	n := len(body) / recSize
	if uint64(n) >= blockCount {
		if uint64(n) > blockCount || len(body)%recSize != 0 {
			partial, unattributed = true, true
			n = int(blockCount)
		}
	} else {
		partial = true
		// If the partial tail carries a readable index, report it;
		// otherwise the loss is unattributable.
		if tail := body[n*recSize:]; len(tail) >= 8 {
			if idx := int64(binary.LittleEndian.Uint64(tail[0:8])); idx >= 0 && idx < maxBlock {
				bad = append(bad, idx)
			} else {
				unattributed = true
			}
		} else {
			unattributed = true
		}
	}

	// Pre-size the overlay for the records actually present. n is derived
	// from len(body), not the untrusted blockCount header field, so a
	// hostile header cannot force a huge allocation.
	dev.dirty = make(map[int64][]byte, n)

	// One arena allocation sliced per record instead of n per-block makes.
	// Each block owns a capacity-capped BlockSize window (three-index
	// slices), so buffers stay independent and never alias blob or base;
	// WriteAt copies into the existing buffer and cannot bleed across
	// blocks. Trade-off: the arena stays live as long as ANY of its blocks
	// is referenced from the overlay, so memory is pinned in proportion to
	// the decoded delta even if most blocks are later overwritten (an
	// overwrite reuses the same window). On partial recovery, records that
	// were skipped (bad CRC, out-of-range index) still consume their arena
	// windows, so a single surviving record can pin up to n*BlockSize —
	// bounded by ~1x the blob size the caller already accepted, which we
	// deem acceptable.
	arena := make([]byte, n*BlockSize)

	for i := 0; i < n; i++ {
		rec := body[i*recSize : (i+1)*recSize]
		idx := int64(binary.LittleEndian.Uint64(rec[0:8]))
		if tier == TierL1 {
			want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
			if crc32.ChecksumIEEE(rec[:8+BlockSize]) != want {
				partial = true // any skipped record makes recovery partial
				if idx >= 0 && idx < maxBlock {
					bad = append(bad, idx)
				} else {
					unattributed = true
				}
				continue
			}
		}
		if idx < 0 || idx >= maxBlock {
			partial, unattributed = true, true
			continue
		}
		b := arena[i*BlockSize : (i+1)*BlockSize : (i+1)*BlockSize]
		copy(b, rec[8:8+BlockSize])
		dev.dirty[idx] = b
	}

	bad = finalizeBad(dev, bad)
	if partial || len(bad) > 0 {
		// Invariant: a non-nil PartialRecoveryError carries at least one
		// BadBlocks entry or Truncated (see the type doc).
		if len(bad) == 0 {
			unattributed = true
		}
		return dev, &PartialRecoveryError{BadBlocks: bad, Truncated: unattributed}
	}
	return dev, nil
}

// deserializeHeaderless handles a blob whose header CRC failed: if the body
// is an integral number of L1-sized records, scan them at fixed stride and
// apply every record whose own CRC verifies and whose index is within base
// bounds.
func deserializeHeaderless(blob, base []byte) (*Device, error) {
	body := blob[headerSize:]
	if len(body) == 0 || len(body)%l1RecordSize != 0 {
		return nil, ErrCorrupt
	}
	dev := New(base)
	maxBlock := int64(len(base) / BlockSize)
	var bad []int64
	recovered := 0
	// Same pre-sizing and single-arena strategy as applyRecords; see the
	// aliasing/pinning notes there. Sizing follows len(body), so hostile
	// input cannot inflate it.
	nRecs := len(body) / l1RecordSize
	dev.dirty = make(map[int64][]byte, nRecs)
	arena := make([]byte, nRecs*BlockSize)
	for i := 0; i+l1RecordSize <= len(body); i += l1RecordSize {
		rec := body[i : i+l1RecordSize]
		idx := int64(binary.LittleEndian.Uint64(rec[0:8]))
		want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
		if crc32.ChecksumIEEE(rec[:8+BlockSize]) != want || idx < 0 || idx >= maxBlock {
			if idx >= 0 && idx < maxBlock {
				bad = append(bad, idx)
			}
			continue
		}
		r := i / l1RecordSize
		b := arena[r*BlockSize : (r+1)*BlockSize : (r+1)*BlockSize]
		copy(b, rec[8:8+BlockSize])
		dev.dirty[idx] = b
		recovered++
	}
	if recovered == 0 {
		return nil, ErrCorrupt
	}
	// The header itself is unverifiable, so completeness is unknowable:
	// Truncated is always set on this path.
	return dev, &PartialRecoveryError{BadBlocks: finalizeBad(dev, bad), Truncated: true}
}
