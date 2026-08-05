package blockdevice

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// Interface satisfaction, checked at compile time.
var (
	_ io.ReaderAt = (*Device)(nil)
	_ io.WriterAt = (*Device)(nil)
)

// devBase builds a deterministic base of the given block count.
func devBase(blocks int) []byte {
	base := make([]byte, blocks*BlockSize)
	for i := range base {
		base[i] = byte((i*7 + i/BlockSize) % 249)
	}
	return base
}

func TestDeviceIO(t *testing.T) {
	const blocks = 8
	type op struct {
		write   bool
		off     int64
		length  int   // for reads: len(p); for writes: len of pattern buffer
		fill    byte  // write fill byte
		wantErr error // expected error, nil = success
	}
	tests := []struct {
		name string
		ops  []op
		// verify: block index -> expected fill byte; blocks absent read as base.
		wantFill map[int64]byte
	}{
		{
			name:     "read from base only",
			ops:      []op{{write: false, off: 0, length: BlockSize}},
			wantFill: map[int64]byte{},
		},
		{
			name: "read after write",
			ops: []op{
				{write: true, off: 2 * BlockSize, length: BlockSize, fill: 0xAB},
			},
			wantFill: map[int64]byte{2: 0xAB},
		},
		{
			name: "multi-block write spanning several blocks",
			ops: []op{
				{write: true, off: 1 * BlockSize, length: 3 * BlockSize, fill: 0x5C},
			},
			wantFill: map[int64]byte{1: 0x5C, 2: 0x5C, 3: 0x5C},
		},
		{
			name: "rewrite same block",
			ops: []op{
				{write: true, off: 4 * BlockSize, length: BlockSize, fill: 0x11},
				{write: true, off: 4 * BlockSize, length: BlockSize, fill: 0x22},
			},
			wantFill: map[int64]byte{4: 0x22},
		},
		{
			name:     "unaligned offset read",
			ops:      []op{{write: false, off: 1, length: BlockSize, wantErr: ErrUnaligned}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "unaligned length read",
			ops:      []op{{write: false, off: 0, length: BlockSize - 1, wantErr: ErrUnaligned}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "unaligned offset write",
			ops:      []op{{write: true, off: BlockSize + 3, length: BlockSize, fill: 0xFF, wantErr: ErrUnaligned}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "unaligned length write",
			ops:      []op{{write: true, off: 0, length: 1, fill: 0xFF, wantErr: ErrUnaligned}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "read past end",
			ops:      []op{{write: false, off: int64(blocks) * BlockSize, length: BlockSize, wantErr: ErrOutOfRange}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "read spanning end",
			ops:      []op{{write: false, off: int64(blocks-1) * BlockSize, length: 2 * BlockSize, wantErr: ErrOutOfRange}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "negative offset",
			ops:      []op{{write: false, off: -BlockSize, length: BlockSize, wantErr: ErrOutOfRange}},
			wantFill: map[int64]byte{},
		},
		{
			name:     "write past end",
			ops:      []op{{write: true, off: int64(blocks) * BlockSize, length: BlockSize, fill: 0xFF, wantErr: ErrOutOfRange}},
			wantFill: map[int64]byte{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := devBase(blocks)
			baseCopy := bytes.Clone(base)
			dev := New(base)

			for i, o := range tc.ops {
				var n int
				var err error
				if o.write {
					p := bytes.Repeat([]byte{o.fill}, o.length)
					n, err = dev.WriteAt(p, o.off)
				} else {
					p := make([]byte, o.length)
					n, err = dev.ReadAt(p, o.off)
				}
				if !errors.Is(err, o.wantErr) {
					t.Fatalf("op %d: err = %v, want %v", i, err, o.wantErr)
				}
				if o.wantErr == nil && n != o.length {
					t.Fatalf("op %d: n = %d, want %d", i, n, o.length)
				}
			}

			// Verify every block: dirty ones carry the fill, others are base.
			for idx := int64(0); idx < blocks; idx++ {
				got := make([]byte, BlockSize)
				if _, err := dev.ReadAt(got, idx*BlockSize); err != nil {
					t.Fatalf("verify ReadAt(%d): %v", idx, err)
				}
				var want []byte
				if fill, ok := tc.wantFill[idx]; ok {
					want = bytes.Repeat([]byte{fill}, BlockSize)
				} else {
					want = baseCopy[idx*BlockSize : (idx+1)*BlockSize]
				}
				if !bytes.Equal(got, want) {
					t.Errorf("block %d content mismatch", idx)
				}
			}

			// Base immutability: the slice handed to New is untouched.
			if !bytes.Equal(base, baseCopy) {
				t.Error("base bytes were modified by device operations")
			}
		})
	}
}

func TestWriteAtDoesNotAliasCallerSlice(t *testing.T) {
	base := devBase(4)
	dev := New(base)

	p := bytes.Repeat([]byte{0x77}, 2*BlockSize)
	if _, err := dev.WriteAt(p, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Mutate the caller's buffer after the write.
	for i := range p {
		p[i] = 0x00
	}
	got := make([]byte, 2*BlockSize)
	if _, err := dev.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	want := bytes.Repeat([]byte{0x77}, 2*BlockSize)
	if !bytes.Equal(got, want) {
		t.Error("device content changed when caller mutated its buffer: WriteAt aliases the caller slice")
	}
}

func TestNewPanicsOnUnalignedBase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New with unaligned base should panic")
		}
	}()
	New(make([]byte, BlockSize+1))
}

func TestZeroLengthIO(t *testing.T) {
	dev := New(devBase(2))
	if n, err := dev.ReadAt(nil, 0); err != nil || n != 0 {
		t.Errorf("zero-length ReadAt = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := dev.WriteAt(nil, 2*BlockSize); err != nil || n != 0 {
		t.Errorf("zero-length WriteAt at end = (%d, %v), want (0, nil)", n, err)
	}
}
