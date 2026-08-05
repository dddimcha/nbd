package blockdevice

// TierL2: Reed-Solomon erasure coding over the L1 record payload.
//
// Wire layout (all little-endian):
//
//	outer header (tier = TierL2, blockCount) — identical to L0/L1, CRC-protected
//	rsMeta:  dataShards N (2) | parityShards K (2) | shardSize (4) |
//	         payloadLen (8) | crc32 of the preceding 16 bytes (4)
//	shards:  (N+K) x [ shardIndex (2) | crc32(shard data) (4) | shardSize bytes ]
//
// The payload is the plain L1 record section (index|data|crc32 per record,
// sorted by block index) — no inner header. The outer header carries the tier
// and blockCount for the whole blob, so wrapping a second header inside the
// shards would duplicate state; instead the recovered payload is fed straight
// back into the shared L1 record decoder (applyRecords in format.go).
//
// The payload is split into N equal shards (the last one zero-padded to
// shardSize), and K parity shards are computed with klauspost/reedsolomon.
// On decode, every shard whose CRC fails — or that is missing/truncated — is
// treated as an erasure; up to K erasures are fully reconstructed. Beyond K,
// decoding degrades: intact data shards are laid into a zero-filled payload
// and whichever L1 records still self-verify are applied, with the rest
// reported via *PartialRecoveryError.

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/klauspost/reedsolomon"
)

const (
	rsMetaSize      = 2 + 2 + 4 + 8 + 4 // N | K | shardSize | payloadLen | crc32
	rsShardHdrSize  = 2 + 4             // shardIndex | crc32(shard data)
	rsMaxShards     = 256               // classic Reed-Solomon limit
	rsDefaultData   = 10
	rsDefaultParity = 2
	// rsMaxDecodeAlloc bounds total shard memory a decoded rsMeta may demand,
	// so a forged-but-CRC-valid rsMeta cannot force a huge allocation.
	rsMaxDecodeAlloc = 1 << 33
)

// rsDefaultShards returns the default shard geometry for a payload of the
// given size. Parity is fixed at rsDefaultParity; the data shard count scales
// with the payload — N = min(10, max(1, payloadLen/4096)) — so tiny deltas
// are not sliced into useless slivers (one dirty block yields 1+2 shards, ten
// or more yield the full 10+2).
func rsDefaultShards(payloadLen int) (data, parity int) {
	data = payloadLen / 4096
	if data < 1 {
		data = 1
	}
	if data > rsDefaultData {
		data = rsDefaultData
	}
	return data, rsDefaultParity
}

// SerializeRS encodes the dirty overlay at TierL2 with the given shard
// geometry. dataShards or parityShards <= 0 selects the defaults (see
// rsDefaultShards). SerializeTier(TierL2) is equivalent to SerializeRS(0, 0).
func (d *Device) SerializeRS(dataShards, parityShards int) ([]byte, error) {
	idxs := d.sortedIndices()

	// Build the L1 record payload.
	payload := make([]byte, len(idxs)*l1RecordSize)
	off := 0
	for _, idx := range idxs {
		binary.LittleEndian.PutUint64(payload[off:off+8], uint64(idx))
		copy(payload[off+8:off+8+BlockSize], d.dirty[idx])
		crc := crc32.ChecksumIEEE(payload[off : off+8+BlockSize])
		binary.LittleEndian.PutUint32(payload[off+8+BlockSize:off+l1RecordSize], crc)
		off += l1RecordSize
	}

	if dataShards <= 0 || parityShards <= 0 {
		dd, dp := rsDefaultShards(len(payload))
		if dataShards <= 0 {
			dataShards = dd
		}
		if parityShards <= 0 {
			parityShards = dp
		}
	}
	if dataShards+parityShards > rsMaxShards {
		return nil, ErrUnsupportedTier
	}

	shardSize := 0
	if len(payload) > 0 {
		shardSize = (len(payload) + dataShards - 1) / dataShards
	}
	total := dataShards + parityShards

	blob := make([]byte, headerSize+rsMetaSize+total*(rsShardHdrSize+shardSize))
	copy(blob[0:4], magic[:])
	blob[4] = formatVersion
	blob[5] = byte(TierL2)
	binary.LittleEndian.PutUint64(blob[6:14], uint64(len(idxs)))
	binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))

	meta := blob[headerSize : headerSize+rsMetaSize]
	binary.LittleEndian.PutUint16(meta[0:2], uint16(dataShards))
	binary.LittleEndian.PutUint16(meta[2:4], uint16(parityShards))
	binary.LittleEndian.PutUint32(meta[4:8], uint32(shardSize))
	binary.LittleEndian.PutUint64(meta[8:16], uint64(len(payload)))
	binary.LittleEndian.PutUint32(meta[16:20], crc32.ChecksumIEEE(meta[:16]))

	if shardSize == 0 {
		// Empty overlay: header + rsMeta fully describe it; the (empty)
		// shard headers still make the blob self-describing.
		writeEmptyShardHeaders(blob[headerSize+rsMetaSize:], total)
		return blob, nil
	}

	// Fill the data shards in place (padding is the zero value already),
	// then encode parity.
	shards := make([][]byte, total)
	base := headerSize + rsMetaSize
	for i := 0; i < total; i++ {
		s := blob[base+i*(rsShardHdrSize+shardSize):]
		shards[i] = s[rsShardHdrSize : rsShardHdrSize+shardSize]
	}
	for i := 0; i < dataShards; i++ {
		lo := i * shardSize
		if lo < len(payload) {
			hi := lo + shardSize
			if hi > len(payload) {
				hi = len(payload)
			}
			copy(shards[i], payload[lo:hi])
		}
	}
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil, ErrUnsupportedTier
	}
	if err := enc.Encode(shards); err != nil {
		return nil, ErrCorrupt
	}
	for i := 0; i < total; i++ {
		hdr := blob[base+i*(rsShardHdrSize+shardSize):]
		binary.LittleEndian.PutUint16(hdr[0:2], uint16(i))
		binary.LittleEndian.PutUint32(hdr[2:6], crc32.ChecksumIEEE(shards[i]))
	}
	return blob, nil
}

