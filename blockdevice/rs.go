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
// decoding degrades: records lying fully inside intact data shards are
// applied if they self-verify, records overlapping lost shards are skipped
// and reported via *PartialRecoveryError (by index when readable, else via
// the Truncated flag). Lost regions are never zero-filled into the record
// stream.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"runtime"
	"sync"

	"github.com/klauspost/reedsolomon"
)

const (
	rsMetaSize      = 2 + 2 + 4 + 8 + 4 // N | K | shardSize | payloadLen | crc32
	rsShardHdrSize  = 2 + 4             // shardIndex | crc32(shard data)
	rsDefaultData   = 10
	rsDefaultParity = 2
	// rsMaxTotalShards is the absolute cap on data+parity shards.
	// reedsolomon.New does O(total^3) matrix setup regardless of payload
	// size, so the shard count itself — not the payload — is the CPU
	// multiplier: 254+2 shards cost tens of milliseconds per decode even
	// when the payload "justifies" them (a 33 KB payload clears the
	// 128-bytes-per-shard rule at N=254). Capping total at 64 bounds the
	// matrix work at sub-millisecond worst case; all default geometries
	// (rsDefaultData=10, +2 parity) fit with wide margin. Enforced
	// identically on encode (SerializeRS) and decode (parseRSMeta). See
	// DESIGN.md ("Shard geometry bounds").
	rsMaxTotalShards = 64
	// rsMinShardPayload is the minimum average payload bytes each data shard
	// must be justified by: geometries with dataShards >
	// max(1, ceil(payloadLen/rsMinShardPayload)) are rejected on both the
	// encode and decode side. This is a secondary tightening on top of the
	// rsMaxTotalShards cap: it keeps tiny payloads from being sliced into
	// useless slivers and ties shard count to bytes the caller already paid
	// to read.
	rsMinShardPayload = 128
	// rsMaxDecodeAlloc bounds total shard memory a decoded rsMeta may demand,
	// so a forged-but-CRC-valid rsMeta cannot force a huge allocation.
	rsMaxDecodeAlloc = 1 << 33
)

// validRSGeometry reports whether a shard geometry is acceptable for a
// payload of the given size. The rule (documented in DESIGN.md):
//
//	dataShards   <= max(1, ceil(payloadLen/rsMinShardPayload))
//	parityShards <= max(rsDefaultParity, dataShards)
//
// (the dataShards >= 1, parityShards >= 0 and total <= rsMaxTotalShards
// checks live at the call sites, which report them separately). SerializeRS and
// parseRSMeta apply the same predicate, so every blob SerializeRS can produce
// is one Deserialize/Inspect accepts, and vice versa.
func validRSGeometry(dataShards, parityShards int, payloadLen uint64) bool {
	maxData := (payloadLen + rsMinShardPayload - 1) / rsMinShardPayload
	if maxData < 1 {
		maxData = 1
	}
	maxParity := dataShards
	if maxParity < rsDefaultParity {
		maxParity = rsDefaultParity
	}
	return uint64(dataShards) <= maxData && parityShards <= maxParity
}

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
	parity = rsDefaultParity
	return data, parity
}

// SerializeRS encodes the dirty overlay at TierL2 with the given shard
// geometry. dataShards or parityShards <= 0 selects the defaults (see
// rsDefaultShards). SerializeTier(TierL2) is equivalent to SerializeRS(0, 0).
func (d *Device) SerializeRS(dataShards, parityShards int) ([]byte, error) {
	idxs := d.sortedIndices()
	payloadLen := len(idxs) * l1RecordSize

	if dataShards <= 0 || parityShards <= 0 {
		dd, dp := rsDefaultShards(payloadLen)
		if dataShards <= 0 {
			dataShards = dd
		}
		if parityShards <= 0 {
			parityShards = dp
		}
	}
	if dataShards+parityShards > rsMaxTotalShards {
		return nil, fmt.Errorf("blockdevice: %d shards exceeds max %d: %w",
			dataShards+parityShards, rsMaxTotalShards, ErrUnsupportedTier)
	}
	if !validRSGeometry(dataShards, parityShards, uint64(payloadLen)) {
		// The decoder (parseRSMeta) rejects payload-unjustified geometries,
		// so refusing to serialize them keeps every SerializeRS output
		// deserializable.
		return nil, fmt.Errorf("blockdevice: shard geometry %d+%d not justified by payload %d (see DESIGN.md): %w",
			dataShards, parityShards, payloadLen, ErrUnsupportedTier)
	}

	shardSize := 0
	if payloadLen > 0 {
		shardSize = (payloadLen + dataShards - 1) / dataShards
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
	binary.LittleEndian.PutUint64(meta[8:16], uint64(payloadLen))
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
	shardSectionOff := headerSize + rsMetaSize
	for i := 0; i < total; i++ {
		s := blob[shardSectionOff+i*(rsShardHdrSize+shardSize):]
		shards[i] = s[rsShardHdrSize : rsShardHdrSize+shardSize]
	}
	// The L1 record payload is written straight into the data-shard slots —
	// no intermediate payload buffer, no second copy. Records straddling a
	// shard boundary are split by writeShardSpans; the record CRC is computed
	// incrementally from the contiguous sources, so the bytes are identical
	// to building a flat payload and slicing it into shards.
	writeRecordsSharded(d, idxs, shards[:dataShards], shardSize)
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil, fmt.Errorf("blockdevice: init reed-solomon encoder (%d+%d shards): %w: %w",
			dataShards, parityShards, ErrUnsupportedTier, err)
	}
	if err := enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("blockdevice: reed-solomon encode: %w: %w", ErrCorrupt, err)
	}
	// Shard headers: index + CRC per shard. Each iteration touches only its
	// own header and shard, so fanning out per shard keeps the output
	// byte-identical to the sequential order.
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hdr := blob[shardSectionOff+i*(rsShardHdrSize+shardSize):]
			binary.LittleEndian.PutUint16(hdr[0:2], uint16(i))
			binary.LittleEndian.PutUint32(hdr[2:6], crc32.ChecksumIEEE(shards[i]))
		}(i)
	}
	wg.Wait()
	return blob, nil
}

