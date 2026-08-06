package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dddimcha/nbd/blockdevice"
)

// parseArgs parses a subcommand's flags and positionals, accepting flags
// before or after the positionals ("bdev diff a b -o d"). A "--" token ends
// flag interpretation: everything after it is positional, so a file literally
// named "-o" is expressible. -h/--help prints the synopsis to stdout and
// exits 0; surplus positionals beyond want are rejected (exit 2). On success
// it returns exactly want positionals and code is unused; on any other
// outcome positionals are nil and code is the exit code.
//
// It works in a single pass: args are partitioned into flag tokens (with
// their values, using the FlagSet to tell value-taking flags from booleans)
// and positionals, honoring "--", then the flag tokens are handed to one
// fs.Parse. Unknown flag-looking tokens are passed through so Parse reports
// them as usage errors (exit 2).
func parseArgs(fs *flag.FlagSet, args []string, want int, synopsis string, stdout, stderr io.Writer) ([]string, int) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue // value attached, nothing more to consume
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // unknown (or -h/--help): let fs.Parse report it
		}
		takesValue := true
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			takesValue = false
		}
		if takesValue && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	// Silence the FlagSet's own output: on -h it would print its generated
	// usage (flag table) in addition to our synopsis, and on a bad flag it
	// prints the error itself — both are reported here instead, exactly once.
	fs.SetOutput(io.Discard)
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, synopsis)
			return nil, 0
		}
		return nil, fail(stderr, 2, "%v\n%s", err, synopsis)
	}
	if len(pos) > want {
		return nil, fail(stderr, 2, "unexpected argument(s) %q\n%s", pos[want:], synopsis)
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

// writeFile writes data to path with mode 0644, atomically: the bytes go to
// a temp file in the target's directory, fsynced and then renamed into
// place, so a failed write (ENOSPC, crash) never leaves a truncated file
// clobbering a good previous one. If the target already exists and is (or
// traverses) a symlink, it is resolved first and the write goes through to
// the real file, instead of the rename silently replacing the symlink with
// a regular file; a target that does not resolve (missing, dangling link)
// is written at the literal path.
func writeFile(path string, data []byte) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