// writeEmptyShardHeaders writes shard headers for shardSize == 0.
func writeEmptyShardHeaders(dst []byte, total int) {
	emptyCRC := crc32.ChecksumIEEE(nil)
	for i := 0; i < total; i++ {
		hdr := dst[i*rsShardHdrSize:]
		binary.LittleEndian.PutUint16(hdr[0:2], uint16(i))
		binary.LittleEndian.PutUint32(hdr[2:6], emptyCRC)
	}
}

// deserializeRS decodes the body of a TierL2 blob (rsMeta + shard section)
// over base. Called from Deserialize after the outer header verified.
func deserializeRS(body []byte, blockCount uint64, base []byte) (*Device, error) {
	if len(body) < rsMetaSize {
		return nil, ErrCorrupt
	}
	meta := body[:rsMetaSize]
	if crc32.ChecksumIEEE(meta[:16]) != binary.LittleEndian.Uint32(meta[16:20]) {
		return nil, ErrCorrupt
	}
	dataShards := int(binary.LittleEndian.Uint16(meta[0:2]))
	parityShards := int(binary.LittleEndian.Uint16(meta[2:4]))
	shardSize := int(binary.LittleEndian.Uint32(meta[4:8]))
	payloadLen64 := binary.LittleEndian.Uint64(meta[8:16])
	total := dataShards + parityShards

	if dataShards < 1 || parityShards < 0 || total > rsMaxShards {
		return nil, ErrCorrupt
	}
	if uint64(total)*uint64(shardSize) > rsMaxDecodeAlloc ||
		payloadLen64 > uint64(dataShards)*uint64(shardSize) {
		return nil, ErrCorrupt
	}
	payloadLen := int(payloadLen64)

	if payloadLen == 0 {
		return applyRecords(nil, TierL1, blockCount, base)
	}
	if shardSize == 0 {
		return nil, ErrCorrupt
	}

	// Parse the shard section; anything missing, truncated, misindexed or
	// CRC-bad becomes a nil erasure.
	shards := make([][]byte, total)
	stride := rsShardHdrSize + shardSize
	sect := body[rsMetaSize:]
	for off := 0; off+stride <= len(sect); off += stride {
		idx := int(binary.LittleEndian.Uint16(sect[off : off+2]))
		want := binary.LittleEndian.Uint32(sect[off+2 : off+6])
		data := sect[off+rsShardHdrSize : off+stride]
		if idx >= total || shards[idx] != nil || crc32.ChecksumIEEE(data) != want {
			continue
		}
		shards[idx] = data
	}

	missing := 0
	for _, s := range shards {
		if s == nil {
			missing++
		}
	}

	if missing <= parityShards {
		enc, err := reedsolomon.New(dataShards, parityShards)
		if err != nil {
			return nil, ErrCorrupt
		}
		if err := enc.Reconstruct(shards); err == nil {
			payload := make([]byte, 0, dataShards*shardSize)
			for i := 0; i < dataShards; i++ {
				payload = append(payload, shards[i]...)
			}
			return applyRecords(payload[:payloadLen], TierL1, blockCount, base)
		}
		// Reconstruction failed despite enough shards claiming valid —
		// fall through to per-record salvage.
	}

	// More than K erasures (or reconstruction failure): salvage. Lay the
	// intact data shards into a zero-filled payload and apply whichever L1
	// records still self-verify; applyRecords reports the rest as partial.
	payload := make([]byte, payloadLen)
	for i := 0; i < dataShards; i++ {
		if shards[i] == nil {
			continue
		}
		lo := i * shardSize
		if lo >= payloadLen {
			continue
		}
		hi := lo + shardSize
		if hi > payloadLen {
			hi = payloadLen
		}
		copy(payload[lo:hi], shards[i][:hi-lo])
	}
	dev, err := applyRecords(payload, TierL1, blockCount, base)
	if err == nil && missing > 0 {
		// Data survived only because every lost shard was parity — still a
		// full recovery. Otherwise applyRecords already reported partial.
		return dev, nil
	}
	return dev, err
}
