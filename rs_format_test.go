package blockdevice

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// TestRSSerializeDeterminism mirrors TestSerializeDeterminism for TierL2:
// write order must not leak into the serialized bytes, and repeated
// SerializeRS calls on the same device must be byte-identical.
func TestRSSerializeDeterminism(t *testing.T) {
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

	b1, err := devSorted.SerializeRS(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := devShuffled.SerializeRS(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Error("write order changed serialized output; must be deterministic")
	}

	// Two SerializeRS calls on the same device are byte-identical.
	b3, err := devSorted.SerializeRS(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b3) {
		t.Error("repeated SerializeRS calls differ")
	}
}

// TestRSSerializeExactSize asserts the L2 blob length against the formula
// derived from the rs.go constants, with default shard geometry:
//
//	payload    = N * l1RecordSize
//	dataShards = min(rsDefaultData, max(1, payload/4096))
//	shardSize  = ceil(payload / dataShards)
//	len(blob)  = headerSize + rsMetaSize +
//	             (dataShards+rsDefaultParity)*(rsShardHdrSize+shardSize)
func TestRSSerializeExactSize(t *testing.T) {
	for _, n := range []int{1, 3, 10, 25} {
		t.Run(fmt.Sprintf("%dblocks", n), func(t *testing.T) {
			dirty := make([]int64, n)
			for i := range dirty {
				dirty[i] = int64(i)
			}
			dev, _ := fmtDevice(t, 32, dirty)
			blob, err := dev.SerializeRS(0, 0)
			if err != nil {
				t.Fatalf("SerializeRS: %v", err)
			}

			payload := n * l1RecordSize
			dataShards := payload / 4096
			if dataShards < 1 {
				dataShards = 1
			}
			if dataShards > rsDefaultData {
				dataShards = rsDefaultData
			}
			shardSize := (payload + dataShards - 1) / dataShards
			total := dataShards + rsDefaultParity
			want := headerSize + rsMetaSize + total*(rsShardHdrSize+shardSize)
			if len(blob) != want {
				t.Errorf("len(blob) = %d, want %d (N=%d data=%d shardSize=%d)",
					len(blob), want, n, dataShards, shardSize)
			}
		})
	}
}

// TestRSEmptyDelta pins down the zero-dirty-block encoding: header + rsMeta +
// (1+rsDefaultParity) empty shard headers (dataShards defaults to 1 for a
// zero-length payload, shardSize = 0), and it must round-trip to a device
// with no dirty blocks.
func TestRSEmptyDelta(t *testing.T) {
	dev, base := fmtDevice(t, 4, nil)
	blob, err := dev.SerializeRS(0, 0)
	if err != nil {
		t.Fatalf("SerializeRS: %v", err)
	}
	want := headerSize + rsMetaSize + (1+rsDefaultParity)*rsShardHdrSize
	if len(blob) != want {
		t.Errorf("empty delta len = %d, want %d (header+rsMeta+%d empty shard headers)",
			len(blob), want, 1+rsDefaultParity)
	}

	dev2, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if len(dev2.dirty) != 0 {
		t.Errorf("empty delta produced %d dirty blocks", len(dev2.dirty))
	}
	// The round-tripped device still reads pure base.
	for idx := int64(0); idx < 4; idx++ {
		expectBlock(t, dev2, base, idx, nil)
	}
}
