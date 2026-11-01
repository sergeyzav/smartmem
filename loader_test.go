package smartmem_test

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

type Vec3 struct{ X, Y, Z float32 }

// Record is the example from docs/DESIGN.md §9 / A.6:
// two flat fields, one field behind a pointer, a string behind a pointer.
type Record struct {
	ID     int32
	Class  uint8
	Origin Vec3
	Name   string
}

var recordBinds atomic.Int32

func (p *Record) Bind(l *smartmem.Loader, base smartmem.Loc) {
	recordBinds.Add(1)
	smartmem.Field(l, &p.ID, base.Add(0x100))
	smartmem.Field(l, &p.Class, base.Add(0x13C))
	smartmem.Field(l, &p.Origin, base.Add(0x30).Deref().Add(0x90))
	smartmem.FieldString(l, &p.Name, base.Add(0x50).Deref(), smartmem.CString(32))
}

// putRecord lays out a Record at base; its geometry and name live elsewhere.
func putRecord(fp *fake.Proc, base, geom, name uint64, id int32, origin Vec3, label string) {
	fp.I32(base+0x100, id).Bytes(base+0x13C, []byte{2})
	fp.U64(base+0x30, geom).U64(base+0x50, name)
	fake.Put(fp, geom+0x90, origin)
	fp.Bytes(name, append([]byte(label), 0))
}

// B4: with ALAP the flat fields wait for the pointer fields, so every field
// is read by the last round's scatter; without it they go in round 1. The
// number of rounds is the same.
func TestB4_ALAP(t *testing.T) {
	for _, tc := range []struct {
		alap  bool
		trace string
	}{
		{true, `
			r1 pid=100 R 0x10030+40
			r2 pid=100 R 0x10100+61 R 0x20090+12 R 0x30000+32`},
		{false, `
			r1 pid=100 R 0x10030+40 R 0x10100+61
			r2 pid=100 R 0x20090+12 R 0x30000+32`},
	} {
		t.Run(fmt.Sprint("alap=", tc.alap), func(t *testing.T) {
			e := newEnv(t)
			putRecord(e.fp, 0x10000, 0x20000, 0x30000, 100, Vec3{1, 2, 3}, "neo")
			b := e.s.NewBatch()
			f := smartmem.Load[Record](b, e.p.At(0x10000), smartmem.WithALAP(tc.alap))
			e.run(b)
			want := Record{ID: 100, Class: 2, Origin: Vec3{1, 2, 3}, Name: "neo"}
			if got := get(t, f); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			e.assertTrace(tc.trace)
		})
	}
}

// B1: the struct is not visible until its last field is in, even though
// some fields were read a round earlier.
func TestB1_AtomicPublish(t *testing.T) {
	e := newEnv(t)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 100, Vec3{1, 2, 3}, "neo")
	b := e.s.NewBatch()
	f := smartmem.Load[Record](b, e.p.At(0x10000), smartmem.WithALAP(false))
	checked := false
	e.mem.OnRound(func(round int, _ *fake.Memory) {
		if round == 2 { // ID and Class were read in round 1
			checked = true
			if _, err := f.Get(); !errors.Is(err, smartmem.ErrPending) {
				t.Errorf("after round 1: err=%v, want ErrPending", err)
			}
		}
	})
	e.run(b)
	if !checked {
		t.Fatal("round 2 never ran")
	}
	if get(t, f).Name != "neo" {
		t.Fatal("wrong value")
	}
}

type withOptional struct {
	A, B int32
}

func (w *withOptional) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &w.A, base)
	smartmem.Field(l, &w.B, base.Add(0x10).Deref(), smartmem.Optional())
}

// B2: a failing field fails the struct with a FieldError naming its Loc; an
// Optional field fails to its zero value instead.
func TestB2_FieldErrors(t *testing.T) {
	e := newEnv(t)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 100, Vec3{}, "neo")
	e.fp.U64(0x10050, 0)
	e.fp.I32(0x40000, 5).U64(0x40010, 0)
	b := e.s.NewBatch()
	bad := smartmem.Load[Record](b, e.p.At(0x10000))
	opt := smartmem.Load[withOptional](b, e.p.At(0x40000))
	e.run(b)

	var fe *smartmem.FieldError
	var ne *smartmem.NullPointerError
	err := getErr(t, bad)
	if !errors.As(err, &fe) || !errors.As(err, &ne) || fe.Loc.String() != "[0x10000+0x50]" {
		t.Fatalf("err=%v, want FieldError at [0x10000+0x50] wrapping NullPointerError", err)
	}
	if got := get(t, opt); got != (withOptional{A: 5}) {
		t.Fatalf("optional: got %+v", got)
	}
}

