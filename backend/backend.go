// Package backend defines the interface smartmem uses to reach memory.
//
// A Backend executes one scatter operation for one process: all writes first,
// then all reads, the same order as VMMDLL_Scatter_Execute in MemProcFS.
package backend

import "context"

// ReadOp is one read range in a scatter operation.
type ReadOp struct {
	Addr uint64
	Dst  []byte // the backend writes the data here; len(Dst) is the range size
	N    int    // out: bytes actually read
	Err  error  // out: error for this range only
}

// WriteOp is one write range in a scatter operation.
type WriteOp struct {
	Addr uint64
	Src  []byte
	Err  error // out: error for this range only
}

// Backend executes scatter operations.
type Backend interface {
	// Exec runs the writes, then the reads, for one (pid, flags) pair.
	// It returns an error only when the whole operation failed (closed handle,
	// broken session). Errors of single ranges go to ReadOp.Err and WriteOp.Err.
	Exec(ctx context.Context, pid uint32, flags uint64, writes []WriteOp, reads []ReadOp) error
}

// ProcInfo describes a target process.
type ProcInfo struct {
	PID   uint32
	X64   bool // 64-bit memory model
	Wow64 bool // 32-bit process on a 64-bit system
}

// ProcessInfo is an optional Backend interface for process and module lookups.
// Without it, smartmem treats every process as native x64 and cannot resolve
// modules or process names.
type ProcessInfo interface {
	Info(pid uint32) (ProcInfo, error)
	PidByName(name string) (uint32, error)
	// ModuleBase returns the base address of a loaded module. Names are
	// case-insensitive, as on Windows.
	ModuleBase(pid uint32, name string) (uint64, error)
}

// RoundAware is an optional Backend interface. smartmem calls BeginRound
// before the first Exec of every round, starting at 1 for each execution.
// Backends use it for tracing and tests; MemProcFS ignores it.
type RoundAware interface {
	BeginRound(round int)
}
