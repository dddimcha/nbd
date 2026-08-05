package blockdevice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// rsTestDevice returns a device with enough dirty blocks to force the full
// 10+2 shard geometry, plus its base.
func rsTestDevice(t *testing.T) (*Device, []byte) {
	t.Helper()
	base := make([]byte, 64*BlockSize)
	dev := New(base)
	buf := make([]byte, BlockSize)
	for i := 0; i < 16; i++ {
		for j := range buf {
			buf[j] = byte(i*31 + j)
		}
		if _, err := dev.WriteAt(buf, int64(i*3)*BlockSize); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}
	return dev, base
}

// corruptShard flips bytes inside the data area of the n-th shard of an L2 blob.
func corruptShard(t *testing.T, blob []byte, shard int) {
	t.Helper()
	meta := blob[headerSize : headerSize+rsMetaSize]
	shardSize := int(binary.LittleEndian.Uint32(meta[4:8]))
	stride := rsShardHdrSize + shardSize
	off := headerSize + rsMetaSize + shard*stride + rsShardHdrSize
	for i := 0; i < 16 && i < shardSize; i++ {
		blob[off+i] ^= 0xFF
	}
}

func TestL2RoundTrip(t *testing.T) {
	dev, base := rsTestDevice(t)
	blob, err := dev.SerializeTier(TierL2)
	if err != nil {
		t.Fatalf("SerializeTier(TierL2): %v", err)
	}
	got, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	want := make([]byte, len(base))
	have := make([]byte, len(base))
	if _, err := dev.ReadAt(want, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := got.ReadAt(have, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, have) {
		t.Fatal("round-trip mismatch")
	}
}

func TestL2RecoversTwoCorruptShards(t *testing.T) {
	dev, base := rsTestDevice(t)
	blob, err := dev.SerializeRS(10, 2)
	if err != nil {
		t.Fatalf("SerializeRS: %v", err)
	}
	// Corrupt every pair of the 12 shards; each must fully recover.
	for a := 0; a < 12; a++ {
		for b := a + 1; b < 12; b++ {
			c := append([]byte(nil), blob...)
			corruptShard(t, c, a)
			corruptShard(t, c, b)
			got, err := Deserialize(c, base)
			if err != nil {
				t.Fatalf("shards (%d,%d): want full recovery, got %v", a, b, err)
			}
			want := make([]byte, len(base))
			have := make([]byte, len(base))
			dev.ReadAt(want, 0)
			got.ReadAt(have, 0)
			if !bytes.Equal(want, have) {
				t.Fatalf("shards (%d,%d): data mismatch after recovery", a, b)
			}
		}
	}
}

func TestL2ThreeCorruptShardsPartial(t *testing.T) {
	dev, base := rsTestDevice(t)
	blob, err := dev.SerializeRS(10, 2)
	if err != nil {
		t.Fatalf("SerializeRS: %v", err)
	}
	c := append([]byte(nil), blob...)
	corruptShard(t, c, 0)
	corruptShard(t, c, 4)
	corruptShard(t, c, 7)

	got, err := Deserialize(c, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if got == nil {
		t.Fatal("device must still be usable")
	}
	// The device stays fully readable; every block matches either the
	// original overlay or falls back to base data.
	want := make([]byte, len(base))
	have := make([]byte, len(base))
	if _, err := dev.ReadAt(want, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := got.ReadAt(have, 0); err != nil {
		t.Fatal(err)
	}
	matched := 0
	for i := 0; i < len(base)/BlockSize; i++ {
		blk := have[i*BlockSize : (i+1)*BlockSize]
		orig := want[i*BlockSize : (i+1)*BlockSize]
		fallback := base[i*BlockSize : (i+1)*BlockSize]
		if !bytes.Equal(blk, orig) && !bytes.Equal(blk, fallback) {
			t.Fatalf("block %d is neither original nor base data", i)
		}
		if bytes.Equal(blk, orig) {
			matched++
		}
	}
	if matched == 0 {
		t.Fatal("no intact blocks survived; expected partial recovery")
	}
}
