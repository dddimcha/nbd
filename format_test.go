package blockdevice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"testing"
)

// fmtDevice builds a device over a deterministic base with the given blocks
// written (fill byte = 0x40 + index).
func fmtDevice(t *testing.T, baseBlocks int, dirty []int64) (*Device, []byte) {
	t.Helper()
	base := devBase(baseBlocks)
	dev := New(base)
	for _, idx := range dirty {
		p := bytes.Repeat([]byte{byte(0x40 + idx)}, BlockSize)
		if _, err := dev.WriteAt(p, idx*BlockSize); err != nil {
			t.Fatalf("WriteAt(%d): %v", idx, err)
		}
	}
	return dev, base
}

// expectBlock asserts block idx of dev reads as fill (dirty) or as base.
func expectBlock(t *testing.T, dev *Device, base []byte, idx int64, dirtyFill *byte) {
	t.Helper()
	got := make([]byte, BlockSize)
	if _, err := dev.ReadAt(got, idx*BlockSize); err != nil {
		t.Fatalf("ReadAt(%d): %v", idx, err)
	}
	var want []byte
	if dirtyFill != nil {
		want = bytes.Repeat([]byte{*dirtyFill}, BlockSize)
	} else {
		want = base[idx*BlockSize : (idx+1)*BlockSize]
	}
	if !bytes.Equal(got, want) {
		t.Errorf("block %d content mismatch", idx)
	}
}

func fill(b byte) *byte { return &b }

// reseal recomputes the header CRC after the header bytes were edited.
func reseal(blob []byte) {
	binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))
}

func TestSerializeExactSize(t *testing.T) {
	tiers := []struct {
		tier    Tier
		recSize int
	}{
		{TierL0, 8 + BlockSize},
		{TierL1, 8 + BlockSize + 4},
	}
	const wantHeader = 4 + 1 + 1 + 8 + 4 // magic | version | tier | blockCount | headerCRC32
	for _, tt := range tiers {
		for _, n := range []int{0, 1, 3, 7} {
			t.Run(fmt.Sprintf("tier%d_%dblocks", tt.tier, n), func(t *testing.T) {
				dirty := make([]int64, n)
				for i := range dirty {
					dirty[i] = int64(i)
				}
				dev, _ := fmtDevice(t, 8, dirty)
				blob, err := dev.SerializeTier(tt.tier)
				if err != nil {
					t.Fatalf("SerializeTier: %v", err)
				}
				want := wantHeader + n*tt.recSize
				if len(blob) != want {
					t.Errorf("len(blob) = %d, want headerSize(%d) + %d*%d = %d",
						len(blob), wantHeader, n, tt.recSize, want)
				}
			})
		}
	}
}

func TestEmptyDeltaHeaderOnly(t *testing.T) {
	for _, tier := range []Tier{TierL0, TierL1} {
		dev, base := fmtDevice(t, 4, nil)
		blob, err := dev.SerializeTier(tier)
		if err != nil {
			t.Fatalf("tier %d: %v", tier, err)
		}
		if len(blob) != headerSize {
			t.Errorf("tier %d: empty delta len = %d, want header only (%d)", tier, len(blob), headerSize)
		}
		dev2, err := Deserialize(blob, base)
		if err != nil {
			t.Fatalf("tier %d: Deserialize: %v", tier, err)
		}
		if len(dev2.dirty) != 0 {
			t.Errorf("tier %d: empty delta produced %d dirty blocks", tier, len(dev2.dirty))
		}
	}
}

