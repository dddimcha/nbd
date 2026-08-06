//go:build race

package blockdevice

// raceEnabled reports whether the race detector is active; timing-sensitive
// assertions relax under its ~10-40x instrumentation overhead.
const raceEnabled = true