// B3: once a field fails, the struct's other fields are not read further.
func TestB3_FailFast(t *testing.T) {
	e := newEnv(t)
	// The base sits so that ID and Class land on the next page, which is
	// unmapped, while both pointer slots stay readable.
	putRecord(e.fp, 0x10f00, 0x20000, 0x30000, 100, Vec3{}, "neo")
	e.fp.Unmap(0x11000)
	b := e.s.NewBatch()
	f := smartmem.Load[Record](b, e.p.At(0x10f00), smartmem.WithALAP(false))
	e.run(b)
	_ = getErr(t, f)
	// Round 1 read both pointers fine, but ID failed, so round 2 (Origin, Name)
	// never runs.
	e.assertTrace(`r1 pid=100 R 0x10f30+40 R 0x11000+61`)
}

// B5: with a dynamic base (LoadEach after a pointer list) ALAP counts from
// the round the base is known: list in round 1, pointers in 2, fields in 3.
func TestB5_ALAPWithDynamicBase(t *testing.T) {
	e := newEnv(t)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{}, "a")
	putRecord(e.fp, 0x50000, 0x60000, 0x70000, 2, Vec3{}, "b")
	e.fp.U64(0x8000, 0x10000).U64(0x8008, 0x50000)
	b := e.s.NewBatch()
	list := smartmem.ReadSlice[smartmem.Ptr[Record]](b, e.p.At(0x8000), 2)
	records := smartmem.LoadEach[Record](b, e.p, list)
	e.run(b)
	got := get(t, records)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("got %+v", got)
	}
	e.assertTrace(`
		r1 pid=100 R 0x8000+16
		r2 pid=100 R 0x10030+40 R 0x50030+40
		r3 pid=100 R 0x10100+61 R 0x20090+12 R 0x30000+32 R 0x50100+61 R 0x60090+12 R 0x70000+32`)
}

// B6: Revalidate notices a pointer that changed between rounds and reads the
// struct again; if it keeps changing, the load fails with ErrUnstable.
func TestB6_Revalidate(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		e := newEnv(t)
		putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{1, 1, 1}, "neo")
		fake.Put(e.fp, 0x21090, Vec3{9, 9, 9})
		e.mem.OnRound(func(round int, _ *fake.Memory) {
			if round == 2 { // after the pointer was read, before the fields
				e.fp.U64(0x10030, 0x21000)
			}
		})
		b := e.s.NewBatch()
		f := smartmem.Load[Record](b, e.p.At(0x10000), smartmem.Revalidate(1))
		e.run(b)
		if got := get(t, f).Origin; got != (Vec3{9, 9, 9}) {
			t.Fatalf("Origin = %v, want the moved geometry's {9 9 9}", got)
		}
		if e.mem.Execs() != 4 {
			t.Fatalf("%d Execs, want 4 (2 rounds, then 2 more for the retry)", e.mem.Execs())
		}
	})
	t.Run("unstable", func(t *testing.T) {
		e := newEnv(t)
		putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{}, "neo")
		e.mem.OnRound(func(round int, _ *fake.Memory) {
			e.fp.U64(0x10030, 0x20000+uint64(round)*0x1000)
			e.fp.Map(0x20000+uint64(round)*0x1000, fake.PageSize)
		})
		b := e.s.NewBatch()
		f := smartmem.Load[Record](b, e.p.At(0x10000), smartmem.Revalidate(2))
		e.run(b)
		if err := getErr(t, f); !errors.Is(err, smartmem.ErrUnstable) {
			t.Fatalf("err=%v, want ErrUnstable", err)
		}
	})
}

// B7: Revalidate costs no extra round when nothing changes: the pointer
// re-reads join the last round's scatter.
func TestB7_RevalidateIsFree(t *testing.T) {
	e := newEnv(t)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{}, "neo")
	b := e.s.NewBatch()
	f := smartmem.Load[Record](b, e.p.At(0x10000), smartmem.Revalidate(1))
	e.run(b)
	get(t, f)
	e.assertTrace(`
		r1 pid=100 R 0x10030+40
		r2 pid=100 R 0x10030+40 R 0x10100+61 R 0x20090+12 R 0x30000+32`)
}

