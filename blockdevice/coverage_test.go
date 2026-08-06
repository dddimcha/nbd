package blockdevice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
)

// --- helpers -----------------------------------------------------------

// covBase returns a base image of n blocks with distinguishable content.
func covBase(n int) []byte {
	base := make([]byte, n*BlockSize)
	for i := range base {
		base[i] = byte(i % 251)
	}
	return base
}

// covBlock returns a BlockSize buffer filled with b.
func covBlock(b byte) []byte {
	blk := make([]byte, BlockSize)
	for i := range blk {
		blk[i] = b
	}
	return blk
}

// covFixHeaderCRC recomputes the outer header CRC in place.
func covFixHeaderCRC(blob []byte) {
	binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))
}

// covFixRSMetaCRC recomputes the rsMeta CRC in place.
func covFixRSMetaCRC(blob []byte) {
	meta := blob[headerSize : headerSize+rsMetaSize]
	binary.LittleEndian.PutUint32(meta[16:20], crc32.ChecksumIEEE(meta[:16]))
}

// covRSShard returns the header and data slices of RS shard i.
func covRSShard(blob []byte, shardSize, i int) (hdr, data []byte) {
	stride := rsShardHdrSize + shardSize
	off := headerSize + rsMetaSize + i*stride
	return blob[off : off+rsShardHdrSize], blob[off+rsShardHdrSize : off+stride]
}

// covKillShard invalidates shard i's CRC so the decoder treats it as lost.
func covKillShard(t *testing.T, blob []byte, shardSize, i int) {
	t.Helper()
	hdr, data := covRSShard(blob, shardSize, i)
	stored := binary.LittleEndian.Uint32(hdr[2:6])
	binary.LittleEndian.PutUint32(hdr[2:6], stored^0xdeadbeef)
	_ = data
}

// covFixShardCRC recomputes shard i's CRC over its (possibly edited) data.
func covFixShardCRC(blob []byte, shardSize, i int) {
	hdr, data := covRSShard(blob, shardSize, i)
	binary.LittleEndian.PutUint32(hdr[2:6], crc32.ChecksumIEEE(data))
}

// --- format.go ---------------------------------------------------------

// Error() (format.go:61) — 0% covered.
func TestPartialRecoveryErrorString(t *testing.T) {
	tests := []struct {
		name string
		err  *PartialRecoveryError
		want []string
	}{
		{"blocks", &PartialRecoveryError{BadBlocks: []int64{3, 7}}, []string{"2 bad block(s)", "[3 7]", "truncated: false"}},
		{"truncated", &PartialRecoveryError{Truncated: true}, []string{"0 bad block(s)", "truncated: true"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.err.Error()
			for _, w := range tc.want {
				if !strings.Contains(s, w) {
					t.Errorf("Error() = %q, want substring %q", s, w)
				}
			}
		})
	}
}

// Two corrupt L1 records naming the SAME block index: neither index is
// verifiable (the CRC covers index+data), so the loss is unattributed and no
// block is blamed.
func TestL1DuplicateBadIndexDeduped(t *testing.T) {
	base := covBase(4)
	dev := New(base)
	if _, err := dev.WriteAt(covBlock(0xAA), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := dev.WriteAt(covBlock(0xBB), BlockSize); err != nil {
		t.Fatal(err)
	}
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite record 1's index to 0 (duplicate of record 0), then corrupt
	// both record CRCs so both are skipped and both report block 0.
	r1 := blob[headerSize+l1RecordSize : headerSize+2*l1RecordSize]
	binary.LittleEndian.PutUint64(r1[0:8], 0)
	blob[headerSize+8] ^= 0xFF           // corrupt record 0 data
	r1[8] ^= 0xFF                        // corrupt record 1 data
	dev2, err := Deserialize(blob, base) // both records bad, same index
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 0 || !pre.Truncated {
		t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true (CRC-failed indices are untrusted)", pre.BadBlocks, pre.Truncated)
	}
	got := make([]byte, BlockSize)
	if _, err := dev2.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, base[:BlockSize]) {
		t.Error("bad block 0 should read as base data")
	}
}

