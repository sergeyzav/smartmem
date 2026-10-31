package smartmem

import (
	"context"
	"unsafe"

	"github.com/sergeyzav/smartmem/internal/codec"
	"github.com/sergeyzav/smartmem/internal/engine"
)

// Read describes a read of a T at loc. T must be blittable: fixed-size
// numbers, arrays and structs of them, Ptr, Addr or CBool. An unsupported T
// panics with a *TypeError.
func Read[T any](s Scope, loc Loc) *Future[T] {
	g := s.scope()
	c := codec.MustOf[T]()
	if c.Kind != codec.Blittable {
		panic("smartmem: Read: " + c.Type.String() + " is a sparse struct; read it with Load")
	}
	f := newFuture[T](g)
	leaf := &engine.Node{Kind: engine.Leaf, Size: uint32(c.Size), Sink: &valueSink[T]{f: f, c: c}}
	g.add(g.newChain(loc, leaf))
	return f
}

type valueSink[T any] struct {
	f *Future[T]
	c *codec.Codec
}

func (s *valueSink[T]) Done(b []byte, err error) {
	if err == nil {
		s.c.Decode(unsafe.Pointer(&s.f.sv), b)
	}
	s.f.resolveStaged(err)
}

// ReadSlice describes a read of n consecutive Ts at loc, as one range.
func ReadSlice[T any](s Scope, loc Loc, n int) *Future[[]T] {
	g := s.scope()
	c := codec.MustOf[T]()
	if c.Kind != codec.Blittable {
		panic("smartmem: ReadSlice: " + c.Type.String() + " is a sparse struct")
	}
	if n < 0 {
		panic("smartmem: ReadSlice: negative length")
	}
	f := newFuture[[]T](g)
	if n == 0 {
		g.add(resolved(func() { f.resolve([]T{}, nil) }))
		return f
	}
	leaf := &engine.Node{Kind: engine.Leaf, Size: uint32(n) * uint32(c.Size), Sink: &sliceSink[T]{f: f, n: n}}
	g.add(g.newChain(loc, leaf))
	return f
}

type sliceSink[T any] struct {
	f *Future[[]T]
	n int
}

func (s *sliceSink[T]) Done(b []byte, err error) {
	if err != nil {
		s.f.resolve(nil, err)
		return
	}
	v := make([]T, s.n)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&v[0])), len(b)), b)
	s.f.resolve(v, nil)
}

// resolved is a request without I/O: it runs fn when the execution starts.
type resolved func()

func (fn resolved) schedule(*engine.Run) { fn() }

// StringKind says how a string is stored in memory.
type StringKind struct {
	enc      uint8
	max      int
	truncate bool
}

const (
	encCString = iota + 1
	encUTF16
	encUnicodeString
)

// CString is a NUL-terminated 8-bit string of at most maxBytes bytes,
// including the NUL. The bytes are returned as they are.
func CString(maxBytes int) StringKind { return StringKind{enc: encCString, max: maxBytes} }

// UTF16 is a NUL-terminated little-endian UTF-16 string of at most maxChars
// code units, including the NUL.
func UTF16(maxChars int) StringKind { return StringKind{enc: encUTF16, max: 2 * maxChars} }

// UnicodeString is a Windows UNICODE_STRING. It takes two rounds: the header,
// then the characters. The length is capped by WithMaxStringLen.
var UnicodeString = StringKind{enc: encUnicodeString}

// Truncate returns the kind that keeps the whole buffer when there is no
// terminator, instead of failing with ErrNoTerminator.
func (k StringKind) Truncate() StringKind {
	k.truncate = true
	return k
}

// ReadString describes a read of a string at loc.
func ReadString(s Scope, loc Loc, kind StringKind) *Future[string] {
	g := s.scope()
	f := newFuture[string](g)
	g.add(g.newChain(loc, newStringLeaf(g, kind, f.resolve)))
	return f
}