// writeShardSpans copies src into the data shards at payload offset off,
// splitting across shard boundaries, and returns the offset past the write.
func writeShardSpans(shards [][]byte, shardSize, off int, src []byte) int {
	for len(src) > 0 {
		n := copy(shards[off/shardSize][off%shardSize:], src)
		src = src[n:]
		off += n
	}
	return off
}

// writeRecordRangeSharded serializes idxs as L1 records directly into the
// data shards, starting at payload offset off. Byte-for-byte identical to
// writing a flat L1 payload and splitting it into shards.
func writeRecordRangeSharded(d *Device, idxs []int64, shards [][]byte, shardSize, off int) {
	var hdr [8]byte
	var tail [4]byte
	for _, idx := range idxs {
		binary.LittleEndian.PutUint64(hdr[:], uint64(idx))
		block := d.dirty[idx]
		crc := crc32.Update(crc32.ChecksumIEEE(hdr[:]), crc32.IEEETable, block)
		binary.LittleEndian.PutUint32(tail[:], crc)
		off = writeShardSpans(shards, shardSize, off, hdr[:])
		off = writeShardSpans(shards, shardSize, off, block)
		off = writeShardSpans(shards, shardSize, off, tail[:])
	}
}

// writeRecordsSharded is writeRecordRangeSharded fanned out over goroutines
// for large deltas, mirroring writeRecords in format.go: each worker owns the
// payload byte range determined solely by its record positions, so output is
// deterministic regardless of scheduling. Race-free under the package's
// "callers synchronize" contract: workers only read the dirty map and write
// disjoint shard spans, joined by wg.Wait before return.
func writeRecordsSharded(d *Device, idxs []int64, shards [][]byte, shardSize int) {
	workers := runtime.GOMAXPROCS(0)
	if max := len(idxs) / serialMinPerWorker; workers > max {
		workers = max
	}
	if workers <= 1 {
		writeRecordRangeSharded(d, idxs, shards, shardSize, 0)
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
			writeRecordRangeSharded(d, idxs[lo:hi], shards, shardSize, lo*l1RecordSize)
		}(lo, hi)
	}
	wg.Wait()
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

// rsGeometry is the validated content of an rsMeta section.
type rsGeometry struct {
	dataShards, parityShards, shardSize, payloadLen int
}

