package blockdevice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
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
	// BadBlocks lists the unrecoverable block indices, sorted ascending.
	BadBlocks []int64
}

// Error implements the error interface.
func (e *PartialRecoveryError) Error() string {
	return fmt.Sprintf("blockdevice: partial recovery, %d bad block(s): %v", len(e.BadBlocks), e.BadBlocks)
}

// sortedIndices returns the dirty block indices in ascending order.
func (d *Device) sortedIndices() []int64 {
	idxs := make([]int64, 0, len(d.dirty))
	for i := range d.dirty {
		idxs = append(idxs, i)
	}
	sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })
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

	off := headerSize
	for _, idx := range idxs {
		binary.LittleEndian.PutUint64(blob[off:off+8], uint64(idx))
		copy(blob[off+8:off+8+BlockSize], d.dirty[idx])
		if t == TierL1 {
			crc := crc32.ChecksumIEEE(blob[off : off+8+BlockSize])
			binary.LittleEndian.PutUint32(blob[off+8+BlockSize:off+recSize], crc)
		}
		off += recSize
	}
	return blob, nil
}

// Deserialize rebuilds a Device from base plus a serialized delta.
//
// The blob's header is validated; L0 records are applied as-is, L1 records
// are applied only if their CRC verifies. Records that fail CRC, fall out of
// base bounds, or are cut off by a truncated tail are skipped and reported
// via *PartialRecoveryError alongside the (still usable) device; the affected
// blocks read as base data. If the header CRC fails on an L1-record-sized
// blob, Deserialize falls back to scanning fixed-stride L1 records and
// applies those that self-verify. Untrusted input never panics.
func Deserialize(blob, base []byte) (*Device, error) {
	if len(base)%BlockSize != 0 {
		return nil, ErrCorrupt
	}
	if len(blob) < headerSize {
		return nil, ErrCorrupt
	}
	if [4]byte(blob[0:4]) != magic || blob[4] != formatVersion {
		return nil, ErrCorrupt
	}
	wantCRC := binary.LittleEndian.Uint32(blob[14:18])
	if crc32.ChecksumIEEE(blob[:14]) != wantCRC {
		return deserializeHeaderless(blob, base)
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
	truncated := false

	n := len(body) / recSize
	if len(body)%recSize != 0 || uint64(n) < blockCount {
		truncated = true
		// If the partial tail carries a readable index, report it.
		if tail := body[n*recSize:]; len(tail) >= 8 {
			if idx := int64(binary.LittleEndian.Uint64(tail[0:8])); idx >= 0 && idx < maxBlock {
				bad = append(bad, idx)
			}
		}
	}

	for i := 0; i < n; i++ {
		rec := body[i*recSize : (i+1)*recSize]
		idx := int64(binary.LittleEndian.Uint64(rec[0:8]))
		if tier == TierL1 {
			want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
			if crc32.ChecksumIEEE(rec[:8+BlockSize]) != want {
				if idx >= 0 && idx < maxBlock {
					bad = append(bad, idx)
				}
				truncated = true // any skipped record makes recovery partial
				continue
			}
		}
		if idx < 0 || idx >= maxBlock {
			truncated = true
			continue
		}
		b := make([]byte, BlockSize)
		copy(b, rec[8:8+BlockSize])
		dev.dirty[idx] = b
	}

	if truncated || len(bad) > 0 {
		sort.Slice(bad, func(a, b int) bool { return bad[a] < bad[b] })
		return dev, &PartialRecoveryError{BadBlocks: bad}
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
		b := make([]byte, BlockSize)
		copy(b, rec[8:8+BlockSize])
		dev.dirty[idx] = b
		recovered++
	}
	if recovered == 0 {
		return nil, ErrCorrupt
	}
	sort.Slice(bad, func(a, b int) bool { return bad[a] < bad[b] })
	return dev, &PartialRecoveryError{BadBlocks: bad}
}
