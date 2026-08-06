# Project Skeleton (cmd/bdev CLI + Architecture Doc) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish the canonical Go project layout: a small `cmd/bdev` CLI that exercises the `blockdevice` library end-to-end (diff / apply / inspect), plus `docs/ARCHITECTURE.md` documenting the layered structure.

**Architecture:** `blockdevice/` remains the single public library package (decision already made — do not re-litigate). The CLI is one `main` package under `cmd/bdev/`, split into one file per subcommand plus shared helpers, all in package `main`. **No `internal/` directory**: the only non-library code is the CLI itself, there is nothing to hide from external importers that isn't already unexported inside `blockdevice`, and a second package would add an import boundary with zero consumers. CLI-only helpers (file loading, exit-code mapping, usage text) live in package `main` in `cmd/bdev/`. One small library addition is required: an exported `Inspect(blob)` so the CLI never re-parses the wire format in `main`.

**Tech Stack:** Go 1.25, stdlib `flag` only (no cobra), existing `github.com/klauspost/reedsolomon` (transitive via the library — the CLI never imports it directly).

## Global Constraints

- `blockdevice/` is the ONLY public library package; no `src/`, no `core/`, no `internal/`.
- CLI uses stdlib `flag` package only — no cobra, no third-party CLI deps.
- Block size is `blockdevice.BlockSize` (4096); never hardcode 4096 in the CLI.
- All errors to stderr prefixed `bdev: `; nothing but the requested output on stdout.
- Exit codes (fixed contract): `0` success · `1` runtime/data error (I/O, corrupt blob, mismatched sizes) · `2` usage error (bad flags/args; also what `flag` produces) · `3` partial recovery (operation completed but some blocks were unrecoverable and degraded to base — the tool still wrote its output).
- Gates before every commit: `make verify` (fmt, vet, race tests).
- Deterministic output: identical inputs must produce byte-identical delta files (guaranteed by `Serialize`'s sorted records — do not iterate maps into output).

## File Structure

```
cmd/bdev/
  main.go        # dispatch: subcommand table, usage, exit-code mapping
  diff.go        # cmdDiff — compare base vs modified, emit delta
  apply.go       # cmdApply — Deserialize + materialize full image
  inspect.go     # cmdInspect — header/tier/block report from a delta
  io.go          # loadImage/loadBlob/writeFile helpers (package main)
  main_test.go   # end-to-end round-trip tests driving the cmd* funcs
blockdevice/
  inspect.go       # NEW public API: Inspect(blob) (Info, error)
  inspect_test.go
docs/
  ARCHITECTURE.md  # layered view + data flow + extension points
docs/plans/
  project-skeleton.md  # this plan
```

Each `cmd*` function has the signature `func(args []string, stdout, stderr io.Writer) int` (returns the exit code) so tests drive it without spawning a process; `main()` is a two-line dispatcher over that table.

### CLI contract (exact)

```
bdev diff <base> <modified> -o <delta> [-tier 0|1|2]
bdev apply <base> <delta> -o <out>
bdev inspect <delta>
```

- `diff`: both images must be equal length and a multiple of BlockSize, else exit 1 with `bdev: base and modified must be equal length and 4096-aligned`. `-o` is required (exit 2 if missing). `-tier` defaults to `0`. Blocks that differ are written into a fresh `Device` and serialized with `SerializeTier`.
- `apply`: `-o` required. `Deserialize(delta, base)`; on `*PartialRecoveryError` still write the materialized image, print the bad-block report to stderr, exit 3. On `ErrCorrupt` exit 1, write nothing.
- `inspect`: prints to stdout, one `key: value` per line: `version`, `tier` (`L0`/`L1`/`L2`), `blocks` (count), `blob-size`, `payload-overhead` (bytes beyond blocks×4096), and for L1/L2 a `bad-blocks:` line listing CRC-failed indices (or `none`). Exit 3 if any bad blocks, else 0. Headerless/corrupt blob → exit 1.

---

### Task 1: Library `Inspect` API

The CLI must never re-parse the wire format in package `main` (format knowledge stays inside `blockdevice`). Add a read-only inspector that decodes the header and, for L1/L2, verifies record CRCs without needing the base image.

**Files:**
- Create: `blockdevice/inspect.go`
- Test: `blockdevice/inspect_test.go`

**Interfaces:**
- Consumes: existing unexported `headerSize`, `magic`, `formatVersion`, tier record parsers in `format.go`/`rs.go`.
- Produces (Task 4 relies on these exact names):

```go
// Info describes a serialized delta without decoding it against a base.
type Info struct {
    Version    uint8
    Tier       Tier
    BlockCount uint64
    BlobSize   int
    // BadBlocks lists record indices whose CRC failed (L1) or that could
    // not be reconstructed (L2). Always nil for L0 (no per-record CRC).
    BadBlocks  []int64
}

func Inspect(blob []byte) (Info, error) // ErrCorrupt on bad magic/header
```

- [ ] **Step 1: Write the failing test**

```go
func TestInspectRoundTrip(t *testing.T) {
    base := make([]byte, 4*BlockSize)
    dev := New(base)
    blk := bytes.Repeat([]byte{0xAB}, BlockSize)
    if _, err := dev.WriteAt(blk, 2*BlockSize); err != nil {
        t.Fatal(err)
    }
    for _, tier := range []Tier{TierL0, TierL1, TierL2} {
        blob, err := dev.SerializeTier(tier)
        if err != nil {
            t.Fatalf("tier %d: %v", tier, err)
        }
        info, err := Inspect(blob)
        if err != nil {
            t.Fatalf("tier %d: %v", tier, err)
        }
        if info.Tier != tier || info.BlockCount != 1 ||
            info.BlobSize != len(blob) || len(info.BadBlocks) != 0 {
            t.Fatalf("tier %d: bad info %+v", tier, info)
        }
    }
}

func TestInspectCorrupt(t *testing.T) {
    if _, err := Inspect([]byte("not a delta")); !errors.Is(err, ErrCorrupt) {
        t.Fatalf("want ErrCorrupt, got %v", err)
    }
}

func TestInspectL1BadBlock(t *testing.T) {
    base := make([]byte, 2*BlockSize)
    dev := New(base)
    dev.WriteAt(bytes.Repeat([]byte{1}, BlockSize), 0)
    blob, _ := dev.SerializeTier(TierL1)
    blob[headerSize+8+100] ^= 0xFF // flip a data byte inside the record
    info, err := Inspect(blob)
    if err != nil {
        t.Fatal(err)
    }
    if len(info.BadBlocks) != 1 || info.BadBlocks[0] != 0 {
        t.Fatalf("want BadBlocks [0], got %v", info.BadBlocks)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./blockdevice -run TestInspect -v`
Expected: FAIL — `undefined: Inspect` / `undefined: Info`.

- [ ] **Step 3: Implement `blockdevice/inspect.go`**

Decode the header exactly as `Deserialize` does (magic, version, tier, blockCount, headerCRC — reuse the existing header-parsing helper if one is factored out; otherwise mirror the ~15 lines and keep both in `format.go`'s vocabulary). Then:
- L0: no per-record CRC — `BadBlocks` stays nil; validate the body length is a whole number of `l0RecordSize` records (else `ErrCorrupt`).
- L1: walk `l1RecordSize` records, recompute `crc32(index|data)`, collect failing indices.
- L2: parse rsMeta and shard headers, CRC-check shards; if reconstruction would fail (more than K bad shards), decode what is salvageable exactly like `rsSalvage` and report unrecoverable indices; if all shards check out, `BadBlocks` is nil. Reuse the existing unexported shard-parsing helpers in `rs.go` rather than duplicating them — extract a shared helper if the current code is inlined inside `deserializeRS`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./blockdevice -run TestInspect -v` then `make verify`
Expected: PASS, gates green.

- [ ] **Step 5: Commit**

```bash
git add blockdevice/inspect.go blockdevice/inspect_test.go
git commit -m "feat(blockdevice): Inspect — decode delta metadata without a base"
```

---

### Task 2: CLI scaffold + `diff` subcommand

**Files:**
- Create: `cmd/bdev/main.go`
- Create: `cmd/bdev/io.go`
- Create: `cmd/bdev/diff.go`
- Test: `cmd/bdev/main_test.go`

**Interfaces:**
- Consumes: `blockdevice.New`, `(*Device).WriteAt`, `(*Device).SerializeTier`, `blockdevice.BlockSize`, `blockdevice.Tier`.
- Produces (Tasks 3–4 rely on): `func cmdDiff(args []string, stdout, stderr io.Writer) int`, helpers `loadImage(path string) ([]byte, error)` (reads file, errors unless `len%BlockSize==0`), `loadBlob(path string) ([]byte, error)` (plain read), `writeFile(path string, data []byte) error` (0644), and `fail(stderr io.Writer, code int, format string, a ...any) int` which prints `bdev: `-prefixed message and returns `code`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bdev -v`
Expected: FAIL — `undefined: cmdDiff`.

- [ ] **Step 3: Implement**

`main.go`:

```go
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
```

(Until Tasks 3–4 land, add temporary stubs `func cmdApply(...) int { return 1 }` / `cmdInspect` in `main.go` so the package compiles; each later task replaces its stub with the real file.)

`io.go`:

```go
package main

import (
    "fmt"
    "io"
    "os"

    "github.com/dddimcha/nbd/blockdevice"
)

func fail(stderr io.Writer, code int, format string, a ...any) int {
    fmt.Fprintf(stderr, "bdev: "+format+"\n", a...)
    return code
}

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

func loadBlob(path string) ([]byte, error) { return os.ReadFile(path) }

func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
```

`diff.go`:

```go
package main

import (
    "bytes"
    "flag"
    "io"

    "github.com/dddimcha/nbd/blockdevice"
)

func cmdDiff(args []string, stdout, stderr io.Writer) int {
    fs := flag.NewFlagSet("diff", flag.ContinueOnError)
    fs.SetOutput(stderr)
    out := fs.String("o", "", "output delta path (required)")
    tier := fs.Int("tier", 0, "serialization tier: 0 (bare), 1 (CRC), 2 (Reed-Solomon)")
    // Accept flags after positionals: "bdev diff a b -o d" — parse twice.
    if err := fs.Parse(args); err != nil {
        return 2
    }
    pos := fs.Args()
    if len(pos) >= 2 {
        if err := fs.Parse(pos[2:]); err != nil {
            return 2
        }
        pos = pos[:2]
    }
    if len(pos) != 2 || *out == "" {
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bdev -v` then `make verify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bdev/main.go cmd/bdev/io.go cmd/bdev/diff.go cmd/bdev/main_test.go
git commit -m "feat(cmd/bdev): CLI scaffold + diff subcommand"
```

---

### Task 3: `apply` subcommand

**Files:**
- Create: `cmd/bdev/apply.go` (replace the stub in `main.go`)
- Modify: `cmd/bdev/main_test.go` (add tests)

**Interfaces:**
- Consumes: `blockdevice.Deserialize`, `(*Device).ReadAt`, `*blockdevice.PartialRecoveryError`, helpers from Task 2 (`loadImage`, `loadBlob`, `writeFile`, `fail`).
- Produces: `func cmdApply(args []string, stdout, stderr io.Writer) int`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bdev -run TestApply -v`
Expected: FAIL (stub returns 1 for the round-trip case).

- [ ] **Step 3: Implement `apply.go`** (and delete the `cmdApply` stub from `main.go`)

```go
package main

import (
    "errors"
    "flag"
    "fmt"
    "io"

    "github.com/dddimcha/nbd/blockdevice"
)

func cmdApply(args []string, stdout, stderr io.Writer) int {
    fs := flag.NewFlagSet("apply", flag.ContinueOnError)
    fs.SetOutput(stderr)
    out := fs.String("o", "", "output image path (required)")
    if err := fs.Parse(args); err != nil {
        return 2
    }
    pos := fs.Args()
    if len(pos) >= 2 {
        if err := fs.Parse(pos[2:]); err != nil {
            return 2
        }
        pos = pos[:2]
    }
    if len(pos) != 2 || *out == "" {
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
        fmt.Fprintf(stderr, "bdev: partial recovery — %d block(s) degraded to base: %v\n",
            len(partial.BadBlocks), partial.BadBlocks)
        return 3
    }
    return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bdev -v` then `make verify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bdev/apply.go cmd/bdev/main.go cmd/bdev/main_test.go
git commit -m "feat(cmd/bdev): apply subcommand — materialize base + delta"
```

---

### Task 4: `inspect` subcommand

**Files:**
- Create: `cmd/bdev/inspect.go` (replace the stub in `main.go`)
- Modify: `cmd/bdev/main_test.go` (add tests)

**Interfaces:**
- Consumes: `blockdevice.Inspect` / `blockdevice.Info` from Task 1; helpers from Task 2.
- Produces: `func cmdInspect(args []string, stdout, stderr io.Writer) int`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/bdev -run TestInspectOutput -v`
Expected: FAIL (stub).

- [ ] **Step 3: Implement `inspect.go`** (delete the `cmdInspect` stub)

```go
package main

import (
    "fmt"
    "io"

    "github.com/dddimcha/nbd/blockdevice"
)

var tierNames = map[blockdevice.Tier]string{
    blockdevice.TierL0: "L0", blockdevice.TierL1: "L1", blockdevice.TierL2: "L2",
}

func cmdInspect(args []string, stdout, stderr io.Writer) int {
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
    fmt.Fprintf(stdout, "tier: %s\n", tierNames[info.Tier])
    fmt.Fprintf(stdout, "blocks: %d\n", info.BlockCount)
    fmt.Fprintf(stdout, "blob-size: %d\n", info.BlobSize)
    fmt.Fprintf(stdout, "payload-overhead: %d\n", info.BlobSize-payload)
    if len(info.BadBlocks) == 0 {
        fmt.Fprintln(stdout, "bad-blocks: none")
        return 0
    }
    fmt.Fprintf(stdout, "bad-blocks: %v\n", info.BadBlocks)
    return 3
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/bdev -v && go test ./blockdevice` then `make verify`
Expected: PASS; also build the binary once: `go build ./cmd/bdev` and run the three subcommands by hand on temp files as a smoke check.

- [ ] **Step 5: Commit**

```bash
git add cmd/bdev/inspect.go cmd/bdev/main.go cmd/bdev/main_test.go
git commit -m "feat(cmd/bdev): inspect subcommand — delta header and bad-block report"
```

---

### Task 5: `docs/ARCHITECTURE.md`

**Files:**
- Create: `docs/ARCHITECTURE.md`
- Modify: `README.md` (Layout section: add `cmd/bdev/` line and a link to ARCHITECTURE.md)

**Interfaces:** documentation only; must match the names shipped in Tasks 1–4 exactly.

- [ ] **Step 1: Write `docs/ARCHITECTURE.md`** with this exact structure and content:

```markdown
# Architecture

## Layers

    cmd/bdev  (package main — CLI, flag parsing, exit codes, file I/O)
        │  imports only the public API
        ▼
    blockdevice  (single public library package)
        │  blockdevice.go — Device: base + dirty overlay, ReadAt/WriteAt
        │  inspect.go     — Inspect: header/CRC report without a base
        │  format.go      — wire format: header, L0/L1 records, Serialize/Deserialize
        │  rs.go          — L2 tier: Reed-Solomon sharding over the L1 payload
        ▼
    github.com/klauspost/reedsolomon  (only external dependency, reached
    only through rs.go behind the tier switch)

The CLI never touches format internals: everything it prints comes from the
exported API (`Inspect`, `Deserialize`, `SerializeTier`, typed errors).

## Data flow: pause / resume

Pause:  live image ──WriteAt──▶ Device{base, dirty} ──SerializeTier──▶ delta blob
Resume: delta blob + shared base ──Deserialize──▶ Device ──ReadAt──▶ live image

`bdev diff` reproduces the pause path from two on-disk images (block compare →
WriteAt of changed blocks → serialize); `bdev apply` is the resume path
materialized to a file; `bdev inspect` is the read-only diagnostic on the blob.

## Why a single library package

(one paragraph, imported from DESIGN.md's reasoning:) The library is one
cohesive concern — a copy-on-write overlay and its wire format — whose parts
share unexported vocabulary (record layouts, tier constants, shard headers).
Splitting format.go/rs.go into sub-packages would force exporting those
internals or duplicating them; keeping one package keeps the format private,
the API surface minimal (Device, Tier, Inspect, two errors), and the only
external dependency quarantined in rs.go. The CLI needs no `internal/`
package: it is a single main package with no importable surface to protect.

## Extension points

- **cmd/nbd-server** — a future NBD network server is another `cmd/` main
  package over the same public API; nothing in the library changes.
- **New tiers** — add a `TierL3` constant, a case in `SerializeTier` /
  `Deserialize`, and a new file beside rs.go; records stay self-describing so
  old decoders reject unknown tiers cleanly via `ErrUnsupportedTier`.
- **Streaming I/O** — `Serialize`'s sorted-record layout permits a future
  `io.Reader`/`io.Writer` pair without format changes.
```

- [ ] **Step 2: Update README.md Layout block** — add `cmd/bdev/          bdev CLI: diff / apply / inspect` and link `docs/ARCHITECTURE.md` next to the DESIGN.md link.

- [ ] **Step 3: Verify docs match code**

Run: `go doc ./blockdevice Inspect` and `go build ./cmd/bdev` — every symbol named in ARCHITECTURE.md must exist. Run `make verify`.

- [ ] **Step 4: Commit**

```bash
git add docs/ARCHITECTURE.md README.md
git commit -m "docs: ARCHITECTURE.md — layers, pause/resume data flow, extension points"
```

---

## Verification (whole plan)

1. `make verify` and `make test` green (fmt, vet, race, fuzz smoke).
2. End-to-end by hand: create a 1 MiB base, flip 3 blocks, `bdev diff -tier 2`, corrupt one byte of the delta, `bdev apply` → exit 0 (RS reconstructs) and output equals modified; corrupt beyond parity → exit 3 with bad-block report and degraded blocks equal base.
3. Determinism: run `diff` twice on the same inputs — output files byte-identical (`cmp`).
4. `go vet ./...` and `go build ./...` cover both packages; CI needs no change (workflows already run `./...`).

## Self-review notes

- Spec coverage: diff/apply/inspect subcommands, exact flags and exit codes (Global Constraints + CLI contract), no-internal decision (Architecture paragraph + ARCHITECTURE.md), helpers in package main (io.go), ARCHITECTURE.md with layers/data-flow/single-package rationale/extension points — all mapped to Tasks 1–5.
- Type consistency: `cmd*` signature `(args []string, stdout, stderr io.Writer) int` used uniformly; `Inspect`/`Info` names match between Task 1 and Task 4.
- The only library change is additive (`Inspect`); no existing exported API is touched.
