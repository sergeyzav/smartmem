// Package codec maps Go types to C memory layout and converts between the two.
//
// Two kinds of types are supported:
//
//   - Blittable: fixed-size integers and floats, arrays of them, and structs
//     made only of blittable fields. The Go layout equals the x64 C layout, so
//     values are copied byte for byte.
//   - Sparse: structs with `mem:"<offset>"` tags. Each tagged field is read
//     on its own from base+offset; the Go layout does not matter.
package codec

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"unsafe"
)

// Kind is the kind of a Codec.
type Kind uint8

const (
	// Blittable types are copied byte for byte.
	Blittable Kind = iota + 1
	// Sparse structs are read field by field.
	Sparse
)

// Codec describes how one Go type maps to memory.
type Codec struct {
	Type reflect.Type
	Kind Kind
	// Size is the size in memory of a Blittable type. It is 0 for Sparse.
	Size uintptr
	// Fields lists the fields of a Sparse struct, in declaration order.
	Fields []Field
}

// Field is one field of a Sparse struct.
type Field struct {
	Name      string
	MemOffset uint64  // offset in target memory, from the mem tag
	GoOffset  uintptr // offset in the Go struct
	Codec     *Codec  // always Blittable
}

// TypeError reports a type that cannot mirror C memory.
type TypeError struct {
	Type   reflect.Type
	Path   string // field path inside Type, for example "Pos.X"; empty for Type itself
	Reason string
}

func (e *TypeError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("smartmem: type %s: %s", e.Type, e.Reason)
	}
	return fmt.Sprintf("smartmem: type %s: field %s: %s", e.Type, e.Path, e.Reason)
}

// Decode copies a Blittable value from src to dst. dst points to a value of
// c.Type, and src holds at least c.Size bytes.
func (c *Codec) Decode(dst unsafe.Pointer, src []byte) {
	if c.Kind != Blittable {
		panic("codec: Decode on a sparse type " + c.Type.String())
	}
	copy(unsafe.Slice((*byte)(dst), c.Size), src[:c.Size])
}

// Encode copies a Blittable value from src to dst. src points to a value of
// c.Type, and dst holds at least c.Size bytes.
func (c *Codec) Encode(dst []byte, src unsafe.Pointer) {
	if c.Kind != Blittable {
		panic("codec: Encode on a sparse type " + c.Type.String())
	}
	copy(dst[:c.Size], unsafe.Slice((*byte)(src), c.Size))
}

// Decode copies the field from src into the struct that structPtr points to.
func (f *Field) Decode(structPtr unsafe.Pointer, src []byte) {
	f.Codec.Decode(unsafe.Add(structPtr, f.GoOffset), src)
}

// MemSpan returns the end offset of the last byte any field of a Sparse struct
// reads, so [0, MemSpan) covers every field.
func (c *Codec) MemSpan() uint64 {
	var end uint64
	for _, f := range c.Fields {
		end = max(end, f.MemOffset+uint64(f.Codec.Size))
	}
	return end
}

type entry struct {
	c   *Codec
	err error
}

var cache sync.Map // reflect.Type -> *entry

// For returns the Codec of t. Results, including errors, are cached.
func For(t reflect.Type) (*Codec, error) {
	if e, ok := cache.Load(t); ok {
		e := e.(*entry)
		return e.c, e.err
	}
	c, err := build(t)
	e, _ := cache.LoadOrStore(t, &entry{c: c, err: err})
	return e.(*entry).c, e.(*entry).err
}

// Of returns the Codec of T.
func Of[T any]() (*Codec, error) { return For(reflect.TypeFor[T]()) }

// MustOf returns the Codec of T and panics if T is not supported. Unsupported
// types are programmer errors, found by the first test that uses them.
func MustOf[T any]() *Codec {
	c, err := Of[T]()
	if err != nil {
		panic(err)
	}
	return c
}

func build(t reflect.Type) (*Codec, error) {
	if t.Kind() == reflect.Struct && hasMemTags(t) {
		return buildSparse(t)
	}
	if reason, path := checkBlittable(t, ""); reason != "" {
		return nil, &TypeError{Type: t, Path: path, Reason: reason}
	}
	if t.Size() == 0 {
		return nil, &TypeError{Type: t, Reason: "zero-size types cannot be read"}
	}
	if err := checkCSize(t); err != nil {
		return nil, err
	}
	return &Codec{Type: t, Kind: Blittable, Size: t.Size()}, nil
}

func checkBlittable(t reflect.Type, path string) (reason, at string) {
	switch t.Kind() {
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "", ""
	case reflect.Array:
		return checkBlittable(t.Elem(), path+"[]")
	case reflect.Struct:
		if hasMemTags(t) {
			return "struct " + t.String() + " has mem tags, so it is sparse and cannot be nested; load it with a Loader", path
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if reason, at := checkBlittable(f.Type, join(path, f.Name)); reason != "" {
				return reason, at
			}
		}
		return "", ""
	case reflect.Bool:
		return "bool is not allowed: a byte other than 0 or 1 is invalid in a Go bool; use smartmem.CBool", path
	case reflect.Int, reflect.Uint:
		return t.Kind().String() + " has a platform-dependent size; use a sized type such as int32 or uint64", path
	case reflect.Uintptr:
		return "uintptr is not allowed; use smartmem.Addr or smartmem.Ptr[T]", path
	default:
		return t.Kind().String() + " is not allowed: only fixed-size values without Go pointers can mirror C memory", path
	}
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func hasMemTags(t reflect.Type) bool {
	for i := range t.NumField() {
		if _, ok := t.Field(i).Tag.Lookup("mem"); ok {
			return true
		}
	}
	return false
}

func buildSparse(t reflect.Type) (*Codec, error) {
	c := &Codec{Type: t, Kind: Sparse}
	for i := range t.NumField() {
		f := t.Field(i)
		tag, ok := f.Tag.Lookup("mem")
		if !ok && f.Name == "_" {
			continue
		}
		if !ok {
			return nil, &TypeError{Type: t, Path: f.Name, Reason: `every field of a sparse struct needs mem:"<offset>" or mem:"-"`}
		}
		if tag == "-" {
			continue
		}
		off, err := strconv.ParseUint(tag, 0, 64)
		if err != nil {
			return nil, &TypeError{Type: t, Path: f.Name, Reason: fmt.Sprintf("bad mem tag %q: want an offset such as \"0x48\"", tag)}
		}
		if reason, at := checkBlittable(f.Type, f.Name); reason != "" {
			return nil, &TypeError{Type: t, Path: at, Reason: reason}
		}
		fc, err := For(f.Type)
		if err != nil {
			return nil, &TypeError{Type: t, Path: f.Name, Reason: err.Error()}
		}
		c.Fields = append(c.Fields, Field{Name: f.Name, MemOffset: off, GoOffset: f.Offset, Codec: fc})
	}
	if len(c.Fields) == 0 {
		return nil, &TypeError{Type: t, Reason: "sparse struct has no fields to read"}
	}
	return c, nil
}

type cSizer interface{ CSize() uintptr }

func checkCSize(t reflect.Type) error {
	s, ok := reflect.New(t).Interface().(cSizer)
	if !ok {
		return nil
	}
	if want := s.CSize(); want != t.Size() {
		return &TypeError{Type: t, Reason: fmt.Sprintf("Go size %d does not match CSize() %d", t.Size(), want)}
	}
	return nil
}
