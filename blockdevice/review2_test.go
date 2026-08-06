package blockdevice

// Regression tests for the second review round (2026-08). Labels reference
// the findings: P0-1 (RS shard-count CPU bound), P1-2 (CRC-failed records
// must not blame their read index), P1-3 (superseded duplicates are full
// recovery), P1-4 (Inspect/Deserialize alignment).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
	"time"
)

// hostileRSBlob builds a complete, internally consistent TierL2 blob (header,
// rsMeta, full shard section with valid CRCs) for the given geometry over a
// payload of blockCount L1 records — the shape SerializeRS produced before
// the geometry bound existed.
func hostileRSBlob(t *testing.T, dataShards, parityShards int, blockCount int) []byte {
	t.Helper()
	payloadLen := blockCount * l1RecordSize
	shardSize := (payloadLen + dataShards - 1) / dataShards
	total := dataShards + parityShards
	blob := make([]byte, headerSize+rsMetaSize+total*(rsShardHdrSize+shardSize))
	copy(blob[0:4], magic[:])
	blob[4] = formatVersion
	blob[5] = byte(TierL2)
	binary.LittleEndian.PutUint64(blob[6:14], uint64(blockCount))
	binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))
	meta := blob[headerSize:]
	binary.LittleEndian.PutUint16(meta[0:2], uint16(dataShards))
	binary.LittleEndian.PutUint16(meta[2:4], uint16(parityShards))
	binary.LittleEndian.PutUint32(meta[4:8], uint32(shardSize))
	binary.LittleEndian.PutUint64(meta[8:16], uint64(payloadLen))
	binary.LittleEndian.PutUint32(meta[16:20], crc32.ChecksumIEEE(meta[:16]))
	for i := 0; i < total; i++ {
		hdr := blob[headerSize+rsMetaSize+i*(rsShardHdrSize+shardSize):]
		binary.LittleEndian.PutUint16(hdr[0:2], uint16(i))
		binary.LittleEndian.PutUint32(hdr[2:6], crc32.ChecksumIEEE(hdr[rsShardHdrSize:rsShardHdrSize+shardSize]))
	}
	return blob
}

// P0-1: a 254+2-shard geometry over a single-record (~6 KB blob) payload must
// be rejected as ErrCorrupt by BOTH Deserialize and Inspect before any
// Reed-Solomon matrix work — pre-fix, reedsolomon.New(254, 2) alone cost tens
// of milliseconds per blob regardless of its size.
func TestRSHostileShardCountRejectedFast(t *testing.T) {
	base := make([]byte, 8*BlockSize)
	blob := hostileRSBlob(t, 254, 2, 1)

	start := time.Now()
	if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Deserialize(254+2 over 1 record): err = %v, want ErrCorrupt", err)
	}
	if _, err := Inspect(blob); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Inspect(254+2 over 1 record): err = %v, want ErrCorrupt", err)
	}
	// Rejection happens in parseRSMeta, before reedsolomon.New: it is
	// sub-microsecond work. The pre-fix path cost >=60ms here; 50ms keeps
	// the assertion far from both that and scheduler noise (even under
	// -race).
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("hostile geometry rejection took %v, want well under 50ms", elapsed)
	}
}

// P0-1 (encode side): SerializeRS refuses geometries the decoder would
// reject, so an undeserializable blob can never be produced.
func TestSerializeRSRejectsUnjustifiedGeometry(t *testing.T) {
	dev, _ := fmtDevice(t, 8, []int64{1}) // payload: one record
	if _, err := dev.SerializeRS(254, 2); !errors.Is(err, ErrUnsupportedTier) {
		t.Fatalf("SerializeRS(254,2) over 1 record: err = %v, want ErrUnsupportedTier", err)
	}
	// Oversized parity is bounded too.
	if _, err := dev.SerializeRS(1, 200); !errors.Is(err, ErrUnsupportedTier) {
		t.Fatalf("SerializeRS(1,200): err = %v, want ErrUnsupportedTier", err)
	}
	// A payload-justified custom geometry still round-trips.
	dev8, base := fmtDevice(t, 16, []int64{1, 2, 3, 4, 5, 6, 7, 8})
	blob, err := dev8.SerializeRS(254, 2) // 8 records = 32864 bytes >= 253*128+1
	if err != nil {
		t.Fatalf("SerializeRS(254,2) over 8 records: %v", err)
	}
	dec, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	expectBlock(t, dec, base, 3, fill(0x43))
}

