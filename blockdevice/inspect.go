package blockdevice

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"slices"

	"github.com/klauspost/reedsolomon"
)

// Info describes a serialized delta without decoding it against a base.
//
// Its loss reporting mirrors what Deserialize would do: BadBlocks carries
// only readable block indices, index-unreadable losses are counted in
// UnattributedLoss, and structural damage (truncation, surplus, an
// unverifiable header) sets Truncated. Inspect returns ErrCorrupt only for
// blobs Deserialize also rejects outright.
type Info struct {
	// Version is the wire format version from the header.
	Version uint8
	// Tier is the serialization tier from the header.
	Tier Tier
	// BlockCount is the number of dirty-block records the header claims.
	BlockCount uint64
	// BlobSize is the total length of the blob in bytes.
	BlobSize int
	// BadBlocks lists lost blocks whose index is readable: CRC-verified L1
	// records carrying an index that cannot exist in any base (negative when
	// read as int64), a truncated tail or lost L2 shard region whose 8-byte
	// index field survived. Sorted ascending and deduplicated; real record
	// indices only — never a sentinel. Inspect has no base, so entries may
	// lie outside any particular base's range (including negative values).
	// Always nil for L0 (no per-record CRC).
	BadBlocks []int64
	// UnattributedLoss counts records that are lost or unverifiable without
	// a readable index: L1/L2 records whose CRC failed (the CRC covers
	// index+data, so the index is unreadable), L2 records whose index bytes
	// lie in a lost shard, and headerless-scan records that did not
	// self-verify. Like Deserialize, a CRC-failed record whose read index
	// matches a record that did verify is treated as superseded and not
	// counted.
	UnattributedLoss int
	// Truncated reports structural damage: a cut-off or surplus body
	// relative to the header's blockCount, an unverifiable header (the
	// headerless L1 fallback), or any UnattributedLoss.
	Truncated bool
}

// Inspect decodes a delta blob's header and, for L1/L2, verifies record CRCs
// without needing the base image. It returns ErrCorrupt on bad magic, version,
// or an unrecoverable body — mirroring Deserialize, a truncated L0/L1 body or
// a header-CRC failure that the L1 fixed-stride fallback can still read is
// reported via Info (Truncated/UnattributedLoss), not as an error.
func Inspect(blob []byte) (Info, error) {
	var info Info
	if len(blob) < headerSize {
		return info, fmt.Errorf("blockdevice: blob length %d shorter than header %d: %w", len(blob), headerSize, ErrCorrupt)
	}
	if [4]byte(blob[0:4]) != magic || blob[4] != formatVersion {
		return info, fmt.Errorf("blockdevice: bad magic or format version: %w", ErrCorrupt)
	}
	info.Version = blob[4]
	info.Tier = Tier(blob[5])
	info.BlockCount = binary.LittleEndian.Uint64(blob[6:14])
	info.BlobSize = len(blob)
	body := blob[headerSize:]

	if crc32.ChecksumIEEE(blob[:14]) != binary.LittleEndian.Uint32(blob[14:18]) {
		// Mirror Deserialize's headerless fallback: when the tier byte still
		// reads TierL1 (or is out of range, i.e. plausibly the corrupted
		// field) and the body scans as self-verifying L1 records, report the
		// blob as recoverable-but-truncated. NOTE: with the header
		// unverifiable, Version/Tier/BlockCount are unverified raw bytes.
		if t := Tier(blob[5]); t == TierL1 || t > TierL2 {
			return inspectHeaderless(info, body)
		}
		return info, fmt.Errorf("blockdevice: header CRC mismatch: %w", ErrCorrupt)
	}

	switch info.Tier {
	case TierL0:
		inspectBodyLayout(&info, body, l0RecordSize)
		return info, nil
	case TierL1:
		n := inspectBodyLayout(&info, body, l1RecordSize)
		bad, unattributed := scanL1Records(body[:n*l1RecordSize])
		info.BadBlocks = append(info.BadBlocks, bad...)
		info.UnattributedLoss += unattributed
		finishInfo(&info)
		return info, nil
	case TierL2:
		if err := inspectRS(&info, body); err != nil {
			return info, err
		}
		finishInfo(&info)
		return info, nil
	default:
		return info, ErrUnsupportedTier
	}
}

// inspectBodyLayout checks the body length against the header blockCount at
// the given record stride, mirroring applyRecords: surplus or misaligned
// bytes set Truncated, a short body sets Truncated and names the cut-off
// record's block when its 8-byte index field survived. It returns the number
// of whole records to scan (capped at blockCount) and finalizes the info.
func inspectBodyLayout(info *Info, body []byte, recSize int) int {
	n := len(body) / recSize
	if uint64(n) >= info.BlockCount {
		if uint64(n) > info.BlockCount || len(body)%recSize != 0 {
			info.Truncated = true
			n = int(info.BlockCount)
		}
	} else {
		info.Truncated = true
		if tail := body[n*recSize:]; len(tail) >= 8 {
			info.BadBlocks = append(info.BadBlocks, int64(binary.LittleEndian.Uint64(tail[0:8])))
		} else {
			info.UnattributedLoss++
		}
	}
	finishInfo(info)
	return n
}

