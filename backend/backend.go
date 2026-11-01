// Package backend defines the interface smartmem uses to reach memory.
//
// Most programs do not implement it: package
// [github.com/sergeyzav/smartmem/backend/memprocfs] connects to MemProcFS,
// and package [github.com/sergeyzav/smartmem/backend/fake] is an in-memory
// process for tests. Implement [Backend] to read memory from another source,
// such as a raw dump or a debugger.
//
// # Contract
//
// A [Backend] executes one scatter operation for one process: all writes
// first, then all reads, the same order as VMMDLL_Scatter_Execute in
// MemProcFS. smartmem guarantees that:
//
//   - requests merged into one read range lie within one 4 KiB page; a
//     request that itself crosses a page boundary gets a range of its own,
//     which may overlap other ranges;
//   - Dst and Src are valid only during Exec, so the backend must not keep
//     them;
//   - Exec may be called from several goroutines at once, by independent
//     executions, so it must be safe for concurrent use.
//
// The backend reports results per range. For a read, it sets N to the bytes
// read into Dst, or sets Err if the range failed; smartmem turns a short read
// into a PartialReadError and Err into a ReadError, for the requests in that
// range only. Exec itself returns an error only if the whole operation could
// not run; that fails the whole execution.
//
// The optional interfaces [ProcessInfo] and [RoundAware] add process and
// module lookups and round notifications.
package backend

import "context"

// ReadOp is one read range in a scatter operation.
type ReadOp struct {
	Addr uint64 // start of the range
	Dst  []byte // the backend writes the data here; len(Dst) is the range size
	N    int    // out: bytes read from the start of the range
	Err  error  // out: error for this range only
}

// WriteOp is one write range in a scatter operation.
type WriteOp struct {
	Addr uint64 // start of the range
	Src  []byte // the bytes to write
	Err  error  // out: error for this range only
}

// Backend executes scatter operations. See the package documentation for
// the contract.
type Backend interface {
	// Exec runs the writes, then the reads, in process pid. flags are the
	// MemProcFS read flags set by smartmem.WithFlags; other backends may
	// ignore them. Exec returns an error only when the whole operation failed
	// (ctx done, closed handle, broken session). Errors of single ranges go
	// to ReadOp.Err and WriteOp.Err.
	Exec(ctx context.Context, pid uint32, flags uint64, writes []WriteOp, reads []ReadOp) error
}

// ProcInfo describes a target process.
type ProcInfo struct {
	PID   uint32 // process id
	X64   bool   // 64-bit memory model
	Wow64 bool   // 32-bit process on a 64-bit system
	PEB   uint64 // address of the PEB, 0 if unknown
}

// ProcessInfo is an optional Backend interface for process and module lookups.
// Without it, smartmem treats every process as native x64 and cannot resolve
// modules or process names.
type ProcessInfo interface {
	// Info describes the process with this pid.
	Info(pid uint32) (ProcInfo, error)
	// PidByName finds a process by its executable name, such as "app.exe".
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