// Truncated tail with a readable, in-range index (format.go:226-228).
func TestL1TruncatedTailNamesBlock(t *testing.T) {
	base := covBase(4)
	dev := New(base)
	if _, err := dev.WriteAt(covBlock(0x11), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := dev.WriteAt(covBlock(0x22), 2*BlockSize); err != nil {
		t.Fatal(err)
	}
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Cut record 1 down to just its 8-byte index: the tail index (block 2)
	// is readable and in range, so it must be named in BadBlocks.
	cut := blob[:headerSize+l1RecordSize+8]
	dev2, err := Deserialize(cut, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 1 || pre.BadBlocks[0] != 2 {
		t.Fatalf("BadBlocks = %v, want [2]", pre.BadBlocks)
	}
	got := make([]byte, BlockSize)
	if _, err := dev2.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, covBlock(0x11)) {
		t.Error("intact record 0 not applied")
	}
}

// Truncated tail whose readable index is OUT of range is unattributable loss
// (format.go:226-228).
func TestL1TruncatedTailOutOfRangeIndex(t *testing.T) {
	base := covBase(4)
	dev := New(base)
	if _, err := dev.WriteAt(covBlock(0x11), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := dev.WriteAt(covBlock(0x22), 2*BlockSize); err != nil {
		t.Fatal(err)
	}
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	cut := blob[:headerSize+l1RecordSize+8]
	// Smash the tail's 8-byte index so it reads out of range.
	binary.LittleEndian.PutUint64(cut[headerSize+l1RecordSize:], ^uint64(0)>>1)
	_, err = Deserialize(cut, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 0 || !pre.Truncated {
		t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
	}
}

// L1 record whose CRC fails AND whose index is out of range is unattributable
// loss (format.go:262-264).
func TestL1CorruptRecordOutOfRangeIndex(t *testing.T) {
	base := covBase(2)
	dev := New(base)
	if _, err := dev.WriteAt(covBlock(0x33), 0); err != nil {
		t.Fatal(err)
	}
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	// Smash the index field: CRC now fails and the index is out of range.
	binary.LittleEndian.PutUint64(blob[headerSize:headerSize+8], ^uint64(0)>>1)
	_, err = Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 0 || !pre.Truncated {
		t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
	}
}

// deserializeHeaderless edge cases (format.go:295-297, 312-316, 324-326).
func TestHeaderlessFallbackEdges(t *testing.T) {
	base := covBase(4)
	mk := func(blocks ...int64) []byte {
		dev := New(base)
		for _, b := range blocks {
			if _, err := dev.WriteAt(covBlock(byte(0x40+b)), b*BlockSize); err != nil {
				t.Fatal(err)
			}
		}
		blob, err := dev.SerializeTier(TierL1)
		if err != nil {
			t.Fatal(err)
		}
		blob[6] ^= 0xFF // corrupt blockCount so the header CRC fails
		return blob
	}

	t.Run("empty body", func(t *testing.T) {
		blob := mk() // header only
		if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})

	t.Run("body not record aligned", func(t *testing.T) {
		blob := append(mk(0), 0x00) // one stray byte
		if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})

	t.Run("bad record is unattributed, survivor applied", func(t *testing.T) {
		blob := mk(1, 3)
		blob[headerSize+8] ^= 0xFF // corrupt record 0 data; its index is now untrusted
		dev2, err := Deserialize(blob, base)
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("want *PartialRecoveryError, got %v", err)
		}
		if len(pre.BadBlocks) != 0 || !pre.Truncated {
			t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true (CRC-failed index untrusted)", pre.BadBlocks, pre.Truncated)
		}
		got := make([]byte, BlockSize)
		if _, err := dev2.ReadAt(got, 3*BlockSize); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, covBlock(0x43)) {
			t.Error("surviving record for block 3 not applied")
		}
	})

	t.Run("no record recovers", func(t *testing.T) {
		blob := mk(0, 2)
		blob[headerSize+8] ^= 0xFF
		blob[headerSize+l1RecordSize+8] ^= 0xFF
		if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
}

// --- rs.go -------------------------------------------------------------

// Shard-count cap on the encode side (rs.go:88-90).
func TestSerializeRSTooManyShards(t *testing.T) {
	dev := New(covBase(1))
	if _, err := dev.SerializeRS(200, 100); !errors.Is(err, ErrUnsupportedTier) {
		t.Fatalf("want ErrUnsupportedTier for 300 shards, got %v", err)
	}
}

// TierL2 body shorter than rsMeta (rs.go:165-167).
func TestRSBodyTooShort(t *testing.T) {
	blob := make([]byte, headerSize)
	copy(blob[0:4], magic[:])
	blob[4] = formatVersion
	blob[5] = byte(TierL2)
	binary.LittleEndian.PutUint64(blob[6:14], 0)
	covFixHeaderCRC(blob)
	if _, err := Deserialize(blob, covBase(1)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

// Forged-but-CRC-valid rsMeta variants (rs.go:169-171, 178-180, 191-193,
// 195-197). Each case edits meta fields, refreshes the meta CRC (except the
// bad-CRC case), and must be rejected with ErrCorrupt.
func TestRSForgedMetaRejected(t *testing.T) {
	base := covBase(2)
	dev := New(base)
	if _, err := dev.WriteAt(covBlock(0x55), 0); err != nil {
		t.Fatal(err)
	}
	good, err := dev.SerializeRS(1, 1)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		mut  func(meta []byte) bool // returns whether to refresh meta CRC
	}{
		{"meta crc bad", func(meta []byte) bool {
			meta[0] ^= 0xFF
			return false
		}},
		{"zero data shards", func(meta []byte) bool {
			binary.LittleEndian.PutUint16(meta[0:2], 0)
			return true
		}},
		{"payload not record aligned", func(meta []byte) bool {
			binary.LittleEndian.PutUint64(meta[8:16], l1RecordSize-1)
			return true
		}},
		{"payload blockCount mismatch", func(meta []byte) bool {
			binary.LittleEndian.PutUint64(meta[8:16], 2*l1RecordSize)
			return true
		}},
		{"shard geometry cannot hold payload", func(meta []byte) bool {
			binary.LittleEndian.PutUint32(meta[4:8], 100) // < payloadLen with 1 data shard
			return true
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blob := bytes.Clone(good)
			if tc.mut(blob[headerSize : headerSize+rsMetaSize]) {
				covFixRSMetaCRC(blob)
			}
			if _, err := Deserialize(blob, base); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("want ErrCorrupt, got %v", err)
			}
		})
	}
}

// covL1Payload serializes dev at TierL1 and returns the record payload
// (header stripped), for building rsSalvage inputs directly.
func covL1Payload(t *testing.T, dev *Device) []byte {
	t.Helper()
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatal(err)
	}
	return blob[headerSize:]
}

// covPayloadShards slices an L1 payload into dataShards record-aligned
// shards of shardSize bytes each (the tail shard, if beyond the payload, is
// zero padding), returning a shard slice suitable for rsSalvage.
func covPayloadShards(payload []byte, dataShards, shardSize int) [][]byte {
	shards := make([][]byte, dataShards)
	for i := range shards {
		s := make([]byte, shardSize)
		if off := i * shardSize; off < len(payload) {
			copy(s, payload[off:])
		}
		shards[i] = s
	}
	return shards
}

// Salvage full-recovery path: a lost data shard that held only zero padding
// loses no record bytes, so every record still decodes and err must be nil.
//
// NOTE: with the rsMaxTotalShards=64 cap plus the 128-bytes-per-shard
// justification rule, no wire-format geometry can place a whole data shard
// in padding anymore (padding is always < dataShards <= 62 bytes while every
// shard is >= ~128 bytes), so this exercises rsSalvage directly with a
// geometry of one record per 4108-byte shard and a ninth, padding-only shard.
func TestRSSalvageFullRecoveryWhenLostShardsArePadding(t *testing.T) {
	base := covBase(16)
	dev := New(base)
	for b := int64(1); b <= 8; b++ {
		if _, err := dev.WriteAt(covBlock(byte(0x60+b)), b*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	payload := covL1Payload(t, dev) // 8 records, 4108 bytes each
	shards := covPayloadShards(payload, 9, l1RecordSize)
	shards[8] = nil // lost shard: pure padding, no record bytes

	dev2, err := rsSalvage(shards, 9, l1RecordSize, len(payload), base)
	if err != nil {
		t.Fatalf("want full recovery, got %v", err)
	}
	got := make([]byte, BlockSize)
	for b := int64(1); b <= 8; b++ {
		if _, err := dev2.ReadAt(got, b*BlockSize); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, covBlock(byte(0x60+b))) {
			t.Errorf("block %d not recovered", b)
		}
	}
}

// Salvage with no nameable block (rs.go:318-320): the record's index field
// lies in a lost shard, so the loss is unattributable — BadBlocks empty,
// Truncated set.
func TestRSSalvageUnattributableLoss(t *testing.T) {
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
	_, err = Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 0 || !pre.Truncated {
		t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
	}
}

// Salvage over intact shards whose record content is bad: the shard CRC is
// refreshed after the edit, so the shard reads as intact and the RECORD check
// must catch it — a failed record CRC means the index is untrusted, so the
// loss is unattributed either way (index plausible or smashed).
func TestRSSalvageBadRecordInIntactShard(t *testing.T) {
	base := covBase(4)
	mkBlob := func() ([]byte, int) {
		dev := New(base)
		if _, err := dev.WriteAt(covBlock(0x88), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := dev.WriteAt(covBlock(0x99), BlockSize); err != nil {
			t.Fatal(err)
		}
		blob, err := dev.SerializeRS(2, 1) // shardSize = l1RecordSize: one record per shard
		if err != nil {
			t.Fatal(err)
		}
		shardSize := int(binary.LittleEndian.Uint32(blob[headerSize+4 : headerSize+8]))
		if shardSize != l1RecordSize {
			t.Fatalf("test premise broken: shardSize=%d, want %d", shardSize, l1RecordSize)
		}
		return blob, shardSize
	}

	t.Run("index readable but unverified: unattributed", func(t *testing.T) {
		blob, shardSize := mkBlob()
		_, data := covRSShard(blob, shardSize, 0)
		data[8] ^= 0xFF // corrupt record payload; index bytes untouched but now untrusted
		covFixShardCRC(blob, shardSize, 0)
		covKillShard(t, blob, shardSize, 1) // lose shard 1
		covKillShard(t, blob, shardSize, 2) // lose parity: missing=2 > K=1 -> salvage
		_, err := Deserialize(blob, base)
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("want *PartialRecoveryError, got %v", err)
		}
		if len(pre.BadBlocks) != 0 || !pre.Truncated {
			t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true (CRC-failed index untrusted)", pre.BadBlocks, pre.Truncated)
		}
	})

	t.Run("index smashed: unattributable", func(t *testing.T) {
		blob, shardSize := mkBlob()
		_, data := covRSShard(blob, shardSize, 0)
		binary.LittleEndian.PutUint64(data[0:8], ^uint64(0)>>1) // out-of-range index
		covFixShardCRC(blob, shardSize, 0)
		covKillShard(t, blob, shardSize, 1)
		covKillShard(t, blob, shardSize, 2)
		_, err := Deserialize(blob, base)
		var pre *PartialRecoveryError
		if !errors.As(err, &pre) {
			t.Fatalf("want *PartialRecoveryError, got %v", err)
		}
		if len(pre.BadBlocks) != 0 || !pre.Truncated {
			t.Fatalf("got BadBlocks=%v Truncated=%v, want none/true", pre.BadBlocks, pre.Truncated)
		}
	})
}
