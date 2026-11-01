package smartmem

import (
	"errors"
	"fmt"

	"github.com/sergeyzav/smartmem/internal/codec"
	"github.com/sergeyzav/smartmem/internal/engine"
)

// Sentinel errors. Match them with [errors.Is], since they may be wrapped.
var (
	// ErrUnsupported is returned for targets this version does not support,
	// such as WoW64 processes, and for lookups the backend cannot do. It
	// matches errors.ErrUnsupported too.
	ErrUnsupported = fmt.Errorf("smartmem: %w", errors.ErrUnsupported)
	// ErrNoTerminator means a string has no terminator within its maximum length.
	ErrNoTerminator = codec.ErrNoTerminator
	// ErrNullBuffer means a UNICODE_STRING has a length but a null Buffer.
	ErrNullBuffer = codec.ErrNullBuffer
	// ErrPending is returned by Future.Get before the value is ready.
	ErrPending = errors.New("smartmem: value not ready yet")
	// ErrBatchDone is returned by a second Batch.Run.
	ErrBatchDone = errors.New("smartmem: batch already ran")
	// ErrMaxRounds is the error of requests still pending after the number
	// of rounds set by WithMaxRounds, for example while walking a cyclic
	// list.
	ErrMaxRounds = engine.ErrMaxRounds
)

type (
	// NullPointerError means a Deref read a zero pointer. Its field Step int
	// is the index of that Deref in the location, counting from 0. Later
	// steps are not read.
	NullPointerError = engine.NullPointerError
	// BadPointerError means a Deref read a value that is not a user-mode x64
	// address, that is above 0x00007FFFFFFFFFFF. Its fields are Step int, the
	// index of the Deref, and Value uint64, the value read.
	BadPointerError = engine.BadPointerError
	// PartialReadError means the backend read only part of a request. Its
	// fields are Addr uint64, the address of the request, and Want and Got
	// int, the bytes requested and read.
	PartialReadError = engine.PartialReadError
	// ReadError is a backend error for one read. Its fields are Addr uint64,
	// the address of the request, and Err, the backend error it wraps.
	ReadError = engine.ReadError
	// WriteError is a backend error for one write. Its fields are Addr
	// uint64, the address of the request, and Err, the backend error it wraps.
	WriteError = engine.WriteError
)

// TypeError reports a Go type that cannot mirror C memory. Describing a
// request with such a type panics with a *TypeError. Its fields are Type
// reflect.Type, Path string, the field inside Type such as "Origin.X" (empty
// for Type itself), and Reason string, the rule the type breaks.
type TypeError = codec.TypeError

// ModuleError reports a module whose base address could not be resolved.
// Name is the name given to [Process.Module]; Err is the backend error, or
// [ErrUnsupported] if the backend has no module lookups.
type ModuleError struct {
	Name string
	Err  error
}

func (e *ModuleError) Error() string {
	return fmt.Sprintf("smartmem: module %q: %v", e.Name, e.Err)
}

func (e *ModuleError) Unwrap() error { return e.Err }
