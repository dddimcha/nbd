package blockdevice

import (
	"bytes"
	"errors"
	"testing"
)

func newTestDevice(t *testing.T, blocks int) (*Device, []byte) {
	t.Helper()
	base := make([]byte, blocks*BlockSize)
	for i := range base {
		base[i] = byte(i % 251)
	}
	return New(base), base
}

func writeBlock(t *testing.T, d *Device, idx int64, fill byte) []byte {
	t.Helper()
	b := bytes.Repeat([]byte{fill}, BlockSize)
	if _, err := d.WriteAt(b, idx*BlockSize); err != nil {
		t.Fatalf("WriteAt(%d): %v", idx, err)
	}
	return b
}

func readBlock(t *testing.T, d *Device, idx int64) []byte {
	t.Helper()
	p := make([]byte, BlockSize)
	if _, err := d.ReadAt(p, idx*BlockSize); err != nil {
		t.Fatalf("ReadAt(%d): %v", idx, err)
	}
	return p
}

func TestL0RoundTrip(t *testing.T) {
	dev, base := newTestDevice(t, 8)
	want1 := writeBlock(t, dev, 1, 0xAA)
	want5 := writeBlock(t, dev, 5, 0xBB)

	blob, err := dev.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dev2, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got := readBlock(t, dev2, 1); !bytes.Equal(got, want1) {
		t.Error("block 1 mismatch after L0 round-trip")
	}
	if got := readBlock(t, dev2, 5); !bytes.Equal(got, want5) {
		t.Error("block 5 mismatch after L0 round-trip")
	}
	if got := readBlock(t, dev2, 0); !bytes.Equal(got, base[:BlockSize]) {
		t.Error("clean block 0 should read as base")
	}
}

func TestL1RoundTrip(t *testing.T) {
	dev, base := newTestDevice(t, 8)
	want2 := writeBlock(t, dev, 2, 0xCC)
	want7 := writeBlock(t, dev, 7, 0xDD)

	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatalf("SerializeTier(L1): %v", err)
	}
	dev2, err := Deserialize(blob, base)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got := readBlock(t, dev2, 2); !bytes.Equal(got, want2) {
		t.Error("block 2 mismatch after L1 round-trip")
	}
	if got := readBlock(t, dev2, 7); !bytes.Equal(got, want7) {
		t.Error("block 7 mismatch after L1 round-trip")
	}
}

func TestL1CorruptRecordPartialRecovery(t *testing.T) {
	dev, base := newTestDevice(t, 8)
	writeBlock(t, dev, 1, 0x11)
	want3 := writeBlock(t, dev, 3, 0x33)
	want6 := writeBlock(t, dev, 6, 0x66)

	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		t.Fatalf("SerializeTier(L1): %v", err)
	}
	// Corrupt the data of the first record (block 1): flip a byte inside
	// its data area, after the header (18) and record index (8).
	blob[18+8+100] ^= 0xFF

	dev2, err := Deserialize(blob, base)
	var pre *PartialRecoveryError
	if !errors.As(err, &pre) {
		t.Fatalf("want *PartialRecoveryError, got %v", err)
	}
	if len(pre.BadBlocks) != 1 || pre.BadBlocks[0] != 1 {
		t.Fatalf("BadBlocks = %v, want [1]", pre.BadBlocks)
	}
	if dev2 == nil {
		t.Fatal("device should still be returned on partial recovery")
	}
	// Good records applied.
	if got := readBlock(t, dev2, 3); !bytes.Equal(got, want3) {
		t.Error("block 3 should be applied")
	}
	if got := readBlock(t, dev2, 6); !bytes.Equal(got, want6) {
		t.Error("block 6 should be applied")
	}
	// Bad block degrades to base content.
	if got := readBlock(t, dev2, 1); !bytes.Equal(got, base[BlockSize:2*BlockSize]) {
		t.Error("corrupt block 1 should read as base data")
	}
}
