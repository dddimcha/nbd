package main

import (
	"bytes"
	"flag"
	"io"

	"github.com/dddimcha/nbd/blockdevice"
)

// cmdDiff compares base and modified images and writes the delta blob.
func cmdDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "output delta path (required)")
	tier := fs.Int("tier", 0, "serialization tier: 0 (bare), 1 (CRC), 2 (Reed-Solomon)")
	pos, code := parseArgs(fs, args, 2, "usage: bdev diff <base> <modified> -o <delta> [-tier 0|1|2]", stdout, stderr)
	if pos == nil {
		return code
	}
	if *out == "" {
		return fail(stderr, 2, "diff needs <base> <modified> and -o <delta>")
	}
	if *tier < 0 || *tier > 2 {
		return fail(stderr, 2, "-tier must be 0, 1 or 2")
	}
	base, err := loadImage(pos[0])
	if err != nil {
		return fail(stderr, 1, "%v", err)
	}
	mod, err := loadImage(pos[1])
	if err != nil {
		return fail(stderr, 1, "%v", err)
	}
	if len(base) != len(mod) {
		return fail(stderr, 1, "base and modified must be equal length and %d-aligned", blockdevice.BlockSize)
	}
	dev := blockdevice.New(base)
	bs := blockdevice.BlockSize
	for off := 0; off < len(base); off += bs {
		if !bytes.Equal(base[off:off+bs], mod[off:off+bs]) {
			if _, err := dev.WriteAt(mod[off:off+bs], int64(off)); err != nil {
				return fail(stderr, 1, "%v", err)
			}
		}
	}
	blob, err := dev.SerializeTier(blockdevice.Tier(*tier))
	if err != nil {
		return fail(stderr, 1, "serialize: %v", err)
	}
	if err := writeFile(*out, blob); err != nil {
		return fail(stderr, 1, "%v", err)
	}
	return 0
}
