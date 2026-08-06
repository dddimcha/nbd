package blockdevice_test

import (
	"bytes"
	"fmt"

	blockdevice "github.com/dddimcha/nbd"
)

// ExampleNew shows the basic copy-on-write cycle: create a device over an
// immutable base, write a block, and read it back merged over the base.
func ExampleNew() {
	base := make([]byte, 4*blockdevice.BlockSize) // zero-filled base image
	dev := blockdevice.New(base)

	// Write one full block at block index 1.
	block := make([]byte, blockdevice.BlockSize)
	copy(block, "hello, block device")
	if _, err := dev.WriteAt(block, 1*blockdevice.BlockSize); err != nil {
		fmt.Println("write:", err)
		return
	}

	// Read it back; untouched blocks still read as base (zeros).
	got := make([]byte, blockdevice.BlockSize)
	if _, err := dev.ReadAt(got, 1*blockdevice.BlockSize); err != nil {
		fmt.Println("read:", err)
		return
	}
	fmt.Println(string(bytes.TrimRight(got, "\x00")))

	if _, err := dev.ReadAt(got, 0); err != nil {
		fmt.Println("read:", err)
		return
	}
	fmt.Println("block 0 untouched:", bytes.Equal(got, base[:blockdevice.BlockSize]))
	// Output:
	// hello, block device
	// block 0 untouched: true
}

// ExampleDevice_Serialize round-trips a device through Serialize/Deserialize
// and shows that the delta stays proportional to the changed blocks, not to
// the base image.
func ExampleDevice_Serialize() {
	base := make([]byte, 256*blockdevice.BlockSize) // 1 MiB base image
	dev := blockdevice.New(base)

	// Dirty just two of the 256 blocks.
	block := make([]byte, blockdevice.BlockSize)
	copy(block, "delta")
	dev.WriteAt(block, 7*blockdevice.BlockSize)
	dev.WriteAt(block, 42*blockdevice.BlockSize)

	blob, err := dev.Serialize()
	if err != nil {
		fmt.Println("serialize:", err)
		return
	}
	fmt.Printf("base:  %d bytes\n", len(base))
	fmt.Printf("delta: %d bytes\n", len(blob))

	// Rebuild from base + delta and verify the write survived.
	dev2, err := blockdevice.Deserialize(blob, base)
	if err != nil {
		fmt.Println("deserialize:", err)
		return
	}
	got := make([]byte, blockdevice.BlockSize)
	dev2.ReadAt(got, 42*blockdevice.BlockSize)
	fmt.Println("round-trip:", bytes.Equal(got, block))
	// Output:
	// base:  1048576 bytes
	// delta: 8226 bytes
	// round-trip: true
}

// ExampleDevice_SerializeRS corrupts part of a TierL2 (Reed-Solomon) blob and
// shows that Deserialize reconstructs the lost shard transparently: no error,
// identical data.
func ExampleDevice_SerializeRS() {
	base := make([]byte, 64*blockdevice.BlockSize)
	dev := blockdevice.New(base)

	block := make([]byte, blockdevice.BlockSize)
	copy(block, "precious data")
	for i := int64(0); i < 8; i++ {
		dev.WriteAt(block, i*blockdevice.BlockSize)
	}

	// 8 data shards + 2 parity: up to 2 lost shards are recoverable.
	blob, err := dev.SerializeRS(8, 2)
	if err != nil {
		fmt.Println("serialize:", err)
		return
	}

	// Corrupt a stretch of the first data shard. Its CRC now fails, so the
	// decoder treats the whole shard as an erasure and rebuilds it from
	// parity.
	for i := 100; i < 200; i++ {
		blob[i] ^= 0xFF
	}

	dev2, err := blockdevice.Deserialize(blob, base)
	if err != nil {
		fmt.Println("deserialize:", err)
		return
	}
	got := make([]byte, blockdevice.BlockSize)
	dev2.ReadAt(got, 0)
	fmt.Println("recovered:", bytes.Equal(got, block))
	// Output:
	// recovered: true
}
