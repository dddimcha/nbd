package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dddimcha/nbd/blockdevice"
)

// parseArgs parses a subcommand's flags and positionals, accepting flags
// after the positionals ("bdev diff a b -o d"). -h/--help prints the synopsis
// to stdout and exits 0; surplus positionals beyond want are rejected (exit
// 2). On success it returns exactly want positionals and code is unused; on
// any other outcome positionals are nil and code is the exit code.
func parseArgs(fs *flag.FlagSet, args []string, want int, synopsis string, stdout, stderr io.Writer) ([]string, int) {
	help := func(err error) bool { return errors.Is(err, flag.ErrHelp) }
	if err := fs.Parse(args); err != nil {
		if help(err) {
			fmt.Fprintln(stdout, synopsis)
			return nil, 0
		}
		return nil, 2
	}
	pos := fs.Args()
	if len(pos) > want {
		// Re-parse the remainder so trailing flags still count as flags.
		if err := fs.Parse(pos[want:]); err != nil {
			if help(err) {
				fmt.Fprintln(stdout, synopsis)
				return nil, 0
			}
			return nil, 2
		}
		if extra := fs.Args(); len(extra) != 0 {
			return nil, fail(stderr, 2, "unexpected argument(s) %q\n%s", extra, synopsis)
		}
		pos = pos[:want]
	}
	if len(pos) != want {
		return nil, fail(stderr, 2, "%s", synopsis)
	}
	return pos, 0
}

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
