package smartmem

import (
	"fmt"
	"slices"
	"strings"
)

type baseKind uint8

const (
	baseAddr baseKind = iota + 1
	baseModule
	baseCell // the base is read from a cell at run time (LoadEach elements)
)

// Loc describes how to get to an address: a base, then a sequence of Add and
// Deref steps. It is an immutable value and does no I/O; every method returns
// a new Loc.
//
// Semantics: p.At(x).Deref().Add(o) reads the pointer y stored at x, and the
// location is y+o. Process.Chain follows the Cheat Engine pointer notation.
//
// The zero Loc is invalid; build one with Process.At or Process.Module.
type Loc struct {
	proc   *Process
	kind   baseKind
	addr   uint64
	module string
	cell   *uint64
	// offs[0] is added to the base; offs[i] is added after the i-th Deref.
	// len(offs)-1 is the number of Derefs. Consecutive Adds fold together.
	offs []int64
}

// At returns the static location addr.
func (p *Process) At(addr uint64) Loc {
	return Loc{proc: p, kind: baseAddr, addr: addr, offs: []int64{0}}
}

// Module returns the location of a module's base address. The base is
// resolved when a request runs and is cached in the Process.
func (p *Process) Module(name string) Loc {
	return Loc{proc: p, kind: baseModule, module: name, offs: []int64{0}}
}

// Chain follows the Cheat Engine pointer notation: Chain(B, o1, ..., on) is
// [[[B]+o1]+o2]...+on, that is B.Deref().Add(o1)...Deref().Add(on). The base
// is dereferenced, and n offsets mean n Derefs; Chain(B) is B itself.
func (p *Process) Chain(base Loc, offs ...int64) Loc {
	base.mustBeValid()
	if base.proc != p {
		panic("smartmem: Chain: base belongs to another process")
	}
	for _, o := range offs {
		base = base.Deref().Add(o)
	}
	return base
}

// Add moves the location by off bytes.
func (l Loc) Add(off int64) Loc {
	l.mustBeValid()
	offs := slices.Clone(l.offs)
	offs[len(offs)-1] += off
	l.offs = offs
	return l
}

// Deref reads the pointer at the location; the result is where it points.
func (l Loc) Deref() Loc {
	l.mustBeValid()
	offs := make([]int64, len(l.offs)+1)
	copy(offs, l.offs)
	l.offs = offs
	return l
}

// Index moves the location to element i of an array with the given stride.
func (l Loc) Index(i, stride int) Loc { return l.Add(int64(i) * int64(stride)) }

// Depth returns the number of Derefs. static is false when the depth is only
// known at run time; that is never the case yet.
func (l Loc) Depth() (n int, static bool) {
	l.mustBeValid()
	return len(l.offs) - 1, true
}

// Process returns the process the location belongs to.
func (l Loc) Process() *Process { return l.proc }

// IsZero reports whether l is the zero Loc.
func (l Loc) IsZero() bool { return l.proc == nil }

// String formats the location in Cheat Engine notation, for example
// "[[client.dll+0x10]+0x30]+0x90".
func (l Loc) String() string {
	if l.IsZero() {
		return "<zero Loc>"
	}
	var b strings.Builder
	b.WriteString(strings.Repeat("[", len(l.offs)-1))
	switch l.kind {
	case baseModule:
		b.WriteString(l.module)
	case baseCell:
		b.WriteString("<element>")
	default:
		fmt.Fprintf(&b, "0x%x", l.addr)
	}
	for i, off := range l.offs {
		if i > 0 {
			b.WriteByte(']')
		}
		switch {
		case off > 0:
			fmt.Fprintf(&b, "+0x%x", off)
		case off < 0:
			fmt.Fprintf(&b, "-0x%x", uint64(-off))
		}
	}
	return b.String()
}

func (l Loc) mustBeValid() {
	if l.IsZero() {
		panic("smartmem: use of the zero Loc; build one with Process.At or Process.Module")
	}
}