// P1-2: on an L1 record whose CRC fails, the decoder must not trust the
// record's index bytes. A record for block 7 whose index bytes are corrupted
// to read 3 must NOT put 3 in BadBlocks: the loss is unattributed, block 3
// stays untouched, block 7 degrades to base.
func TestL1CorruptIndexDoesNotBlameReadIndex(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{7})
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the index field 7 -> 3 WITHOUT resealing the record CRC.
	binary.LittleEndian.PutUint64(blob[headerSize:headerSize+8], 3)

	got, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("err = %v, want *PartialRecoveryError", err)
	}
	for _, b := range pre.BadBlocks {
		if b == 3 {
			t.Errorf("BadBlocks = %v blames block 3 on an unverified index", pre.BadBlocks)
		}
	}
	if !pre.Truncated {
		t.Error("index-unreadable loss must set Truncated")
	}
	expectBlock(t, got, base, 3, nil) // block 3 unchanged
	expectBlock(t, got, base, 7, nil) // block 7 degrades to base
}

// P1-3: duplicate records for the same block where the corrupt copy's read
// index matches the intact copy's applied block = superseded, full recovery,
// nil error. The opposite direction (no intact copy) stays a Truncated
// partial recovery.
func TestL1SupersededDuplicateIsFullRecovery(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{3})
	single, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	goodRec := single[headerSize:]
	badRec := bytes.Clone(goodRec)
	badRec[8] ^= 0xFF // corrupt data; CRC fails, index still reads 3

	mk := func(recs ...[]byte) []byte {
		blob := bytes.Clone(single[:headerSize])
		binary.LittleEndian.PutUint64(blob[6:14], uint64(len(recs)))
		reseal(blob)
		for _, r := range recs {
			blob = append(blob, r...)
		}
		return blob
	}

	t.Run("superseded: nil error", func(t *testing.T) {
		got, err := Deserialize(mk(badRec, goodRec), base)
		if err != nil {
			t.Fatalf("corrupt copy superseded by intact copy: err = %v, want nil", err)
		}
		expectBlock(t, got, base, 3, fill(0x43))
	})

	t.Run("not superseded: Truncated", func(t *testing.T) {
		got, err := Deserialize(mk(badRec), base)
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("err = %v, want *PartialRecoveryError", err)
		}
		if len(pre.BadBlocks) != 0 || !pre.Truncated {
			t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
		}
		expectBlock(t, got, base, 3, nil)
	})
}

