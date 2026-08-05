package blockdevice

import "errors"

// BlockSize is the fixed block size in bytes. All offsets and lengths passed
// to ReadAt/WriteAt must be multiples of BlockSize.
const BlockSize = 4096

// Typed errors returned by Device operations.
var (
	// ErrUnaligned is returned when an offset or length is not a multiple
	// of BlockSize.
	ErrUnaligned = errors.New("blockdevice: offset or length not block-aligned")
	// ErrOutOfRange is returned when a request extends beyond the device.
	ErrOutOfRange = errors.New("blockdevice: request out of range")
)

// Device is an in-memory copy-on-write block device. The base data is never
// modified; changed blocks live in a per-block dirty overlay.
type Device struct {
	base  []byte
	dirty map[int64][]byte // block index -> BlockSize-byte copy
}

// New returns a Device backed by base. The base slice is retained but never
// modified. len(base) must be a multiple of BlockSize; New panics otherwise
// (programmer error, not untrusted input).
func New(base []byte) *Device {
	if len(base)%BlockSize != 0 {
		panic("blockdevice: base length not a multiple of BlockSize")
	}
	return &Device{
		base:  base,
		dirty: make(map[int64][]byte),
	}
}

// checkRange validates alignment and bounds for an I/O of length n at off.
func (d *Device) checkRange(n int, off int64) error {
	if off%BlockSize != 0 || n%BlockSize != 0 {
		return ErrUnaligned
	}
	// Overflow-safe bounds check: never compute off+n directly — for a huge
	// off the sum wraps negative and would pass a naive comparison. n is a
	// slice length, so 0 <= n <= MaxInt; comparing n against the remaining
	// space (len(base)-off, non-negative once off is validated) cannot wrap.
	if off < 0 || off > int64(len(d.base)) || int64(n) > int64(len(d.base))-off {
		return ErrOutOfRange
	}
	return nil
}

// ReadAt implements io.ReaderAt. Each block is served from the dirty overlay
// if present, else from the base. off and len(p) must be block-aligned and
// within the device.
func (d *Device) ReadAt(p []byte, off int64) (int, error) {
	if err := d.checkRange(len(p), off); err != nil {
		return 0, err
	}
	n := 0
	for n < len(p) {
		// checkRange guarantees off+len(p) <= len(base), so idx*BlockSize
		// stays within int64 and within the base slice.
		idx := (off + int64(n)) / BlockSize
		src := d.base[idx*BlockSize : (idx+1)*BlockSize]
		if b, ok := d.dirty[idx]; ok {
			src = b
		}
		copy(p[n:n+BlockSize], src)
		n += BlockSize
	}
	return n, nil
}

// WriteAt implements io.WriterAt. Incoming bytes are copied into per-block
// buffers; the caller's slice is never retained and the base is never
// modified. off and len(p) must be block-aligned and within the device.
func (d *Device) WriteAt(p []byte, off int64) (int, error) {
	if err := d.checkRange(len(p), off); err != nil {
		return 0, err
	}
	n := 0
	for n < len(p) {
		idx := (off + int64(n)) / BlockSize
		b, ok := d.dirty[idx]
		if !ok {
			b = make([]byte, BlockSize)
			d.dirty[idx] = b
		}
		copy(b, p[n:n+BlockSize])
		n += BlockSize
	}
	return n, nil
}
