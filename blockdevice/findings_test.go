package blockdevice

// Regression tests for the 2026-08 review findings. Each test is labeled with
// the finding it pins down.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"testing"
)

// Finding 1: checkRange must not overflow on off+int64(n) for huge offsets.
func TestCheckRangeOverflow(t *testing.T) {
	dev, _ := fmtDevice(t, 4, nil)
	p := make([]byte, BlockSize)

	// MaxInt64-BlockSize+1 is block-aligned; off+len(p) wraps negative.
	hugeOff := int64(math.MaxInt64) - BlockSize + 1
	if _, err := dev.ReadAt(p, hugeOff); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("ReadAt(huge off) err = %v, want ErrOutOfRange", err)
	}
	if _, err := dev.WriteAt(p, hugeOff); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("WriteAt(huge off) err = %v, want ErrOutOfRange", err)
	}
	// Aligned but just past the wrap-safe boundary.
	if _, err := dev.ReadAt(p, int64(math.MaxInt64/BlockSize)*BlockSize); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("ReadAt(near-max off) err = %v, want ErrOutOfRange", err)
	}
	// Sanity: valid I/O still works.
	if _, err := dev.ReadAt(p, 0); err != nil {
		t.Errorf("ReadAt(0): %v", err)
	}
}

// forgeRSBlob builds a header+rsMeta-only TierL2 blob with valid CRCs and the
// given (attacker-chosen) geometry.
func forgeRSBlob(dataShards, parityShards int, shardSize uint32, blockCount uint64) []byte {
	payloadLen := blockCount * uint64(l1RecordSize)
	blob := make([]byte, headerSize+rsMetaSize)
	copy(blob[0:4], magic[:])
	blob[4] = formatVersion
	blob[5] = byte(TierL2)
	binary.LittleEndian.PutUint64(blob[6:14], blockCount)
	binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))
	meta := blob[headerSize:]
	binary.LittleEndian.PutUint16(meta[0:2], uint16(dataShards))
	binary.LittleEndian.PutUint16(meta[2:4], uint16(parityShards))
	binary.LittleEndian.PutUint32(meta[4:8], shardSize)
	binary.LittleEndian.PutUint64(meta[8:16], payloadLen)
	binary.LittleEndian.PutUint32(meta[16:20], crc32.ChecksumIEEE(meta[:16]))
	return blob
}

// Finding 2: a forged-but-CRC-valid rsMeta demanding gigabytes must be
// rejected as ErrCorrupt — allocations are capped by the blob size.
func TestRSForgedMetaHugeAllocRejected(t *testing.T) {
	base := make([]byte, 8*BlockSize)

	// Internally consistent forgery: N=255, shardSize == ceil(payload/N),
	// payload == blockCount records, total*shardSize just under the old
	// 8 GiB constant cap — only the len(blob) consistency check stops it.
	const blockCount = 2082000 // payload ~8.55 GB
	payload := uint64(blockCount) * uint64(l1RecordSize)
	shardSize := uint32((payload + 254) / 255)
	blob := forgeRSBlob(255, 1, shardSize, blockCount)
	if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
		t.Errorf("forged 8GB rsMeta: err = %v, want ErrCorrupt", err)
	}

	// The original repro shape: shardSize = 1<<25 out of thin air.
	blob = forgeRSBlob(255, 0, 1<<25, 1)
	if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
		t.Errorf("forged shardSize=1<<25: err = %v, want ErrCorrupt", err)
	}
}