// parseRSMeta decodes and validates the rsMeta section at the front of a
// TierL2 body against the outer header's blockCount. It returns ErrCorrupt
// on any inconsistency; see the inline comments for the trust model.
func parseRSMeta(body []byte, blockCount uint64) (rsGeometry, error) {
	var g rsGeometry
	if len(body) < rsMetaSize {
		return g, fmt.Errorf("blockdevice: body length %d shorter than rs meta %d: %w", len(body), rsMetaSize, ErrCorrupt)
	}
	meta := body[:rsMetaSize]
	if crc32.ChecksumIEEE(meta[:16]) != binary.LittleEndian.Uint32(meta[16:20]) {
		return g, fmt.Errorf("blockdevice: rs meta CRC mismatch: %w", ErrCorrupt)
	}
	g.dataShards = int(binary.LittleEndian.Uint16(meta[0:2]))
	g.parityShards = int(binary.LittleEndian.Uint16(meta[2:4]))
	g.shardSize = int(binary.LittleEndian.Uint32(meta[4:8]))
	payloadLen64 := binary.LittleEndian.Uint64(meta[8:16])
	total := g.dataShards + g.parityShards

	// The absolute total cap and the payload-justification rule below are
	// both enforced BEFORE any allocation or Reed-Solomon matrix work:
	// reedsolomon.New is O(total^3) regardless of payload size, so a
	// hostile-but-CRC-valid rsMeta claiming e.g. 254+2 shards would
	// otherwise cost tens of milliseconds per blob — even with a payload
	// large enough to "justify" the count. Same predicates as the encode
	// side.
	if g.dataShards < 1 || g.parityShards < 0 || total > rsMaxTotalShards {
		return g, fmt.Errorf("blockdevice: invalid shard geometry %d+%d: %w", g.dataShards, g.parityShards, ErrCorrupt)
	}
	if !validRSGeometry(g.dataShards, g.parityShards, payloadLen64) {
		return g, fmt.Errorf("blockdevice: shard geometry %d+%d not justified by payload %d: %w",
			g.dataShards, g.parityShards, payloadLen64, ErrCorrupt)
	}
	// A CRC-valid rsMeta is not a trustworthy rsMeta (CRC32 is not
	// authentication), so its fields must also be internally consistent and
	// consistent with the bytes actually present:
	//   - the payload is whole L1 records and matches the header blockCount;
	//   - shardSize is exactly what SerializeRS derives from payloadLen;
	//   - the claimed shard geometry cannot exceed a small multiple of
	//     len(body) — allocations stay O(len(blob)), never a forged multi-GB
	//     demand from a tiny blob. The 4x slack tolerates truncation of up
	//     to ~75% of the blob; rsMaxDecodeAlloc stays as an absolute cap.
	if payloadLen64%uint64(l1RecordSize) != 0 ||
		payloadLen64/uint64(l1RecordSize) != blockCount {
		return g, fmt.Errorf("blockdevice: rs payload length %d inconsistent with block count %d: %w", payloadLen64, blockCount, ErrCorrupt)
	}
	if uint64(total)*uint64(g.shardSize) > rsMaxDecodeAlloc ||
		payloadLen64 > uint64(g.dataShards)*uint64(g.shardSize) {
		return g, fmt.Errorf("blockdevice: rs shard geometry demands excessive or insufficient allocation: %w", ErrCorrupt)
	}
	g.payloadLen = int(payloadLen64)

	if g.payloadLen == 0 {
		return g, nil
	}
	if g.shardSize == 0 {
		return g, fmt.Errorf("blockdevice: zero shard size with non-empty payload %d: %w", g.payloadLen, ErrCorrupt)
	}
	if g.shardSize != (g.payloadLen+g.dataShards-1)/g.dataShards {
		return g, fmt.Errorf("blockdevice: shard size %d inconsistent with payload %d over %d data shards: %w",
			g.shardSize, g.payloadLen, g.dataShards, ErrCorrupt)
	}
	if uint64(total)*uint64(rsShardHdrSize+g.shardSize) > 4*uint64(len(body)) {
		return g, fmt.Errorf("blockdevice: claimed shard section exceeds 4x body length %d: %w", len(body), ErrCorrupt)
	}
	return g, nil
}

// parseShardSection splits the shard section into total shard slots; anything
// missing, truncated, misindexed or CRC-bad becomes a nil erasure. It also
// returns the erasure count.
func parseShardSection(sect []byte, total, shardSize int) (shards [][]byte, missing int) {
	shards = make([][]byte, total)
	stride := rsShardHdrSize + shardSize
	for off := 0; off+stride <= len(sect); off += stride {
		idx := int(binary.LittleEndian.Uint16(sect[off : off+2]))
		want := binary.LittleEndian.Uint32(sect[off+2 : off+6])
		data := sect[off+rsShardHdrSize : off+stride]
		if idx >= total || shards[idx] != nil || crc32.ChecksumIEEE(data) != want {
			continue
		}
		shards[idx] = data
	}
	for _, s := range shards {
		if s == nil {
			missing++
		}
	}
	return shards, missing
}

// readShardSpan copies payload bytes [off, off+len(dst)) out of the intact
// data shards; it reports false if any covering shard is lost.
func readShardSpan(shards [][]byte, dataShards, shardSize int, dst []byte, off int) bool {
	for len(dst) > 0 {
		si := off / shardSize
		if si >= dataShards || shards[si] == nil {
			return false
		}
		n := copy(dst, shards[si][off%shardSize:])
		dst = dst[n:]
		off += n
	}
	return true
}