func newStringLeaf(g *graph, kind StringKind, done func(string, error)) *engine.Node {
	switch kind.enc {
	case encCString, encUTF16:
		if kind.max <= 0 {
			panic("smartmem: string kind needs a positive maximum length")
		}
		return &engine.Node{Kind: engine.Leaf, Size: uint32(kind.max), Sink: &stringSink{kind: kind, done: done}}
	case encUnicodeString:
		us := &unicodeSink{g: g, done: done}
		us.buf.Kind = engine.Leaf
		us.buf.Sink = (*unicodeBufSink)(us)
		us.header = &engine.Node{Kind: engine.Leaf, Size: uint32(unsafe.Sizeof(codec.UnicodeStringHeader{})), Sink: us}
		return us.header
	default:
		panic("smartmem: zero StringKind; use CString, UTF16 or UnicodeString")
	}
}

type stringSink struct {
	kind StringKind
	done func(string, error)
}

func (s *stringSink) Done(b []byte, err error) {
	if err != nil {
		s.done("", err)
		return
	}
	if s.kind.enc == encCString {
		s.done(codec.DecodeCString(b, s.kind.truncate))
	} else {
		s.done(codec.DecodeUTF16(b, s.kind.truncate))
	}
}

// unicodeSink reads a UNICODE_STRING header, then schedules its buffer.
type unicodeSink struct {
	g      *graph
	done   func(string, error)
	header *engine.Node // the header leaf; the buffer read uses its pid and flags
	buf    engine.Node
}

func (s *unicodeSink) Done(b []byte, err error) {
	if err != nil {
		s.done("", err)
		return
	}
	var h codec.UnicodeStringHeader
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&h)), unsafe.Sizeof(h)), b)
	addr, n, err := codec.UnicodeRange(h, s.g.s.opts.maxStringLen)
	if err != nil || n == 0 {
		s.done("", err)
		return
	}
	s.buf.Size = uint32(n)
	s.buf.PID, s.buf.Flags = s.header.PID, s.header.Flags
	s.g.run.Add(&s.buf, addr)
}

type unicodeBufSink unicodeSink

func (s *unicodeBufSink) Done(b []byte, err error) {
	if err != nil {
		s.done("", err)
		return
	}
	s.done(codec.DecodeUTF16Exact(b), nil)
}

// Write describes a write of v at loc. Writes run before reads in their round
// and are never merged. The Future fails only if the backend reports an error
// for this write. Plans do not support writes.
func Write[T any](s Scope, loc Loc, v T) *Future[struct{}] {
	g := s.scope()
	if g.pub != nil {
		panic("smartmem: Write is not supported in a Plan")
	}
	c := codec.MustOf[T]()
	if c.Kind != codec.Blittable {
		panic("smartmem: Write: " + c.Type.String() + " is a sparse struct")
	}
	data := make([]byte, c.Size)
	c.Encode(data, unsafe.Pointer(&v))
	f := newFuture[struct{}](g)
	tail := &engine.Node{Kind: engine.Write, Data: data, Sink: &writeSink{f: f}}
	g.add(g.newChain(loc, tail))
	return f
}

type writeSink struct{ f *Future[struct{}] }

func (s *writeSink) Done(_ []byte, err error) { s.f.resolve(struct{}{}, err) }

// ReadNow reads a T at loc in a Batch of its own.
func ReadNow[T any](ctx context.Context, loc Loc) (T, error) {
	loc.mustBeValid()
	b := loc.proc.s.NewBatch()
	f := Read[T](b, loc)
	if err := b.Run(ctx); err != nil {
		var zero T
		return zero, err
	}
	return f.Get()
}

// WriteNow writes v at loc in a Batch of its own.
func WriteNow[T any](ctx context.Context, loc Loc, v T) error {
	loc.mustBeValid()
	b := loc.proc.s.NewBatch()
	f := Write(b, loc, v)
	if err := b.Run(ctx); err != nil {
		return err
	}
	_, err := f.Get()
	return err
}