// P1-3 (rs.go): the same superseded rule in the salvage path. Geometry as in
// TestRSSalvageFullRecoveryWhenLostShardsArePadding (lost shards carry only
// padding/parity), plus record 0's index edited 1 -> 2 without fixing its
// record CRC: the skipped record's read index names applied block 2, so the
// blob is fully recovered and err must be nil (pre-fix: an empty bad list
// forced Truncated).
func TestRSSalvageSupersededFullRecovery(t *testing.T) {
	base := covBase(16)
	dev := New(base)
	for b := int64(1); b <= 8; b++ {
		if _, err := dev.WriteAt(covBlock(byte(0x60+b)), b*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := dev.SerializeRS(254, 2)
	if err != nil {
		t.Fatal(err)
	}
	shardSize := int(binary.LittleEndian.Uint32(blob[headerSize+4 : headerSize+8]))
	// Record 0 (block 1) starts at payload offset 0 = shard 0 offset 0.
	_, data := covRSShard(blob, shardSize, 0)
	binary.LittleEndian.PutUint64(data[0:8], 2) // record CRC now fails; index reads 2
	covFixShardCRC(blob, shardSize, 0)
	covKillShard(t, blob, shardSize, 253) // padding-only data shard
	covKillShard(t, blob, shardSize, 254) // parity
	covKillShard(t, blob, shardSize, 255) // parity: missing=3 > K=2 -> salvage

	got, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("skipped record superseded by applied block 2: err = %v, want nil", err)
	}
	expectBlock(t, got, base, 1, nil)        // corrupt record's block degrades to base
	expectBlock(t, got, base, 2, fill(0x62)) // intact duplicate target applied
}

// P1-4: Inspect must mirror Deserialize's loss classes.
func TestInspectAlignsWithDeserialize(t *testing.T) {
	t.Run("negative-index CRC-valid record lands in BadBlocks", func(t *testing.T) {
		dev, _ := fmtDevice(t, 8, []int64{1})
		blob, err := dev.SerializeTier(TierL1)
		if err != nil {
			t.Fatal(err)
		}
		rec := blob[headerSize:]
		binary.LittleEndian.PutUint64(rec[0:8], ^uint64(4)) // int64 -5, CRC-verified below
		binary.LittleEndian.PutUint32(rec[8+BlockSize:], crc32.ChecksumIEEE(rec[:8+BlockSize]))
		info, err := Inspect(blob)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if len(info.BadBlocks) != 1 || info.BadBlocks[0] != -5 {
			t.Fatalf("BadBlocks = %v, want [-5] (verified impossible index)", info.BadBlocks)
		}
	})

	t.Run("truncated L1 reports Info, not ErrCorrupt", func(t *testing.T) {
		dev, base := fmtDevice(t, 8, []int64{1, 3})
		blob, err := dev.SerializeTier(TierL1)
		if err != nil {
			t.Fatal(err)
		}
		cut := blob[:headerSize+l1RecordSize+8] // record 1 cut to its index field
		if _, derr := Deserialize(cut, base); derr == nil {
			t.Fatal("premise: Deserialize should report partial recovery")
		}
		info, err := Inspect(cut)
		if err != nil {
			t.Fatalf("Inspect(truncated L1): %v, want Info", err)
		}
		if !info.Truncated {
			t.Error("Truncated must be set for a cut-off body")
		}
		if len(info.BadBlocks) != 1 || info.BadBlocks[0] != 3 {
			t.Errorf("BadBlocks = %v, want [3] (readable tail index)", info.BadBlocks)
		}
	})

	t.Run("truncated L0 reports Info, not ErrCorrupt", func(t *testing.T) {
		dev, _ := fmtDevice(t, 8, []int64{1, 3})
		blob, err := dev.SerializeTier(TierL0)
		if err != nil {
			t.Fatal(err)
		}
		info, err := Inspect(blob[:len(blob)-100])
		if err != nil {
			t.Fatalf("Inspect(truncated L0): %v, want Info", err)
		}
		if !info.Truncated {
			t.Error("Truncated must be set for a cut-off L0 body")
		}
	})

	t.Run("L2 lost index bytes count as UnattributedLoss, no -1 sentinel", func(t *testing.T) {
		base := covBase(2)
		dev := New(base)
		if _, err := dev.WriteAt(covBlock(0x77), 0); err != nil {
			t.Fatal(err)
		}
		blob, err := dev.SerializeRS(2, 1)
		if err != nil {
			t.Fatal(err)
		}
		shardSize := int(binary.LittleEndian.Uint32(blob[headerSize+4 : headerSize+8]))
		covKillShard(t, blob, shardSize, 0) // data shard holding the index field
		covKillShard(t, blob, shardSize, 2) // parity: missing=2 > K=1 -> salvage
		info, err := Inspect(blob)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		for _, b := range info.BadBlocks {
			if b == -1 {
				t.Errorf("BadBlocks = %v carries the removed -1 sentinel", info.BadBlocks)
			}
		}
		if info.UnattributedLoss == 0 || !info.Truncated {
			t.Errorf("got UnattributedLoss=%d Truncated=%v, want >0/true", info.UnattributedLoss, info.Truncated)
		}
	})

	t.Run("headerless-recoverable blob reports Info", func(t *testing.T) {
		dev, base := fmtDevice(t, 8, []int64{1, 3})
		blob, err := dev.SerializeTier(TierL1)
		if err != nil {
			t.Fatal(err)
		}
		blob[6] ^= 0xFF // corrupt blockCount: header CRC fails, records intact
		if _, derr := Deserialize(blob, base); derr == nil {
			t.Fatal("premise: headerless fallback reports partial recovery")
		}
		info, err := Inspect(blob)
		if err != nil {
			t.Fatalf("Inspect(headerless-recoverable): %v, want Info", err)
		}
		if !info.Truncated {
			t.Error("headerless recovery must set Truncated")
		}
	})
}
