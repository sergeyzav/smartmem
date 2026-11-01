package smartmem

import (
	"errors"
	"fmt"

	"github.com/sergeyzav/smartmem/internal/codec"
)

var (
	// ErrUnsupported is returned for targets v1 does not support, such as
	// WoW64 processes. It matches errors.ErrUnsupported too.
	ErrUnsupported = fmt.Errorf("smartmem: %w", errors.ErrUnsupported)
	// ErrNoTerminator means a string has no terminator within its maximum length.
	ErrNoTerminator = codec.ErrNoTerminator
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
