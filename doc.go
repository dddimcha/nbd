// Package blockdevice implements an in-memory copy-on-write block device.
//
// # Model
//
// A Device is created over an immutable base image (for example, a filesystem
// image shared by many sandboxes). The base is never modified: WriteAt copies
// the caller's bytes into a per-block dirty overlay, and ReadAt merges that
// overlay over the base. All I/O is aligned to BlockSize (4096 bytes).
//
// The overlay serializes to a compact delta containing only the changed
// blocks; Deserialize rebuilds an equivalent device from the base plus that
// delta. This is the pause/resume model: many devices share one base and each
// persists only its own delta.
//
//	dev := blockdevice.New(base)
//	dev.WriteAt(block, 4096)
//	blob, _ := dev.Serialize()
//	dev2, _ := blockdevice.Deserialize(blob, base)
//
// # Tiers
//
// Serialization offers three integrity tiers (see SerializeTier):
//
//   - TierL0 (default): smallest possible — 8 bytes of overhead per block, no
//     protection beyond the header CRC.
//   - TierL1: a CRC32 per record. Corrupt records are detected, degraded to
//     base data, and reported via *PartialRecoveryError alongside a still
//     usable device.
//   - TierL2 (SerializeRS): the L1 payload wrapped in Reed-Solomon shards; up
//     to K lost or corrupt shards are reconstructed transparently, and beyond
//     K decoding degrades to per-record salvage.
//
// Every record carries its own block index, so a decoder survives reordering,
// gaps and truncation. Deserialize never panics on untrusted input; it
// returns ErrCorrupt when nothing is decodable and *PartialRecoveryError when
// only part of the delta survived.
package blockdevice
