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
	// Even a payload-justified 254+2 is rejected now: the absolute
	// rsMaxTotalShards=64 cap binds regardless of payload (a 33 KB payload
	// clears the 128-bytes-per-shard rule at N=254, but reedsolomon.New at
	// that count is O(total^3) CPU per decode).
	dev8, base := fmtDevice(t, 16, []int64{1, 2, 3, 4, 5, 6, 7, 8})
	if _, err := dev8.SerializeRS(254, 2); !errors.Is(err, ErrUnsupportedTier) {
		t.Fatalf("SerializeRS(254,2) over 8 records: err = %v, want ErrUnsupportedTier (total > 64)", err)
	}
	// A payload-justified geometry within the cap still round-trips.
	blob, err := dev8.SerializeRS(62, 2) // total 64; 8 records = 32864 bytes >= 61*128+1
	if err != nil {
		t.Fatalf("SerializeRS(62,2) over 8 records: %v", err)
	}
	dec, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	expectBlock(t, dec, base, 3, fill(0x43))
}

// Shard-cap regression: the worst legal geometry (62+2, total 64) at its
// minimal justifying payload must decode in well under 5 ms — the whole
// point of the cap — while 254+2 is rejected on both sides even when
// payload-justified (pre-cap it cost ~78 ms of reedsolomon.New per decode).
func TestRSShardCapWorstCaseFast(t *testing.T) {
	// 2 records = 8216 payload bytes: the smallest record count with
	// 62 <= ceil(payload/128), so 62+2 is the worst geometry a legal blob
	// can demand per payload byte.
	dev, base := fmtDevice(t, 16, []int64{1, 2})
	blob, err := dev.SerializeRS(62, 2)
	if err != nil {
		t.Fatalf("SerializeRS(62,2): %v", err)
	}
	start := time.Now()
	dec, err := Deserialize(blob, base)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	// The 5ms bound holds only without the race detector's ~10-40x
	// instrumentation overhead; the functional assertions run either way.
	if !raceEnabled && elapsed > 5*time.Millisecond {
		t.Fatalf("62+2 decode took %v, want < 5ms", elapsed)
	}
	expectBlock(t, dec, base, 1, fill(0x41))
	expectBlock(t, dec, base, 2, fill(0x42))

	// Decode side of the cap: a well-formed 254+2 blob over a justifying
	// payload (8 records) is ErrCorrupt for Deserialize and Inspect alike.
	hostile := hostileRSBlob(t, 254, 2, 8)
	if _, err := Deserialize(hostile, base); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Deserialize(254+2 over 8 records): err = %v, want ErrCorrupt", err)
	}
	if _, err := Inspect(hostile); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Inspect(254+2 over 8 records): err = %v, want ErrCorrupt", err)
	}
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

// P1-3 (tightened): a CRC-failed record is suppressed as a harmless
// duplicate ONLY when its data bytes are bit-identical to the record applied
// for its read index. The honest matrix:
//
//   - genuine duplicate, damage confined to the CRC (or index) bytes, data
//     identical to the applied copy  -> suppressed, nil error;
//   - duplicate with CORRUPT DATA (the old rule's false-suppression window)
//     -> Truncated: content differing from the applied copy is real loss;
//   - no intact copy at all -> Truncated.
func TestL1SupersededDuplicateIsFullRecovery(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{3})
	single, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	goodRec := single[headerSize:]

	mk := func(recs ...[]byte) []byte {
		blob := bytes.Clone(single[:headerSize])
		binary.LittleEndian.PutUint64(blob[6:14], uint64(len(recs)))
		reseal(blob)
		for _, r := range recs {
			blob = append(blob, r...)
		}
		return blob
	}
	wantTruncatedNoBad := func(t *testing.T, err error) *PartialRecoveryError {
		t.Helper()
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("err = %v, want *PartialRecoveryError", err)
		}
		if len(pre.BadBlocks) != 0 || !pre.Truncated {
			t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
		}
		return pre
	}

	t.Run("true duplicate (CRC bytes hit, data identical): nil error", func(t *testing.T) {
		badRec := bytes.Clone(goodRec)
		badRec[l1RecordSize-1] ^= 0xFF // CRC fails; index and data intact
		got, err := Deserialize(mk(badRec, goodRec), base)
		if err != nil {
			t.Fatalf("bit-identical duplicate: err = %v, want nil", err)
		}
		expectBlock(t, got, base, 3, fill(0x43))
	})

	t.Run("duplicate with corrupt data: Truncated, not suppressed", func(t *testing.T) {
		badRec := bytes.Clone(goodRec)
		badRec[8] ^= 0xFF // corrupt data; CRC fails, index still reads 3
		got, err := Deserialize(mk(badRec, goodRec), base)
		wantTruncatedNoBad(t, err)
		expectBlock(t, got, base, 3, fill(0x43)) // intact copy still applied
	})

	t.Run("no intact copy: Truncated", func(t *testing.T) {
		badRec := bytes.Clone(goodRec)
		badRec[8] ^= 0xFF
		got, err := Deserialize(mk(badRec), base)
		wantTruncatedNoBad(t, err)
		expectBlock(t, got, base, 3, nil)
	})
}

