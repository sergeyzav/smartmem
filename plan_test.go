package smartmem_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
	"github.com/sergeyzav/smartmem/internal/testutil"
)

func (e *env) exec(p *smartmem.Plan) {
	e.t.Helper()
	if err := p.Exec(e.ctx); err != nil {
		e.t.Fatalf("Exec: %v", err)
	}
}

// P1: every Exec reads memory again; Gen counts the Execs.
func TestP1_ExecRereads(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	plan := e.s.NewPlan()
	f := smartmem.Read[uint32](plan, e.p.At(0x1000))
	twice := smartmem.Map(f, func(v uint32) (uint32, error) { return 2 * v, nil })
	late := smartmem.Read[uint32](plan, e.p.At(0x5000))
	if f.Gen() != 0 {
		t.Fatal("Gen before the first Exec is not 0")
	}
	e.exec(plan)
	_ = getErr(t, late)
	e.fp.U32(0x1000, 2).U32(0x5000, 5)
	e.exec(plan)
	if get(t, f) != 2 || f.Gen() != 2 || plan.Gen() != 2 {
		t.Fatalf("value %d gen %d plan gen %d, want 2/2/2", get(t, f), f.Gen(), plan.Gen())
	}
	if get(t, twice) != 4 {
		t.Fatalf("Map = %d, want 4: continuations must run on every Exec", get(t, twice))
	}
	if get(t, late) != 5 {
		t.Fatal("an error of the first Exec stuck")
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+4 R 0x5000+4
		r1 pid=100 R 0x1000+4 R 0x5000+4`)
}

type Summary struct {
	ID     int32
	Origin Vec3
}

func (s *Summary) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &s.ID, base.Add(0x100))
	smartmem.Field(l, &s.Origin, base.Add(0x30).Deref().Add(0x90))
}

// P2: a static Plan (values, chains, structs, an unchanged string, Map)
// runs without allocating.
func TestP2_ZeroAlloc(t *testing.T) {
	if testutil.RaceEnabled {
		t.Skip("the race detector allocates")
	}
	e := newEnv(t)
	e.mem.SetTrace(false)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 100, Vec3{1, 2, 3}, "neo")
	e.fp.U32(0x1000, 7)
	unicodeString(e, 0x2000, 0x3000, "unchanged")

	plan := e.s.NewPlan()
	flat := smartmem.Read[uint32](plan, e.p.At(0x1000))
	origin := smartmem.Read[Vec3](plan, e.p.Chain(e.p.At(0x10030), 0x90))
	summary := smartmem.Load[Summary](plan, e.p.At(0x10000), smartmem.Revalidate(1))
	name := smartmem.ReadString(plan, e.p.Chain(e.p.At(0x10050), 0), smartmem.CString(32))
	path := smartmem.ReadString(plan, e.p.At(0x2000), smartmem.UnicodeString)
	twice := smartmem.Map(flat, func(v uint32) (uint32, error) { return 2 * v, nil })
	e.exec(plan) // warm up: buffers grow, strings are decoded once

	allocs := testing.AllocsPerRun(100, func() { e.exec(plan) })
	if allocs != 0 {
		t.Fatalf("Exec allocates %.1f times, want 0", allocs)
	}
	if get(t, twice) != 14 || get(t, origin) != (Vec3{1, 2, 3}) || get(t, summary).ID != 100 ||
		get(t, name) != "neo" || get(t, path) != "unchanged" {
		t.Fatal("wrong values")
	}
}

// P3: an unchanged string costs nothing; a changed one is decoded again.
func TestP3_StringReuse(t *testing.T) {
	if testutil.RaceEnabled {
		t.Skip("the race detector allocates")
	}
	e := newEnv(t)
	e.mem.SetTrace(false)
	e.fp.Bytes(0x1000, []byte("same\x00"))
	plan := e.s.NewPlan()
	f := smartmem.ReadString(plan, e.p.At(0x1000), smartmem.CString(16))
	e.exec(plan)
	if allocs := testing.AllocsPerRun(50, func() { e.exec(plan) }); allocs != 0 {
		t.Fatalf("unchanged string: %.1f allocs", allocs)
	}
	n := byte('a')
	next := func() []byte { return []byte{'p', 'l', 'a', 'y', 'e', 'r', n, 0} } // >1 byte: Go does not allocate 1-byte strings
	e.mem.OnRound(func(int, *fake.Memory) { n++; e.fp.Bytes(0x1000, next()) })
	if allocs := testing.AllocsPerRun(50, func() { e.exec(plan) }); allocs == 0 {
		t.Fatal("a changing string did not allocate: is it decoded at all?")
	}
	if got, want := get(t, f), string(next()[:7]); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// P4: LoadEach with a stable element count binds once per slot and allocates
// only its result slice per Exec.
func TestP4_LoadEachSteadyState(t *testing.T) {
	if testutil.RaceEnabled {
		t.Skip("the race detector allocates")
	}
	e := newEnv(t)
	e.mem.SetTrace(false)
	putRecord(e.fp, 0x10000, 0x20000, 0x30000, 1, Vec3{}, "a")
	putRecord(e.fp, 0x50000, 0x60000, 0x70000, 2, Vec3{}, "b")
	e.fp.U64(0x8000, 0x10000).U64(0x8008, 0x50000)
	plan := e.s.NewPlan()
	list := smartmem.ReadSlice[smartmem.Ptr[Record]](plan, e.p.At(0x8000), 2)
	records := smartmem.LoadEach[Record](plan, e.p, list)
	recordBinds.Store(0)
	e.exec(plan)
	binds := recordBinds.Load()

	allocs := testing.AllocsPerRun(50, func() { e.exec(plan) })
	if allocs > 2 { // the pointer list and the result slice
		t.Fatalf("Exec allocates %.1f times, want at most 2", allocs)
	}
	if recordBinds.Load() != binds || binds != 2 {
		t.Fatalf("Bind ran %d times after the first Exec's %d", recordBinds.Load()-binds, binds)
	}
	if ps := get(t, records); len(ps) != 2 || ps[1].Name != "b" {
		t.Fatalf("got %+v", ps)
	}
}

// P5: values read inside View come from the same Exec, while Execs keep
// running on another goroutine.
func TestP5_ViewIsConsistent(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 0).U32(0x3000, 0)
	var n uint32
	e.mem.OnRound(func(int, *fake.Memory) { n++; e.fp.U32(0x1000, n).U32(0x3000, n) })
	plan := e.s.NewPlan()
	a := smartmem.Read[uint32](plan, e.p.At(0x1000))
	b := smartmem.Read[uint32](plan, e.p.At(0x3000))
	e.exec(plan)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			plan.View(func() {
				va, _ := a.Get() // Get inside View takes the lock again: must not deadlock
				vb, _ := b.Get()
				if va != vb {
					t.Errorf("View saw a=%d b=%d", va, vb)
				}
			})
		}
	}()
	for range 500 {
		e.exec(plan)
	}
	stop.Store(true)
	wg.Wait()
}

// P6: a frozen Plan rejects new requests, and Plans never write.
func TestP6_Frozen(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	plan := e.s.NewPlan()
	f := smartmem.Read[uint32](plan, e.p.At(0x1000))
	mustPanic(t, "Write", "not supported in a Plan", func() { smartmem.Write(plan, e.p.At(0x1000), uint32(1)) })
	e.exec(plan)
	mustPanic(t, "Read after Exec", "already ran", func() { smartmem.Read[uint32](plan, e.p.At(0x1000)) })
	mustPanic(t, "Map after Exec", "already ran", func() {
		smartmem.Map(f, func(v uint32) (uint32, error) { return v, nil })
	})
}

// P7: concurrent Execs run one after another.
func TestP7_ConcurrentExec(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	plan := e.s.NewPlan()
	f := smartmem.Read[uint32](plan, e.p.At(0x1000))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if err := plan.Exec(context.Background()); err != nil {
					t.Error(err)
				}
				_, _ = f.Get()
			}
		}()
	}
	wg.Wait()
	if plan.Gen() != 200 || e.mem.Execs() != 200 {
		t.Fatalf("Gen %d, Execs %d, want 200", plan.Gen(), e.mem.Execs())
	}
}

// Next hands a subscriber every new value of a Plan exactly once.
func TestPlanNext(t *testing.T) {
	e := newEnv(t)
	plan := e.s.NewPlan()
	f := smartmem.Read[uint32](plan, e.p.At(0x1000))
	got := make(chan uint32)
	go func() {
		var gen uint64
		for range 3 {
			v, g, err := f.Next(e.ctx, gen)
			if err != nil || g <= gen {
				t.Errorf("Next: gen %d after %d, err %v", g, gen, err)
				return
			}
			gen = g
			got <- v
		}
	}()
	for i := range uint32(3) {
		e.fp.U32(0x1000, 10+i)
		e.exec(plan)
		if v := <-got; v != 10+i {
			t.Fatalf("Next = %d, want %d", v, 10+i)
		}
	}
}

func TestPlanWithBatchAndErrors(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1).U32(0x1010, 2)
	plan := e.s.NewPlan()
	pf := smartmem.Read[uint32](plan, e.p.At(0x1000))
	bad := smartmem.Read[uint32](plan, e.p.At(0x9000))
	b := e.s.NewBatch()
	bf := smartmem.Read[uint32](b, e.p.At(0x1010))
	if err := smartmem.Run(e.ctx, plan, b); err != nil {
		t.Fatal(err)
	}
	if get(t, pf) != 1 || get(t, bf) != 2 || plan.Gen() != 1 {
		t.Fatal("merged Run did not publish")
	}
	if !errors.Is(getErr(t, bad), fake.ErrUnmapped) {
		t.Fatal("error not published")
	}
	e.assertTrace(`r1 pid=100 R 0x1000+20 R 0x9000+4`)
	mustPanic(t, "Run twice", "passed twice", func() { _ = smartmem.Run(e.ctx, plan, plan) })
}