// finishInfo sorts and dedupes BadBlocks and folds UnattributedLoss into
// Truncated.
func finishInfo(info *Info) {
	if len(info.BadBlocks) > 0 {
		slices.Sort(info.BadBlocks)
		info.BadBlocks = slices.Compact(info.BadBlocks)
	}
	if info.UnattributedLoss > 0 {
		info.Truncated = true
	}
}

// scanL1Records walks a whole-record L1 payload. CRC-verified records with a
// negative (impossible-in-any-base) index are reported in bad; CRC-failed
// records have an unreadable index and are counted as unattributed loss,
// except when their read index matches a record that did verify (superseded
// duplicate, mirroring Deserialize).
func scanL1Records(body []byte) (bad []int64, unattributed int) {
	applied := make(map[int64]bool)
	var skipped []int64
	for off := 0; off+l1RecordSize <= len(body); off += l1RecordSize {
		rec := body[off : off+l1RecordSize]
		idx := int64(binary.LittleEndian.Uint64(rec[0:8]))
		want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
		if crc32.ChecksumIEEE(rec[:8+BlockSize]) != want {
			skipped = append(skipped, idx)
			continue
		}
		if idx < 0 {
			bad = append(bad, idx)
			continue
		}
		applied[idx] = true
	}
	for _, s := range skipped {
		if !applied[s] {
			unattributed++
		}
	}
	return bad, unattributed
}

// inspectHeaderless mirrors deserializeHeaderless for a blob whose header CRC
// failed: an integral-record body with at least one self-verifying L1 record
// is reported as recoverable (Truncated, non-verifying records counted as
// unattributed loss); anything else is ErrCorrupt, exactly as Deserialize
// would reject it.
func inspectHeaderless(info Info, body []byte) (Info, error) {
	if len(body) == 0 || len(body)%l1RecordSize != 0 {
		return info, fmt.Errorf("blockdevice: header CRC mismatch: %w", ErrCorrupt)
	}
	recovered := 0
	for off := 0; off+l1RecordSize <= len(body); off += l1RecordSize {
		rec := body[off : off+l1RecordSize]
		want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
		if crc32.ChecksumIEEE(rec[:8+BlockSize]) == want {
			recovered++
		}
	}
	if recovered == 0 {
		return info, fmt.Errorf("blockdevice: header CRC mismatch: %w", ErrCorrupt)
	}
	info.Truncated = true
	info.UnattributedLoss = len(body)/l1RecordSize - recovered
	return info, nil
}

// inspectRS reports the unrecoverable records of a TierL2 body into info.
// When Reed-Solomon reconstruction succeeds the payload is scanned like plain
// L1; beyond K erasures it mirrors rsSalvage's record walk: a lost-shard
// record with a readable index field lands in BadBlocks, index-unreadable and
// CRC-failed records count as unattributed loss.
func inspectRS(info *Info, body []byte) error {
	g, err := parseRSMeta(body, info.BlockCount)
	if err != nil {
		return err
	}
	if g.payloadLen == 0 {
		return nil
	}
	total := g.dataShards + g.parityShards
	shards, missing := parseShardSection(body[rsMetaSize:], total, g.shardSize)

	if missing <= g.parityShards {
		if enc, err := reedsolomon.New(g.dataShards, g.parityShards); err == nil {
			if err := enc.Reconstruct(shards); err == nil {
				payload := make([]byte, 0, g.dataShards*g.shardSize)
				for i := 0; i < g.dataShards; i++ {
					payload = append(payload, shards[i]...)
				}
				bad, unattributed := scanL1Records(payload[:g.payloadLen])
				info.BadBlocks = append(info.BadBlocks, bad...)
				info.UnattributedLoss += unattributed
				return nil
			}
		}
		// Reconstruction failed despite enough shards claiming valid —
		// fall through to the salvage walk, like deserializeRS.
	}

	applied := make(map[int64]bool)
	var skipped, lostNamed []int64
	rec := make([]byte, l1RecordSize)
	for lo := 0; lo+l1RecordSize <= g.payloadLen; lo += l1RecordSize {
		if readShardSpan(shards, g.dataShards, g.shardSize, rec, lo) {
			idx := int64(binary.LittleEndian.Uint64(rec[0:8]))
			want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
			switch {
			case crc32.ChecksumIEEE(rec[:8+BlockSize]) != want:
				skipped = append(skipped, idx) // index unverified
			case idx < 0:
				info.BadBlocks = append(info.BadBlocks, idx)
			default:
				applied[idx] = true
			}
			continue
		}
		var idxBuf [8]byte
		if readShardSpan(shards, g.dataShards, g.shardSize, idxBuf[:], lo) {
			lostNamed = append(lostNamed, int64(binary.LittleEndian.Uint64(idxBuf[:])))
		} else {
			info.UnattributedLoss++ // index bytes themselves are lost
		}
	}
	for _, s := range skipped {
		if !applied[s] {
			info.UnattributedLoss++
		}
	}
	for _, s := range lostNamed {
		if applied[s] {
			// The lost copy's block was applied from a duplicate record, but
			// the lost content itself is gone — structural loss.
			info.UnattributedLoss++
		} else {
			info.BadBlocks = append(info.BadBlocks, s)
		}
	}
	return nil
}
