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

// Table-driven coverage for flag/positional ordering and the "--" terminator
// in parseArgs, driven through cmdDiff.
func TestDiffArgOrderingAndDashDash(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := bytes.Repeat([]byte{1}, 2*bs)
	mod := append([]byte(nil), base...)
	mod[0] = 2
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)
	// A modified image literally named "-o", addressable only via "--".
	dashOPath := filepath.Join(dir, "-o")
	os.WriteFile(dashOPath, mod, 0o644)

	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  bool // delta file must exist with a decodable blob
	}{
		{"flags after positionals", []string{basePath, modPath, "-o", filepath.Join(dir, "d1")}, 0, true},
		{"flags before positionals", []string{"-o", filepath.Join(dir, "d2"), basePath, modPath}, 0, true},
		{"dash-dash before positionals", []string{"-o", filepath.Join(dir, "d3"), "--", basePath, modPath}, 0, true},
		{"flag-looking positional after dash-dash", []string{"-o", filepath.Join(dir, "d4"), "--", basePath, dashOPath}, 0, true},
		{"mixed flags around positionals", []string{"-tier", "1", basePath, modPath, "-o", filepath.Join(dir, "d5")}, 0, true},
		{"surplus positional", []string{basePath, modPath, "extra", "-o", filepath.Join(dir, "x1")}, 2, false},
		{"surplus positional after dash-dash", []string{"-o", filepath.Join(dir, "x2"), "--", basePath, modPath, "extra"}, 2, false},
		{"unknown flag token", []string{basePath, modPath, "-o", filepath.Join(dir, "x3"), "-bogus"}, 2, false},
		{"missing positional after dash-dash", []string{"-o", filepath.Join(dir, "x4"), "--", basePath}, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := cmdDiff(tc.args, &out, &errb)
			if code != tc.wantCode {
				t.Fatalf("args %q: exit %d, want %d (stderr: %s)", tc.args, code, tc.wantCode, errb.String())
			}
			if tc.wantOut {
				deltaPath := tc.args[slicesIndex(tc.args, "-o")+1]
				blob, err := os.ReadFile(deltaPath)
				if err != nil {
					t.Fatalf("delta not written: %v", err)
				}
				if _, err := blockdevice.Inspect(blob); err != nil {
					t.Fatalf("delta not decodable: %v", err)
				}
			}
		})
	}
}

func slicesIndex(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// apply also honors "--" (shared parseArgs, but exercised end to end).
func TestApplyDashDash(t *testing.T) {
	dir := t.TempDir()
	bs := blockdevice.BlockSize
	base := bytes.Repeat([]byte{3}, 2*bs)
	mod := append([]byte(nil), base...)
	mod[bs] = 4
	basePath := filepath.Join(dir, "base.img")
	modPath := filepath.Join(dir, "mod.img")
	deltaPath := filepath.Join(dir, "d.delta")
	outPath := filepath.Join(dir, "out.img")
	os.WriteFile(basePath, base, 0o644)
	os.WriteFile(modPath, mod, 0o644)
	var out, errb bytes.Buffer
	if code := cmdDiff([]string{basePath, modPath, "-o", deltaPath}, &out, &errb); code != 0 {
		t.Fatalf("diff: %s", errb.String())
	}
	if code := cmdApply([]string{"-o", outPath, "--", basePath, deltaPath}, &out, &errb); code != 0 {
		t.Fatalf("apply with --: exit %d (stderr: %s)", code, errb.String())
	}
	got, _ := os.ReadFile(outPath)
	if !bytes.Equal(got, mod) {
		t.Fatal("apply with -- produced wrong image")
	}
}

// writeFile must land the exact bytes at the target path (via temp+rename)
// and replace an existing file.
func TestWriteFileHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	os.WriteFile(path, []byte("old"), 0o644)
	want := []byte("new content")
	if err := writeFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content %q, want %q", got, want)
	}
	// No temp litter left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory litter: %v", entries)
	}
}
