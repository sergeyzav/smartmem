// Package smartmem provides typed, batched access to process memory on top of
// MemProcFS.
//
// Requests are described lazily and executed in rounds: every round is one
// scatter operation per process, so independent requests from different
// subsystems share a round, and dependent requests (pointer chains) move to
// the next one.
//
// The package is under construction; see docs/DESIGN.md for the v1 design.
package smartmem
