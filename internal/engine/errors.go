package engine

import (
	"errors"
	"fmt"
)

// ErrMaxRounds is the error of requests still pending after MaxRounds rounds,
// for example while walking a cyclic list.
var ErrMaxRounds = errors.New("smartmem: too many rounds")

// NullPointerError means a Deref read a zero pointer. Step is the index of
// that Deref in the chain, counting from 0. Later steps are not read.
type NullPointerError struct{ Step int }

func (e *NullPointerError) Error() string {
	return fmt.Sprintf("smartmem: null pointer at deref %d", e.Step)
}

// BadPointerError means a Deref read a value that is not a user-mode x64
// address.
type BadPointerError struct {
	Step  int
	Value uint64
}

func (e *BadPointerError) Error() string {
	return fmt.Sprintf("smartmem: bad pointer 0x%x at deref %d", e.Value, e.Step)
}

// PartialReadError means the backend read fewer bytes than requested.
type PartialReadError struct {
	Addr      uint64
	Want, Got int
}

func (e *PartialReadError) Error() string {
	return fmt.Sprintf("smartmem: partial read at 0x%x: got %d of %d bytes", e.Addr, e.Got, e.Want)
}

// ReadError is a backend error for one read.
type ReadError struct {
	Addr uint64
	Err  error
}

func (e *ReadError) Error() string { return fmt.Sprintf("smartmem: read 0x%x: %v", e.Addr, e.Err) }
func (e *ReadError) Unwrap() error { return e.Err }

// WriteError is a backend error for one write.
type WriteError struct {
	Addr uint64
	Err  error
}

func (e *WriteError) Error() string { return fmt.Sprintf("smartmem: write 0x%x: %v", e.Addr, e.Err) }
func (e *WriteError) Unwrap() error { return e.Err }