// deserializeRS decodes the body of a TierL2 blob (rsMeta + shard section)
// over base. Called from Deserialize after the outer header verified.
func deserializeRS(body []byte, blockCount uint64, base []byte) (*Device, error) {
	g, err := parseRSMeta(body, blockCount)
	if err != nil {
		return nil, err
	}
	dataShards, parityShards := g.dataShards, g.parityShards
	shardSize, payloadLen := g.shardSize, g.payloadLen
	total := dataShards + parityShards

	if payloadLen == 0 {
		return applyRecords(nil, TierL1, blockCount, base)
	}

	shards, missing := parseShardSection(body[rsMetaSize:], total, shardSize)

	if missing <= parityShards {
		enc, err := reedsolomon.New(dataShards, parityShards)
		if err != nil {
			return nil, fmt.Errorf("blockdevice: init reed-solomon decoder (%d+%d shards): %w: %w",
				dataShards, parityShards, ErrCorrupt, err)
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

	// More than K erasures (or reconstruction failure): salvage record by
	// record. Regions covered by lost data shards are SKIPPED, never
	// zero-filled — feeding zeroed spans into the record decoder would parse
	// pseudo-records with index 0 and misreport which blocks were lost.
	// A record fully inside intact shards decodes normally; a record that
	// overlaps a lost shard is reported by its true index when the 8-byte
	// index field is still readable, else counted as unattributable loss
	// (Truncated).
	return rsSalvage(shards, dataShards, shardSize, payloadLen, base)
}

// rsSalvage applies whatever L1 records survive when Reed-Solomon
// reconstruction is impossible. shards[i] == nil marks a lost shard.
func rsSalvage(shards [][]byte, dataShards, shardSize, payloadLen int, base []byte) (*Device, error) {
	readSpan := func(dst []byte, off int) bool {
		return readShardSpan(shards, dataShards, shardSize, dst, off)
	}

	dev := New(base)
	maxBlock := int64(len(base) / BlockSize)
	var bad []int64          // lost-shard records with a readable, in-range index
	var skipped []skippedRec // read-but-UNVERIFIED index+data of CRC-failed records
	var lostNamed []int64    // indices of records overlapping lost shards
	unattributed := false
	rec := make([]byte, l1RecordSize)
	for lo := 0; lo+l1RecordSize <= payloadLen; lo += l1RecordSize {
		if readSpan(rec, lo) {
			idx := int64(binary.LittleEndian.Uint64(rec[0:8]))
			want := binary.LittleEndian.Uint32(rec[8+BlockSize : l1RecordSize])
			switch {
			case crc32.ChecksumIEEE(rec[:8+BlockSize]) != want:
				// CRC covers index+data: the index is unverified, so no
				// block is blamed (see PartialRecoveryError). Keep the read
				// index and a copy of the data (rec is reused) for the
				// true-duplicate suppression check below.
				skipped = append(skipped, skippedRec{idx: idx, data: bytes.Clone(rec[8 : 8+BlockSize])})
			case idx >= 0 && idx < maxBlock:
				b := make([]byte, BlockSize)
				copy(b, rec[8:8+BlockSize])
				dev.dirty[idx] = b
			default:
				unattributed = true
			}
			continue
		}
		// Record overlaps a lost shard: name the block if the index field
		// happens to lie in an intact shard.
		var idxBuf [8]byte
		if readSpan(idxBuf[:], lo) {
			if idx := int64(binary.LittleEndian.Uint64(idxBuf[:])); idx >= 0 && idx < maxBlock {
				bad = append(bad, idx)
				lostNamed = append(lostNamed, idx)
				continue
			}
		}
		unattributed = true
	}

	// True-duplicate suppression, mirroring applyRecords: a CRC-failed
	// record is no loss ONLY when its data bytes are bit-identical to the
	// applied record for its read index (see PartialRecoveryError for the
	// residual window); anything else is unattributable loss.
	for _, s := range skipped {
		if s.idx >= 0 && s.idx < maxBlock {
			if applied, ok := dev.dirty[s.idx]; ok && bytes.Equal(applied, s.data) {
				continue
			}
		}
		unattributed = true
	}
	// A record lost to a dead shard is structural loss even when its named
	// block was applied by a duplicate record: that copy's content is gone.
	for _, s := range lostNamed {
		if _, applied := dev.dirty[s]; applied {
			unattributed = true
		}
	}
	bad = finalizeBad(dev, bad)
	if len(bad) == 0 && !unattributed {
		// Every record decoded — the lost shards were all parity (or
		// padding), or every skipped record was a bit-identical duplicate
		// of an applied one. Full recovery.
		return dev, nil
	}
	return dev, &PartialRecoveryError{BadBlocks: bad, Truncated: unattributed}
}
