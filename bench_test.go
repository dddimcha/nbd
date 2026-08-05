package blockdevice

import (
	"bytes"
	"testing"
)

const (
	benchBaseSize    = 64 << 20 // 64 MiB
	benchBaseBlocks  = benchBaseSize / BlockSize
	benchDirtyBlocks = benchBaseBlocks / 10 // 10% dirty
)

func benchBase() []byte {
	base := make([]byte, benchBaseSize)
	for i := range base {
		base[i] = byte(i % 251)
	}
	return base
}

// benchDevice returns a device over a 64 MiB base with 10% of the blocks
// dirty, spread evenly across the device.
func benchDevice(b *testing.B) *Device {
	b.Helper()
	dev := New(benchBase())
	p := bytes.Repeat([]byte{0xA5}, BlockSize)
	stride := int64(benchBaseBlocks / benchDirtyBlocks)
	for i := 0; i < benchDirtyBlocks; i++ {
		if _, err := dev.WriteAt(p, int64(i)*stride*BlockSize); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
	}
	return dev
}

func BenchmarkReadAt(b *testing.B) {
	dev := benchDevice(b)
	p := make([]byte, BlockSize)
	b.ReportAllocs()
	b.SetBytes(BlockSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(i%benchBaseBlocks) * BlockSize
		if _, err := dev.ReadAt(p, off); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteAt(b *testing.B) {
	dev := benchDevice(b)
	p := bytes.Repeat([]byte{0x5A}, BlockSize)
	b.ReportAllocs()
	b.SetBytes(BlockSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(i%benchBaseBlocks) * BlockSize
		if _, err := dev.WriteAt(p, off); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSerializeL0(b *testing.B) {
	dev := benchDevice(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := dev.SerializeTier(TierL0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSerializeL1(b *testing.B) {
	dev := benchDevice(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := dev.SerializeTier(TierL1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeserializeL0(b *testing.B) {
	dev := benchDevice(b)
	blob, err := dev.SerializeTier(TierL0)
	if err != nil {
		b.Fatal(err)
	}
	base := benchBase()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Deserialize(blob, base); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeserializeL1(b *testing.B) {
	dev := benchDevice(b)
	blob, err := dev.SerializeTier(TierL1)
	if err != nil {
		b.Fatal(err)
	}
	base := benchBase()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Deserialize(blob, base); err != nil {
			b.Fatal(err)
		}
	}
}
