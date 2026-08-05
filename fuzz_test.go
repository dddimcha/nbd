package blockdevice

import (
	"bytes"
	"testing"
)

// FuzzDeserialize feeds mutated blobs to Deserialize. It must never panic;
// whenever a device is returned (even alongside a PartialRecoveryError) it
// must serve every block via ReadAt without panicking.
func FuzzDeserialize(f *testing.F) {
	const baseBlocks = 8
	base := make([]byte, baseBlocks*BlockSize)
	for i := range base {
		base[i] = byte((i*7 + i/BlockSize) % 249)
	}

	seedDev := New(bytes.Clone(base))
	for _, idx := range []int64{1, 3, 6} {
		p := bytes.Repeat([]byte{byte(0x40 + idx)}, BlockSize)
		if _, err := seedDev.WriteAt(p, idx*BlockSize); err != nil {
			f.Fatalf("seed WriteAt: %v", err)
		}
	}
	l0, err := seedDev.SerializeTier(TierL0)
	if err != nil {
		f.Fatalf("seed L0 Serialize: %v", err)
	}
	l1, err := seedDev.SerializeTier(TierL1)
	if err != nil {
		f.Fatalf("seed L1 Serialize: %v", err)
	}
	empty, err := New(bytes.Clone(base)).Serialize()
	if err != nil {
		f.Fatalf("seed empty Serialize: %v", err)
	}

	f.Add(l0)
	f.Add(l1)
	f.Add(empty)
	f.Add([]byte{})
	f.Add([]byte("BDEV"))
	f.Add(l1[:len(l1)-5]) // mid-record truncation

	f.Fuzz(func(t *testing.T, blob []byte) {
		dev, err := Deserialize(blob, base)
		if dev == nil {
			if err == nil {
				t.Fatal("Deserialize returned nil device and nil error")
			}
			return
		}
		// Any returned device must serve reads over its whole range.
		p := make([]byte, BlockSize)
		for idx := int64(0); idx < baseBlocks; idx++ {
			if _, rerr := dev.ReadAt(p, idx*BlockSize); rerr != nil {
				t.Fatalf("ReadAt(%d) on deserialized device: %v", idx, rerr)
			}
		}
	})
}
