package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dddimcha/nbd/blockdevice"
)

// cmdApply materializes base + delta into a full output image. On partial
// recovery the image is still written and the exit code is 3.
func cmdApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "output image path (required)")
	pos, code := parseArgs(fs, args, 2, "usage: bdev apply <base> <delta> -o <out>", stdout, stderr)
	if pos == nil {
		return code
	}
	if *out == "" {
		return fail(stderr, 2, "apply needs <base> <delta> and -o <out>")
	}
	base, err := loadImage(pos[0])
	if err != nil {
		return fail(stderr, 1, "%v", err)
	}
	blob, err := loadBlob(pos[1])
	if err != nil {
		return fail(stderr, 1, "%v", err)
	}
	dev, err := blockdevice.Deserialize(blob, base)
	var partial *blockdevice.PartialRecoveryError
	if err != nil && !errors.As(err, &partial) {
		return fail(stderr, 1, "deserialize: %v", err)
	}
	img := make([]byte, len(base))
	if _, err := dev.ReadAt(img, 0); err != nil {
		return fail(stderr, 1, "read: %v", err)
	}
	if err := writeFile(*out, img); err != nil {
		return fail(stderr, 1, "%v", err)
	}
	if partial != nil {
		fmt.Fprintf(stderr, "bdev: partial recovery — %d block(s) degraded to base: %v (unattributed loss: %v)\n",
			len(partial.BadBlocks), partial.BadBlocks, partial.Truncated)
		return 3
	}
	return 0
}
