// Command bdev exercises the blockdevice library end-to-end: diff two images
// into a delta, apply a delta onto a base, and inspect a delta's metadata.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `usage:
  bdev diff <base> <modified> -o <delta> [-tier 0|1|2]
  bdev apply <base> <delta> -o <out>
  bdev inspect <delta>

exit codes: 0 ok · 1 error · 2 usage · 3 partial recovery
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) int{
		"diff": cmdDiff, "apply": cmdApply, "inspect": cmdInspect,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "bdev: unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	os.Exit(cmd(os.Args[2:], os.Stdout, os.Stderr))
}
