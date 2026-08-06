package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dddimcha/nbd/blockdevice"
)

func TestDiffProducesDelta(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := bytes.Repeat([]byte{0}, 4*bs)
	mod := append([]byte(nil), base...)
	copy(mod[2*bs:], bytes.Repeat([]byte{0xCD}, bs))
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	deltaPath := filepath.Join(dir, "d.delta")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)

	var out, errb bytes.Buffer
	code := cmdDiff([]string{basePath, modPath, "-o", deltaPath, "-tier", "1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	blob, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := blockdevice.Inspect(blob)
	if err != nil || info.Tier != blockdevice.TierL1 || info.BlockCount != 1 {
		t.Fatalf("info %+v err %v", info, err)
	}
}

func TestDiffUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdDiff([]string{"only-one-arg"}, &out, &errb); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errb.String(), "bdev: ") {
		t.Fatalf("stderr not prefixed: %q", errb.String())
	}
}

// Surplus positionals must be rejected (exit 2), not silently ignored.
func TestSurplusPositionalsRejected(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.img")
	os.WriteFile(img, make([]byte, blockdevice.BlockSize), 0o644)
	for name, run := range map[string]func([]string, *bytes.Buffer, *bytes.Buffer) int{
		"diff":  func(a []string, o, e *bytes.Buffer) int { return cmdDiff(a, o, e) },
		"apply": func(a []string, o, e *bytes.Buffer) int { return cmdApply(a, o, e) },
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run([]string{img, img, "extra", "-o", filepath.Join(dir, "x")}, &out, &errb); code != 2 {
				t.Fatalf("%s with surplus positional: exit %d, want 2 (stderr: %s)", name, code, errb.String())
			}
		})
	}
	var out, errb bytes.Buffer
	if code := cmdInspect([]string{img, "extra"}, &out, &errb); code != 2 {
		t.Fatalf("inspect with surplus positional: exit %d, want 2", code)
	}
}

// -h prints a synopsis to stdout and exits 0 for every subcommand.
func TestHelpExitsZero(t *testing.T) {
	cmds := map[string]func([]string, io.Writer, io.Writer) int{
		"diff": cmdDiff, "apply": cmdApply, "inspect": cmdInspect,
	}
	for name, cmd := range cmds {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := cmd([]string{"-h"}, &out, &errb); code != 0 {
				t.Fatalf("%s -h: exit %d, want 0 (stderr: %s)", name, code, errb.String())
			}
			if !strings.Contains(out.String(), "usage: bdev "+name) {
				t.Fatalf("%s -h stdout missing synopsis: %q", name, out.String())
			}
		})
	}
}

// A partial apply must state the unattributed-loss (Truncated) status, not
// only the nameable bad blocks.
func TestApplyPartialMessageHonest(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := bytes.Repeat([]byte{7}, 4*bs)
	mod := append([]byte(nil), base...)
	copy(mod[bs:], bytes.Repeat([]byte{9}, bs))
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	deltaPath := filepath.Join(dir, "d.delta")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)
	var out, errb bytes.Buffer
	if code := cmdDiff([]string{basePath, modPath, "-o", deltaPath, "-tier", "1"}, &out, &errb); code != 0 {
		t.Fatalf("diff: %s", errb.String())
	}
	blob, _ := os.ReadFile(deltaPath)
	blob[len(blob)-10] ^= 0xFF // corrupt record data: CRC fails, unattributed loss
	os.WriteFile(deltaPath, blob, 0o644)
	errb.Reset()
	if code := cmdApply([]string{basePath, deltaPath, "-o", filepath.Join(dir, "out.img")}, &out, &errb); code != 3 {
		t.Fatalf("apply exit %d, want 3 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "unattributed loss: true") {
		t.Fatalf("partial message hides unattributed loss: %q", errb.String())
	}
}

// Inspect prints an unattributed-loss line (and exits 3) when records are
// lost without a readable index.
func TestInspectUnattributedLossOutput(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := make([]byte, 2*bs)
	mod := append([]byte(nil), base...)
	mod[0] = 1
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	deltaPath := filepath.Join(dir, "d.delta")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)
	var out, errb bytes.Buffer
	if code := cmdDiff([]string{basePath, modPath, "-o", deltaPath, "-tier", "1"}, &out, &errb); code != 0 {
		t.Fatalf("diff: %s", errb.String())
	}
	blob, _ := os.ReadFile(deltaPath)
	blob[len(blob)-10] ^= 0xFF // record CRC failure -> unattributed loss
	os.WriteFile(deltaPath, blob, 0o644)
	out.Reset()
	if code := cmdInspect([]string{deltaPath}, &out, &errb); code != 3 {
		t.Fatalf("inspect exit %d, want 3 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"bad-blocks: none", "unattributed-loss: 1", "truncated: true"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
}

func TestApplyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := bytes.Repeat([]byte{7}, 3*bs)
	mod := append([]byte(nil), base...)
	copy(mod[bs:], bytes.Repeat([]byte{9}, bs))
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	deltaPath := filepath.Join(dir, "d.delta")
	outPath := filepath.Join(dir, "out.img")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)

	var out, errb bytes.Buffer
	if code := cmdDiff([]string{basePath, modPath, "-o", deltaPath}, &out, &errb); code != 0 {
		t.Fatalf("diff exit %d: %s", code, errb.String())
	}
	if code := cmdApply([]string{basePath, deltaPath, "-o", outPath}, &out, &errb); code != 0 {
		t.Fatalf("apply exit %d: %s", code, errb.String())
	}
	got, _ := os.ReadFile(outPath)
	if !bytes.Equal(got, mod) {
		t.Fatal("apply(base, diff(base, mod)) != mod")
	}
}

func TestApplyCorruptDelta(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.img")
	os.WriteFile(basePath, make([]byte, blockdevice.BlockSize), 0o644)
	deltaPath := filepath.Join(dir, "bad.delta")
	os.WriteFile(deltaPath, []byte("garbage"), 0o644)
	var out, errb bytes.Buffer
	if code := cmdApply([]string{basePath, deltaPath, "-o", filepath.Join(dir, "x")}, &out, &errb); code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
}

func TestInspectOutput(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := make([]byte, 2*bs)
	mod := append([]byte(nil), base...)
	mod[0] = 1
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	deltaPath := filepath.Join(dir, "d.delta")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)
	var out, errb bytes.Buffer
	if code := cmdDiff([]string{basePath, modPath, "-o", deltaPath, "-tier", "1"}, &out, &errb); code != 0 {
		t.Fatalf("diff: %s", errb.String())
	}
	out.Reset()
	if code := cmdInspect([]string{deltaPath}, &out, &errb); code != 0 {
		t.Fatalf("inspect exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"tier: L1", "blocks: 1", "bad-blocks: none"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
}