func TestSerializeDeterminism(t *testing.T) {
	for _, tier := range []Tier{TierL0, TierL1} {
		t.Run(fmt.Sprintf("tier%d", tier), func(t *testing.T) {
			blocks := []int64{6, 1, 4, 0, 7, 3}

			// Sorted-order writes.
			sorted := append([]int64(nil), blocks...)
			for i := range sorted {
				for j := i + 1; j < len(sorted); j++ {
					if sorted[j] < sorted[i] {
						sorted[i], sorted[j] = sorted[j], sorted[i]
					}
				}
			}
			devSorted, _ := fmtDevice(t, 8, sorted)

			// Random-order writes of the same state.
			shuffled := append([]int64(nil), blocks...)
			rng := rand.New(rand.NewSource(42))
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			devShuffled, _ := fmtDevice(t, 8, shuffled)

			b1, err := devSorted.SerializeTier(tier)
			if err != nil {
				t.Fatal(err)
			}
			b2, err := devShuffled.SerializeTier(tier)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b1, b2) {
				t.Error("write order changed serialized output; must be deterministic")
			}

			// Two Serialize calls on the same device are byte-identical.
			b3, err := devSorted.SerializeTier(tier)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b1, b3) {
				t.Error("repeated Serialize calls differ")
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	for _, tier := range []Tier{TierL0, TierL1} {
		t.Run(fmt.Sprintf("tier%d", tier), func(t *testing.T) {
			dirty := []int64{0, 2, 5, 7}
			dev, base := fmtDevice(t, 8, dirty)
			blob, err := dev.SerializeTier(tier)
			if err != nil {
				t.Fatalf("SerializeTier: %v", err)
			}
			dev2, err := Deserialize(blob, base)
			if err != nil {
				t.Fatalf("Deserialize: %v", err)
			}
			isDirty := map[int64]bool{}
			for _, idx := range dirty {
				isDirty[idx] = true
			}
			for idx := int64(0); idx < 8; idx++ {
				if isDirty[idx] {
					expectBlock(t, dev2, base, idx, fill(byte(0x40+idx)))
				} else {
					expectBlock(t, dev2, base, idx, nil)
				}
			}
		})
	}
}

func TestSerializeUnsupportedTier(t *testing.T) {
	// TierL2 is owned by another engineer and deliberately not covered here;
	// only a truly unknown tier value is asserted.
	dev, _ := fmtDevice(t, 2, []int64{0})
	if _, err := dev.SerializeTier(Tier(99)); !errors.Is(err, ErrUnsupportedTier) {
		t.Errorf("SerializeTier(99) err = %v, want ErrUnsupportedTier", err)
	}
}

func TestL1CorruptHeaderFallbackScan(t *testing.T) {
	dirty := []int64{1, 3, 6}
	dev, base := fmtDevice(t, 8, dirty)
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatalf("SerializeTier: %v", err)
	}
	// Break the header CRC (last header field) without touching records.
	blob[14] ^= 0xFF

	dev2, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError from header-fallback scan, got %v", err)
	}
	if len(pre.BadBlocks) != 0 {
		t.Errorf("all records intact, BadBlocks = %v, want empty", pre.BadBlocks)
	}
	if dev2 == nil {
		t.Fatal("fallback scan should return a usable device")
	}
	for _, idx := range dirty {
		expectBlock(t, dev2, base, idx, fill(byte(0x40+idx)))
	}
	expectBlock(t, dev2, base, 0, nil)
}

func TestL1CorruptRecordCRC(t *testing.T) {
	dirty := []int64{1, 3, 6}
	dev, base := fmtDevice(t, 8, dirty)
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatalf("SerializeTier: %v", err)
	}
	// Records sorted: [1, 3, 6]. Corrupt the CRC of the middle record (block 3).
	crcOff := headerSize + 1*l1RecordSize + 8 + BlockSize
	blob[crcOff] ^= 0xFF

	dev2, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 1 || pre.BadBlocks[0] != 3 {
		t.Fatalf("BadBlocks = %v, want [3]", pre.BadBlocks)
	}
	// Bad block reads as base; good records applied.
	expectBlock(t, dev2, base, 3, nil)
	expectBlock(t, dev2, base, 1, fill(0x41))
	expectBlock(t, dev2, base, 6, fill(0x46))
}

func TestL1TruncationMatrix(t *testing.T) {
	dirty := []int64{1, 3, 6}
	dev, base := fmtDevice(t, 8, dirty)
	full, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatalf("SerializeTier: %v", err)
	}
	n := len(dirty)

	// Truncate at every record boundary.
	for i := 0; i <= n; i++ {
		t.Run(fmt.Sprintf("boundary_%d_records", i), func(t *testing.T) {
			blob := bytes.Clone(full[:headerSize+i*l1RecordSize])
			dev2, err := Deserialize(blob, base)
			if i == n {
				if err != nil {
					t.Fatalf("untruncated blob: %v", err)
				}
			} else {
				var pre *PartialRecoveryError
				if !errors.As(err, &pre) {
					t.Fatalf("truncated to %d records: err = %v, want *PartialRecoveryError", i, err)
				}
				if dev2 == nil {
					t.Fatal("device must still be returned")
				}
			}
			// The first i records are applied, the rest degrade to base.
			for j, idx := range dirty {
				if j < i {
					expectBlock(t, dev2, base, idx, fill(byte(0x40+idx)))
				} else {
					expectBlock(t, dev2, base, idx, nil)
				}
			}
		})
	}

	// Truncate mid-record: within each record, cut a few bytes in.
	for i := 0; i < n; i++ {
		for _, extra := range []int{1, 8, l1RecordSize - 1} {
			t.Run(fmt.Sprintf("midrecord_%d_plus_%d", i, extra), func(t *testing.T) {
				blob := bytes.Clone(full[:headerSize+i*l1RecordSize+extra])
				dev2, err := Deserialize(blob, base)
				var pre *PartialRecoveryError
				if !errors.As(err, &pre) {
					t.Fatalf("mid-record truncation: err = %v, want *PartialRecoveryError", err)
				}
				// A tail carrying a readable index must report that block.
				if extra >= 8 {
					wantBad := dirty[i]
					found := false
					for _, b := range pre.BadBlocks {
						if b == wantBad {
							found = true
						}
					}
					if !found {
						t.Errorf("BadBlocks = %v, want to include cut-off block %d", pre.BadBlocks, wantBad)
					}
				}
				for j, idx := range dirty {
					if j < i {
						expectBlock(t, dev2, base, idx, fill(byte(0x40+idx)))
					} else {
						expectBlock(t, dev2, base, idx, nil)
					}
				}
			})
		}
	}
}

