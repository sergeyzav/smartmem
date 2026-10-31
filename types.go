package smartmem

// Addr is a raw 64-bit address in target memory.
type Addr uint64

// Ptr is a 64-bit pointer to a T in target memory. It has the same layout as a
// C pointer, so it can be a field of a blittable struct.
type Ptr[T any] uint64

// IsNil reports whether the pointer is zero.
func (p Ptr[T]) IsNil() bool { return p == 0 }

// Addr returns the address the pointer holds.
func (p Ptr[T]) Addr() uint64 { return uint64(p) }

// Loc returns the location the pointer points to, in process proc.
func (p Ptr[T]) Loc(proc *Process) Loc { return proc.At(uint64(p)) }

// CBool is a one-byte C boolean. Go bool is not allowed in memory types,
// because a byte other than 0 or 1 is invalid in a Go bool.
type CBool uint8

// Bool reports whether b is non-zero.
func (b CBool) Bool() bool { return b != 0 }
