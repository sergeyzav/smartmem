package smartmem

import (
	"errors"
	"fmt"

	"github.com/sergeyzav/smartmem/internal/codec"
	"github.com/sergeyzav/smartmem/internal/engine"
)

var (
	// ErrUnsupported is returned for targets v1 does not support, such as
	// WoW64 processes. It matches errors.ErrUnsupported too.
	ErrUnsupported = fmt.Errorf("smartmem: %w", errors.ErrUnsupported)
	// ErrNoTerminator means a string has no terminator within its maximum length.
	ErrNoTerminator = codec.ErrNoTerminator
	// ErrNullBuffer means a UNICODE_STRING has a length but a null Buffer.
	ErrNullBuffer = codec.ErrNullBuffer
	// ErrPending is returned by Get before the value is ready.
	ErrPending = errors.New("smartmem: value not ready yet")
	// ErrBatchDone is returned by a second Batch.Run.
	ErrBatchDone = errors.New("smartmem: batch already ran")
	// ErrMaxRounds is the error of requests still pending after MaxRounds
	// rounds, for example while walking a cyclic list.
	ErrMaxRounds = engine.ErrMaxRounds
)

type (
	// NullPointerError means a Deref read a zero pointer. Step is the index of
	// that Deref, counting from 0. Later steps are not read.
	NullPointerError = engine.NullPointerError
	// BadPointerError means a Deref read a value that is not a user-mode x64 address.
	BadPointerError = engine.BadPointerError
	// PartialReadError means the backend read fewer bytes than requested.
	PartialReadError = engine.PartialReadError
	// ReadError is a backend error for one read. It wraps the backend error.
	ReadError = engine.ReadError
	// WriteError is a backend error for one write. It wraps the backend error.
	WriteError = engine.WriteError
)

// TypeError reports a Go type that cannot mirror C memory. Describing a
// request with such a type panics with a *TypeError.
type TypeError = codec.TypeError

// ModuleError reports a module whose base address could not be resolved.
type ModuleError struct {
	Name string
	Err  error
}

func (e *ModuleError) Error() string {
	return fmt.Sprintf("smartmem: module %q: %v", e.Name, e.Err)
}

func (e *ModuleError) Unwrap() error { return e.Err }
