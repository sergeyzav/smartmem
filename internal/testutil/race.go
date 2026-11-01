//go:build race

package testutil

// RaceEnabled reports whether the race detector is on. Allocation tests skip
// themselves under -race, because the detector allocates on its own.
const RaceEnabled = true