func TestL1ShuffledRecordsDecode(t *testing.T) {
	dirty := []int64{1, 3, 6}
	dev, base := fmtDevice(t, 8, dirty)
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatalf("SerializeTier: %v", err)
	}
	// Reverse the record order in place; records are self-describing so the
	// decoder must not care.
	body := blob[headerSize:]
	shuffled := bytes.Clone(blob)
	for i := 0; i < len(dirty); i++ {
		src := body[i*l1RecordSize : (i+1)*l1RecordSize]
		dstOff := headerSize + (len(dirty)-1-i)*l1RecordSize
		copy(shuffled[dstOff:dstOff+l1RecordSize], src)
	}

	dev2, err := Deserialize(shuffled, base)
	if err != nil {
		t.Fatalf("Deserialize(shuffled): %v", err)
	}
	for _, idx := range dirty {
		expectBlock(t, dev2, base, idx, fill(byte(0x40+idx)))
	}
}

func TestDeserializeRejects(t *testing.T) {
	dev, base := fmtDevice(t, 4, []int64{0})
	l0, _ := dev.SerializeTier(TierL0)

	tests := []struct {
		name string
		blob []byte
		base []byte
		want error
	}{
		{"too short", l0[:headerSize-1], base, ErrCorrupt},
		{"bad magic", func() []byte { b := bytes.Clone(l0); b[0] = 'X'; return b }(), base, ErrCorrupt},
		{"bad version", func() []byte { b := bytes.Clone(l0); b[4] = 99; return b }(), base, ErrCorrupt},
		{"unaligned base", l0, base[:len(base)-1], ErrCorrupt},
		{"unsupported tier in valid header", func() []byte {
			b := bytes.Clone(l0)
			b[5] = 99
			// Re-seal the header CRC so the tier switch is reached.
			reseal(b)
			return b
		}(), base, ErrUnsupportedTier},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Deserialize(tc.blob, tc.base); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestSerializeParallelByteIdentity covers the goroutine-fanned record
// writers (writeRecords / writeRecordsSharded), which only engage above
// serialMinPerWorker records per worker: a large delta serialized via the
// public API must be byte-identical to the single-threaded reference built
// with writeRecordRange, and the L2 blob must equal the flat-payload-split
// construction.
func TestSerializeParallelByteIdentity(t *testing.T) {
	const baseBlocks = 8192
	dirty := make([]int64, 0, baseBlocks/2)
	for i := int64(0); i < baseBlocks; i += 2 {
		dirty = append(dirty, i)
	}
	dev, _ := fmtDevice(t, baseBlocks, dirty)
	idxs := dev.sortedIndices()

	for _, tier := range []Tier{TierL0, TierL1} {
		recSize := l0RecordSize
		if tier == TierL1 {
			recSize = l1RecordSize
		}
		blob, err := dev.SerializeTier(tier)
		if err != nil {
			t.Fatalf("SerializeTier(%d): %v", tier, err)
		}
		want := make([]byte, len(idxs)*recSize)
		dev.writeRecordRange(want, idxs, recSize, tier == TierL1)
		if !bytes.Equal(blob[headerSize:], want) {
			t.Errorf("tier %d: parallel record section differs from sequential reference", tier)
		}
	}

	// L2: rebuild the reference blob from a flat sequential payload.
	blob, err := dev.SerializeTier(TierL2)
	if err != nil {
		t.Fatalf("SerializeTier(L2): %v", err)
	}
	payload := make([]byte, len(idxs)*l1RecordSize)
	dev.writeRecordRange(payload, idxs, l1RecordSize, true)
	dataShards, _ := rsDefaultShards(len(payload))
	shardSize := (len(payload) + dataShards - 1) / dataShards
	base := headerSize + rsMetaSize
	for i := 0; i < dataShards; i++ {
		lo := i * shardSize
		hi := lo + shardSize
		if hi > len(payload) {
			hi = len(payload)
		}
		got := blob[base+i*(rsShardHdrSize+shardSize)+rsShardHdrSize:]
		if !bytes.Equal(got[:hi-lo], payload[lo:hi]) {
			t.Errorf("L2 data shard %d differs from flat-payload reference", i)
		}
	}
	if dec, err := Deserialize(blob, devBase(baseBlocks)); err != nil {
		t.Fatalf("Deserialize(L2): %v", err)
	} else if len(dec.dirty) != len(idxs) {
		t.Fatalf("L2 roundtrip: got %d dirty blocks, want %d", len(dec.dirty), len(idxs))
	}
}
