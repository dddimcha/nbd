package blockdevice

import (
	"bytes"
	"errors"
	"testing"
)

func TestInspectRoundTrip(t *testing.T) {
	base := make([]byte, 4*BlockSize)
	dev := New(base)
	blk := bytes.Repeat([]byte{0xAB}, BlockSize)
	if _, err := dev.WriteAt(blk, 2*BlockSize); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []Tier{TierL0, TierL1, TierL2} {
		blob, err := dev.SerializeTier(tier)
		if err != nil {
			t.Fatalf("tier %d: %v", tier, err)
		}
		info, err := Inspect(blob)
		if err != nil {
			t.Fatalf("tier %d: %v", tier, err)
		}
		if info.Tier != tier || info.BlockCount != 1 ||
			info.BlobSize != len(blob) || len(info.BadBlocks) != 0 {
			t.Fatalf("tier %d: bad info %+v", tier, info)
		}
	}
}

func TestInspectCorrupt(t *testing.T) {
	if _, err := Inspect([]byte("not a delta")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestInspectL1BadBlock(t *testing.T) {
	base := make([]byte, 2*BlockSize)
	dev := New(base)
	dev.WriteAt(bytes.Repeat([]byte{1}, BlockSize), 0)
	blob, _ := dev.SerializeTier(TierL1)
	blob[headerSize+8+100] ^= 0xFF // flip a data byte inside the record
	info, err := Inspect(blob)
	if err != nil {
		t.Fatal(err)
	}
	// The record CRC covers index+data, so the failed record's index is
	// unreadable: the loss is counted, not attributed to a block.
	if len(info.BadBlocks) != 0 || info.UnattributedLoss != 1 || !info.Truncated {
		t.Fatalf("got BadBlocks=%v UnattributedLoss=%d Truncated=%v, want none/1/true",
			info.BadBlocks, info.UnattributedLoss, info.Truncated)
	}
}
