package main

import (
	"fmt"
	"io"

	"github.com/dddimcha/nbd/blockdevice"
)

var tierNames = map[blockdevice.Tier]string{
	blockdevice.TierL0: "L0", blockdevice.TierL1: "L1", blockdevice.TierL2: "L2",
}

// tierName names a tier byte, tolerating unverified out-of-range values
// (headerless-recovered blobs report the raw byte).
func tierName(t blockdevice.Tier) string {
	if n, ok := tierNames[t]; ok {
		return n
	}
	return fmt.Sprintf("unknown(%d)", uint8(t))
}

// cmdInspect prints a delta blob's header and loss report.
func cmdInspect(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprintln(stdout, "usage: bdev inspect <delta>")
			return 0
		}
	}
	if len(args) != 1 {
		return fail(stderr, 2, "inspect needs exactly one <delta>")
	}
	blob, err := loadBlob(args[0])
	if err != nil {
		return fail(stderr, 1, "%v", err)
	}
	info, err := blockdevice.Inspect(blob)
	if err != nil {
		return fail(stderr, 1, "inspect: %v", err)
	}
	payload := int(info.BlockCount) * blockdevice.BlockSize
	fmt.Fprintf(stdout, "version: %d\n", info.Version)
	fmt.Fprintf(stdout, "tier: %s\n", tierName(info.Tier))
	fmt.Fprintf(stdout, "blocks: %d\n", info.BlockCount)
	fmt.Fprintf(stdout, "blob-size: %d\n", info.BlobSize)
	fmt.Fprintf(stdout, "payload-overhead: %d\n", info.BlobSize-payload)
	if len(info.BadBlocks) == 0 {
		fmt.Fprintln(stdout, "bad-blocks: none")
	} else {
		fmt.Fprintf(stdout, "bad-blocks: %v\n", info.BadBlocks)
	}
	if info.UnattributedLoss > 0 {
		fmt.Fprintf(stdout, "unattributed-loss: %d\n", info.UnattributedLoss)
	}
	if info.Truncated {
		fmt.Fprintln(stdout, "truncated: true")
	}
	if len(info.BadBlocks) > 0 || info.Truncated {
		return 3
	}
	return 0
}