type Group struct {
	ID    uint32
	Owner Record
}

func (g *Group) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &g.ID, base)
	smartmem.Sub(l, &g.Owner, base.Add(8).Deref())
}

// B8: Sub binds a nested struct whose fields are published with the outer one.
func TestB8_Sub(t *testing.T) {
	e := newEnv(t)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 7, Vec3{}, "root")
	e.fp.U32(0x9000, 42).U64(0x9008, 0x10000)
	b := e.s.NewBatch()
	f := smartmem.Load[Group](b, e.p.At(0x9000))
	e.run(b)
	if got := get(t, f); got.ID != 42 || got.Owner.Name != "root" || got.Owner.ID != 7 {
		t.Fatalf("got %+v", got)
	}
}

// B9: LoadEach skips nil pointers and leaves failed elements out, unless Strict.
func TestB9_LoadEach(t *testing.T) {
	setup := func(t *testing.T) (*env, *smartmem.Future[[]smartmem.Ptr[Record]], *smartmem.Batch) {
		e := newEnv(t)
		putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{}, "a")
		putRecord(e.fp, 0x50000, 0x60000, 0x70000, 3, Vec3{}, "c")
		e.fp.U64(0x8000, 0x10000).U64(0x8008, 0).U64(0x8010, 0x90000).U64(0x8018, 0x50000)
		b := e.s.NewBatch()
		return e, smartmem.ReadSlice[smartmem.Ptr[Record]](b, e.p.At(0x8000), 4), b
	}

	e, list, b := setup(t)
	all := smartmem.LoadEach[Record](b, e.p, list)
	e.run(b)
	got := get(t, all)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("got %+v, want a and c", got)
	}

	e, list, b = setup(t)
	strict := smartmem.LoadEach[Record](b, e.p, list, smartmem.Strict())
	e.run(b)
	var fe *smartmem.FieldError
	if err := getErr(t, strict); !errors.As(err, &fe) {
		t.Fatalf("strict: err=%v, want FieldError", err)
	}

	e, _, b = setup(t)
	empty := smartmem.LoadEach[Record](b, e.p, smartmem.ReadSlice[smartmem.Ptr[Record]](b, e.p.At(0x8008), 1))
	e.run(b)
	if v := get(t, empty); v == nil || len(v) != 0 {
		t.Fatalf("all-nil list: %#v", v)
	}
}

// B10: Bind runs once per Load, not per field or round; LoadEach binds once
// per element slot.
func TestB10_BindOnce(t *testing.T) {
	e := newEnv(t)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{}, "a")
	putRecord(e.fp, 0x50000, 0x60000, 0x70000, 2, Vec3{}, "b")
	e.fp.U64(0x8000, 0x10000).U64(0x8008, 0x50000)
	recordBinds.Store(0)
	b := e.s.NewBatch()
	smartmem.Load[Record](b, e.p.At(0x10000), smartmem.Revalidate(3))
	smartmem.LoadEach[Record](b, e.p, smartmem.ReadSlice[smartmem.Ptr[Record]](b, e.p.At(0x8000), 2))
	e.run(b)
	if n := recordBinds.Load(); n != 3 {
		t.Fatalf("Bind ran %d times, want 3 (one Load, two slots)", n)
	}
}

// A struct with mem tags is read field by field and published at once.
func TestReadSparse(t *testing.T) {
	type Item struct {
		ID     int32  `mem:"0x100"`
		Class  uint8  `mem:"0x13C"`
		Origin Vec3   `mem:"0x30"`
		Note   uint64 `mem:"-"`
	}
	e := newEnv(t)
	e.fp.I32(0x10100, 50).Bytes(0x1013C, []byte{4})
	fake.Put(e.fp, 0x10030, Vec3{1, 2, 3})
	e.fp.U64(0x9000, 0x10000)
	b := e.s.NewBatch()
	f := smartmem.Read[Item](b, e.p.Chain(e.p.At(0x9000), 0))
	e.run(b)
	if got := get(t, f); got != (Item{ID: 50, Class: 4, Origin: Vec3{1, 2, 3}}) {
		t.Fatalf("got %+v", got)
	}
	e.assertTrace(`
		r1 pid=100 R 0x9000+8
		r2 pid=100 R 0x10030+12 R 0x10100+61`)
}