// Regression for the silent-loss window the old superseded heuristic left
// open: a one-byte flip in a record's INDEX bytes (5 -> 6, CRC fails) used
// to make the loss look like a superseded duplicate of applied block 6 —
// Deserialize returned nil and block 5 silently read base. Now the data
// comparison fails (block 6's content differs), so the loss is reported:
// non-nil PartialRecoveryError with Truncated, block 5 reads base, block 6
// intact, and Inspect reports the loss too.
func TestL1IndexFlipIsReportedLoss(t *testing.T) {
	dev, base := fmtDevice(t, 8, []int64{5, 6})
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Record 0 is block 5 (records sorted by index). Flip its index 5 -> 6
	// WITHOUT resealing the record CRC.
	binary.LittleEndian.PutUint64(blob[headerSize:headerSize+8], 6)

	got, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("err = %v, want *PartialRecoveryError (silently swallowed loss)", err)
	}
	if !pre.Truncated {
		t.Error("index-flipped record with differing data must set Truncated")
	}
	if len(pre.BadBlocks) != 0 {
		t.Errorf("BadBlocks = %v, want none (index unverified)", pre.BadBlocks)
	}
	expectBlock(t, got, base, 5, nil)        // lost write degrades to base
	expectBlock(t, got, base, 6, fill(0x46)) // intact record applied

	info, err := Inspect(blob)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.UnattributedLoss == 0 || !info.Truncated {
		t.Errorf("Inspect got UnattributedLoss=%d Truncated=%v, want >0/true", info.UnattributedLoss, info.Truncated)
	}
}

// P1-3 (rs.go): the same true-duplicate rule in the salvage path. Under the
// rsMaxTotalShards=64 cap no wire geometry can put a whole data shard in
// padding (see TestRSSalvageFullRecoveryWhenLostShardsArePadding), so this
// drives rsSalvage directly: one record per 4108-byte shard plus a lost
// padding-only shard. Record 0 is a byte-identical duplicate of block 2's
// record with only its CRC bytes smashed -> suppressed, full recovery, nil
// error. The counter-case (same shape but the duplicate's DATA corrupted)
// must stay a Truncated partial recovery — that was the old rule's silent
// false-suppression window.
func TestRSSalvageSupersededFullRecovery(t *testing.T) {
	base := covBase(16)
	dev := New(base)
	for b := int64(2); b <= 8; b++ {
		if _, err := dev.WriteAt(covBlock(byte(0x60+b)), b*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	payload := covL1Payload(t, dev) // 7 records: blocks 2..8
	// Prepend a duplicate of block 2's record (payload[0:4108]) -> 8 records.
	dup := bytes.Clone(payload[:l1RecordSize])
	full := append(dup, payload...)

	run := func(t *testing.T, corrupt func(rec []byte)) (*Device, error) {
		p := bytes.Clone(full)
		corrupt(p[:l1RecordSize])
		shards := covPayloadShards(p, 9, l1RecordSize)
		shards[8] = nil // lost padding-only shard
		return rsSalvage(shards, 9, l1RecordSize, len(p), base)
	}

	t.Run("bit-identical duplicate: nil error", func(t *testing.T) {
		got, err := run(t, func(rec []byte) { rec[l1RecordSize-1] ^= 0xFF })
		if err != nil {
			t.Fatalf("CRC-only damage on a true duplicate: err = %v, want nil", err)
		}
		expectBlock(t, got, base, 2, fill(0x62))
		expectBlock(t, got, base, 8, fill(0x68))
	})

	t.Run("data-corrupt duplicate: Truncated", func(t *testing.T) {
		got, err := run(t, func(rec []byte) { rec[8] ^= 0xFF })
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("err = %v, want *PartialRecoveryError", err)
		}
		if len(pre.BadBlocks) != 0 || !pre.Truncated {
			t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
		}
		expectBlock(t, got, base, 2, fill(0x62)) // intact copy still applied
	})
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
