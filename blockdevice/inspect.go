package blockdevice

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/klauspost/reedsolomon"
)

// Info describes a serialized delta without decoding it against a base.
type Info struct {
	// Version is the wire format version from the header.
	Version uint8
	// Tier is the serialization tier from the header.
	Tier Tier
	// BlockCount is the number of dirty-block records the header claims.
	BlockCount uint64
	// BlobSize is the total length of the blob in bytes.
	BlobSize int
	// BadBlocks lists record indices whose CRC failed (L1) or that could
	// not be reconstructed (L2). Always nil for L0 (no per-record CRC).
	BadBlocks []int64
}

// Inspect decodes a delta blob's header and, for L1/L2, verifies record CRCs
// without needing the base image. It returns ErrCorrupt on bad magic, version,
// header CRC, or a body inconsistent with the header.
func Inspect(blob []byte) (Info, error) {
	var info Info
	if len(blob) < headerSize {
		return info, fmt.Errorf("blockdevice: blob length %d shorter than header %d: %w", len(blob), headerSize, ErrCorrupt)
	}
	if [4]byte(blob[0:4]) != magic || blob[4] != formatVersion {
		return info, fmt.Errorf("blockdevice: bad magic or format version: %w", ErrCorrupt)
	}
	if crc32.ChecksumIEEE(blob[:14]) != binary.LittleEndian.Uint32(blob[14:18]) {
		return info, fmt.Errorf("blockdevice: header CRC mismatch: %w", ErrCorrupt)
	}
	info.Version = blob[4]
	info.Tier = Tier(blob[5])
	info.BlockCount = binary.LittleEndian.Uint64(blob[6:14])
	info.BlobSize = len(blob)
	body := blob[headerSize:]

	switch info.Tier {
	case TierL0:
		if len(body)%l0RecordSize != 0 || uint64(len(body)/l0RecordSize) != info.BlockCount {
			return info, fmt.Errorf("blockdevice: L0 body length %d inconsistent with block count %d: %w",
				len(body), info.BlockCount, ErrCorrupt)
		}
		return info, nil
	case TierL1:
		if len(body)%l1RecordSize != 0 || uint64(len(body)/l1RecordSize) != info.BlockCount {
			return info, fmt.Errorf("blockdevice: L1 body length %d inconsistent with block count %d: %w",
				len(body), info.BlockCount, ErrCorrupt)
		}
		info.BadBlocks = badL1Records(body)
		return info, nil
	case TierL2:
		bad, err := inspectRS(body, info.BlockCount)
		if err != nil {
			return info, err
		}
		info.BadBlocks = bad
		return info, nil
	default:
		return info, ErrUnsupportedTier
	}
}

// badL1Records walks a whole-record L1 payload and returns the indices of
// records whose CRC fails, sorted by record position. nil when all verify.
func badL1Records(body []byte) []int64 {
	var bad []int64
	for off := 0; off+l1RecordSize <= len(body); off += l1RecordSize {
		rec := body[off : off+l1RecordSize]
		want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
		if crc32.ChecksumIEEE(rec[:8+BlockSize]) != want {
			bad = append(bad, int64(binary.LittleEndian.Uint64(rec[0:8])))
		}
	}
	return bad
}

// inspectRS reports the unrecoverable record indices of a TierL2 body. When
// Reed-Solomon reconstruction succeeds the payload is fully recoverable and
// the result is nil; beyond K erasures it mirrors rsSalvage's record walk.
func inspectRS(body []byte, blockCount uint64) ([]int64, error) {
	g, err := parseRSMeta(body, blockCount)
	if err != nil {
		return nil, err
	}
	if g.payloadLen == 0 {
		return nil, nil
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
				return badL1Records(payload[:g.payloadLen]), nil
			}
		}
		// Reconstruction failed despite enough shards claiming valid —
		// fall through to the salvage walk, like deserializeRS.
	}

	// Beyond repair: report every record that overlaps a lost shard or fails
	// its own CRC, by index when the index field is readable.
	var bad []int64
	rec := make([]byte, l1RecordSize)
	for lo := 0; lo+l1RecordSize <= g.payloadLen; lo += l1RecordSize {
		if readShardSpan(shards, g.dataShards, g.shardSize, rec, lo) {
			want := binary.LittleEndian.Uint32(rec[8+BlockSize:])
			if crc32.ChecksumIEEE(rec[:8+BlockSize]) != want {
				bad = append(bad, int64(binary.LittleEndian.Uint64(rec[0:8])))
			}
			continue
		}
		var idxBuf [8]byte
		if readShardSpan(shards, g.dataShards, g.shardSize, idxBuf[:], lo) {
			bad = append(bad, int64(binary.LittleEndian.Uint64(idxBuf[:])))
		} else {
			bad = append(bad, -1) // index bytes themselves are lost
		}
	}
	return bad, nil
}