// Finding 3: salvage (> K lost shards) must not zero-fill lost shard regions
// into the record stream. Lost regions are skipped; blocks are reported by
// their real index when readable, never as pseudo-index 0, and applied blocks
// are never listed.
func TestRSSalvageReportsRealLostBlocks(t *testing.T) {
	base := make([]byte, 64*BlockSize)
	dev := New(base)
	dirty := make([]int64, 15)
	for i := range dirty {
		dirty[i] = int64(i + 1) // blocks 1..15; block 0 stays clean
		p := bytes.Repeat([]byte{byte(0x40 + dirty[i])}, BlockSize)
		if _, err := dev.WriteAt(p, dirty[i]*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := dev.SerializeRS(4, 1)
	if err != nil {
		t.Fatalf("SerializeRS: %v", err)
	}
	// payload = 15 records, shardSize = ceil(15*4108/4) = 15405. Losing data
	// shards 1 and 2 (> K=1) leaves records 0-2 (blocks 1-3) and 12-14
	// (blocks 13-15) intact; record 3 (block 4) straddles the shard 0/1
	// boundary with a readable index; records 4-11 are unattributable.
	corruptShard(t, blob, 1)
	corruptShard(t, blob, 2)

	got, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if !pre.Truncated {
		t.Error("records with unreadable indices were lost; Truncated must be set")
	}
	if want := []int64{4}; !equalInt64s(pre.BadBlocks, want) {
		t.Errorf("BadBlocks = %v, want %v (real straddling record; no pseudo-index 0, no dups)", pre.BadBlocks, want)
	}
	for _, idx := range []int64{1, 2, 3, 13, 14, 15} {
		expectBlock(t, got, base, idx, fill(byte(0x40+idx)))
	}
	expectBlock(t, got, base, 0, nil) // block 0 must not be fabricated
	expectBlock(t, got, base, 4, nil) // lost record degrades to base
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Finding 4: records appended beyond the header's blockCount must not be
// silently applied with a nil error.
func TestSurplusRecordsAreCorruption(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{1, 2})
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Forge a perfectly valid extra record for block 5 and append it,
	// leaving the (CRC-valid) header claiming blockCount=2.
	extra := make([]byte, l1RecordSize)
	binary.LittleEndian.PutUint64(extra[0:8], 5)
	for i := 8; i < 8+BlockSize; i++ {
		extra[i] = 0xEE
	}
	binary.LittleEndian.PutUint32(extra[8+BlockSize:], crc32.ChecksumIEEE(extra[:8+BlockSize]))
	blob = append(blob, extra...)

	got, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("surplus record: err = %v, want *PartialRecoveryError", err)
	}
	if !pre.Truncated {
		t.Error("surplus bytes must set Truncated")
	}
	expectBlock(t, got, base, 5, nil) // the smuggled record must not apply
	expectBlock(t, got, base, 1, fill(0x41))
	expectBlock(t, got, base, 2, fill(0x42))
}

// Finding 5: a non-nil PartialRecoveryError must never be empty — an
// out-of-range-index record that cannot be named sets Truncated.
func TestPartialRecoveryNeverEmpty(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{1})
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the record's index far out of range and re-seal its CRC.
	rec := blob[headerSize:]
	binary.LittleEndian.PutUint64(rec[0:8], 1<<40)
	binary.LittleEndian.PutUint32(rec[8+BlockSize:], crc32.ChecksumIEEE(rec[:8+BlockSize]))

	_, err = Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("out-of-range index: err = %v, want *PartialRecoveryError", err)
	}
	if len(pre.BadBlocks) == 0 && !pre.Truncated {
		t.Error("PartialRecoveryError carries neither BadBlocks nor Truncated")
	}
}

// Finding 6: a duplicate index whose later copy is intact recovers the block;
// it must not appear in BadBlocks.
func TestDuplicateIndexRecoveredNotBad(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{2})
	single, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	goodRec := single[headerSize:]
	badRec := bytes.Clone(goodRec)
	badRec[8] ^= 0xFF // data flip -> record CRC fails, index still reads 2

	blob := bytes.Clone(single[:headerSize])
	binary.LittleEndian.PutUint64(blob[6:14], 2) // two records now
	reseal(blob)
	blob = append(blob, badRec...)
	blob = append(blob, goodRec...)

	got, err := Deserialize(blob, base)
	if err != nil {
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("err = %v, want nil or *PartialRecoveryError", err)
		}
		for _, b := range pre.BadBlocks {
			if b == 2 {
				t.Errorf("BadBlocks = %v lists block 2, which the intact copy recovered", pre.BadBlocks)
			}
		}
	}
	expectBlock(t, got, base, 2, fill(0x42))
}

// Finding 7: DESIGN.md documents the real shard header (6 bytes, no size
// field) and the real default geometry; pin the code side of both claims.
func TestRSDocumentedConstants(t *testing.T) {
	if rsShardHdrSize != 6 {
		t.Errorf("rsShardHdrSize = %d, want 6 (shardIndex(2)|crc32(4))", rsShardHdrSize)
	}
	cases := []struct{ payload, wantN int }{
		{0, 1}, {1, 1}, {4108, 1}, {3 * 4096, 3}, {10 * 4096, 10}, {1 << 30, 10},
	}
	for _, c := range cases {
		n, k := rsDefaultShards(c.payload)
		if n != c.wantN || k != 2 {
			t.Errorf("rsDefaultShards(%d) = (%d,%d), want (%d,2) per N=min(10,max(1,payloadLen/4096))",
				c.payload, n, k, c.wantN)
		}
	}
}

// Finding 8: the headerless L1-stride fallback must not run for blobs whose
// tier byte reads a known non-L1 tier.
func TestHeaderlessFallbackGatedOnTier(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{1, 3})
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Flip the tier byte to L0 without re-sealing: the header CRC fails and
	// the tier byte no longer reads TierL1 — no L1-stride scan.
	l0tier := bytes.Clone(blob)
	l0tier[5] = byte(TierL0)
	if _, err := Deserialize(l0tier, base); !errors.Is(err, ErrCorrupt) {
		t.Errorf("tier byte L0 + bad header CRC: err = %v, want ErrCorrupt", err)
	}

	// An out-of-range tier byte is plausibly the corrupted field itself, so
	// the fallback still runs and recovers the intact records.
	wild := bytes.Clone(blob)
	wild[5] = 0x7F
	got, err := Deserialize(wild, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("wild tier byte: err = %v, want *PartialRecoveryError from fallback", err)
	}
	expectBlock(t, got, base, 1, fill(0x41))
	expectBlock(t, got, base, 3, fill(0x43))
}
