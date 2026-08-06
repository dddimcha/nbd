package main

import (
	"fmt"
	"io"
	"os"

	"github.com/dddimcha/nbd/blockdevice"
)

// fail prints a "bdev: "-prefixed message to stderr and returns code.
func fail(stderr io.Writer, code int, format string, a ...any) int {
	fmt.Fprintf(stderr, "bdev: "+format+"\n", a...)
	return code
}

// loadImage reads a device image; its length must be block-aligned.
func loadImage(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b)%blockdevice.BlockSize != 0 {
		return nil, fmt.Errorf("%s: length %d not a multiple of %d", path, len(b), blockdevice.BlockSize)
	}
	return b, nil
}

// loadBlob reads a delta blob without alignment constraints.
func loadBlob(path string) ([]byte, error) { return os.ReadFile(path) }

// writeFile writes data to path with mode 0644.
func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
