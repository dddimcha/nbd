package main

import (
	"bytes"
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
