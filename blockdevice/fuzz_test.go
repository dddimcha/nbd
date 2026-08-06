package blockdevice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

const fuzzBaseBlocks = 8

// fuzzBase returns the deterministic base image shared by the fuzz targets.
func fuzzBase() []byte {
	base := make([]byte, fuzzBaseBlocks*BlockSize)
	for i := range base {
		base[i] = byte((i*7 + i/BlockSize) % 249)
	}
	return base
}

// addFuzzSeeds seeds f with well-formed blobs at every tier plus a few
// degenerate shapes.
func addFuzzSeeds(f *testing.F, base []byte) {
	f.Helper()
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
	l2, err := seedDev.SerializeTier(TierL2)
	if err != nil {
		f.Fatalf("seed L2 Serialize: %v", err)
	}
	empty, err := New(bytes.Clone(base)).Serialize()
	if err != nil {
		f.Fatalf("seed empty Serialize: %v", err)
	}

	f.Add(l0)
	f.Add(l1)
	f.Add(l2)
	f.Add(empty)
	f.Add([]byte{})
	f.Add([]byte("BDEV"))
	f.Add(l1[:len(l1)-5]) // mid-record truncation
	f.Add(l2[:len(l2)-7]) // mid-shard truncation
}

// FuzzDeserialize feeds mutated blobs to Deserialize. It must never panic;
// whenever a device is returned (even alongside a PartialRecoveryError) it
// must serve every block via ReadAt without panicking.
func FuzzDeserialize(f *testing.F) {
	base := fuzzBase()
	addFuzzSeeds(f, base)

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
		for idx := int64(0); idx < fuzzBaseBlocks; idx++ {
			if _, rerr := dev.ReadAt(p, idx*BlockSize); rerr != nil {
				t.Fatalf("ReadAt(%d) on deserialized device: %v", idx, rerr)
			}
		}
	})
}

// FuzzDeserializeResealed re-seals the header CRC after mutation so the fuzz
// input gets PAST the header gate that plain FuzzDeserialize mutations almost
// always die on, exercising the body decoders (L0/L1 records, rsMeta, shard
// section) with hostile-but-header-valid input. It cross-checks Deserialize
// and Inspect: neither may panic, and a blob Deserialize decodes cleanly must
// not be ErrCorrupt to Inspect.
func FuzzDeserializeResealed(f *testing.F) {
	base := fuzzBase()
	addFuzzSeeds(f, base)

	f.Fuzz(func(t *testing.T, blob []byte) {
		blob = bytes.Clone(blob)
		if len(blob) >= headerSize {
			// Reseal: bytes 14:18 = crc32 of bytes 0:14.
			binary.LittleEndian.PutUint32(blob[14:18], crc32.ChecksumIEEE(blob[:14]))
		}
		dev, derr := Deserialize(blob, base)
		if dev == nil && derr == nil {
			t.Fatal("Deserialize returned nil device and nil error")
		}
		_, ierr := Inspect(blob)
		if derr == nil && errors.Is(ierr, ErrCorrupt) {
			t.Fatalf("error-class mismatch: Deserialize succeeded cleanly but Inspect returned %v", ierr)
		}
		if dev != nil {
			p := make([]byte, BlockSize)
			for idx := int64(0); idx < fuzzBaseBlocks; idx++ {
				if _, rerr := dev.ReadAt(p, idx*BlockSize); rerr != nil {
					t.Fatalf("ReadAt(%d) on deserialized device: %v", idx, rerr)
				}
			}
		}
	})
}
