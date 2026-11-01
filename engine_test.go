package smartmem_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend"
	"github.com/sergeyzav/smartmem/backend/fake"
)

// E1: independent reads take one round and one Exec.
func TestE1_IndependentReadsOneRound(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1).U32(0x3000, 2).U32(0x5000, 3)
	b := e.s.NewBatch()
	a := smartmem.Read[uint32](b, e.p.At(0x1000))
	c := smartmem.Read[uint32](b, e.p.At(0x3000))
	d := smartmem.Read[uint32](b, e.p.At(0x5000))
	e.run(b)
	if get(t, a) != 1 || get(t, c) != 2 || get(t, d) != 3 {
		t.Fatal("wrong values")
	}
	e.assertTrace(`r1 pid=100 R 0x1000+4 R 0x3000+4 R 0x5000+4`)
}

// E2: the motivating case. Subsystem A reads X while subsystem B follows
// Y -> Z: X and Y share round 1, Z comes in round 2. Two scatter calls in all.
func TestE2_IndependentAndChainShareRounds(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 7)
	e.fp.U64(0x2000, 0x5000)
	e.fp.U32(0x5010, 42)
	b := e.s.NewBatch()
	x := smartmem.Read[uint32](b, e.p.At(0x1000))
	z := smartmem.Read[uint32](b, e.p.Chain(e.p.At(0x2000), 0x10))
	e.run(b)
	if get(t, x) != 7 || get(t, z) != 42 {
		t.Fatalf("x=%d z=%d", get(t, x), get(t, z))
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+4 R 0x2000+8
		r2 pid=100 R 0x5010+4`)
}

// E3: the same range requested three times is read once.
func TestE3_Dedup(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 9)
	b := e.s.NewBatch()
	fs := []*smartmem.Future[uint32]{
		smartmem.Read[uint32](b, e.p.At(0x1000)),
		smartmem.Read[uint32](b, e.p.At(0x1000)),
		smartmem.Read[uint32](b, e.p.At(0x1000)),
	}
	e.run(b)
	for _, f := range fs {
		if get(t, f) != 9 {
			t.Fatal("wrong value")
		}
	}
	e.assertTrace(`r1 pid=100 R 0x1000+4`)
}

// E4: coalescing merges reads on one page within MaxGap, and never across a
// page boundary.
func TestE4_Coalescing(t *testing.T) {
	cases := []struct {
		name  string
		addrs []uint64
		trace string
	}{
		{"gap within MaxGap", []uint64{0x1000, 0x1010}, "r1 pid=100 R 0x1000+20"},
		{"overlapping", []uint64{0x1000, 0x1002}, "r1 pid=100 R 0x1000+6"},
		{"gap above MaxGap", []uint64{0x1000, 0x1100}, "r1 pid=100 R 0x1000+4 R 0x1100+4"},
		{"adjacent pages", []uint64{0x1ffc, 0x2000}, "r1 pid=100 R 0x1ffc+4 R 0x2000+4"},
		{"read crossing a page", []uint64{0x1ff0, 0x1ffe}, "r1 pid=100 R 0x1ff0+4 R 0x1ffe+4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.fp.Map(0x1000, 2*fake.PageSize)
			b := e.s.NewBatch()
			for _, a := range tc.addrs {
				smartmem.Read[uint32](b, e.p.At(a))
			}
			e.run(b)
			e.assertTrace(tc.trace)
		})
	}
}

// E5: an unreadable page fails only the requests on it.
func TestE5_ErrorIsolation(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1).U32(0x3000, 3)
	b := e.s.NewBatch()
	ok1 := smartmem.Read[uint32](b, e.p.At(0x1000))
	bad := smartmem.Read[uint32](b, e.p.At(0x2000))
	ok3 := smartmem.Read[uint32](b, e.p.At(0x3000))
	e.run(b)
	if get(t, ok1) != 1 || get(t, ok3) != 3 {
		t.Fatal("readable requests failed")
	}
	err := getErr(t, bad)
	var re *smartmem.ReadError
	if !errors.As(err, &re) || re.Addr != 0x2000 || !errors.Is(err, fake.ErrUnmapped) {
		t.Fatalf("err=%v, want ReadError at 0x2000 wrapping ErrUnmapped", err)
	}
}

// E6: a short read is a PartialReadError with what was read.
func TestE6_PartialRead(t *testing.T) {
	e := newEnv(t)
	e.mem.SetPartialRead(fake.Truncate)
	e.fp.Map(0x1000, fake.PageSize)
	b := e.s.NewBatch()
	f := smartmem.Read[uint64](b, e.p.At(0x1ffc)) // 4 bytes mapped, 4 not
	e.run(b)
	var pe *smartmem.PartialReadError
	if err := getErr(t, f); !errors.As(err, &pe) || pe.Want != 8 || pe.Got != 4 || pe.Addr != 0x1ffc {
		t.Fatalf("err=%v, want PartialReadError{0x1ffc, 8, 4}", err)
	}
}

// E7: a cyclic list walked with Then stops at MaxRounds instead of looping.
func TestE7_MaxRounds(t *testing.T) {
	e := newEnv(t, smartmem.WithMaxRounds(8))
	e.fp.U64(0x1000, 0x2000).U64(0x2000, 0x1000) // A -> B -> A
	b := e.s.NewBatch()
	var walk func(s smartmem.Scope, next uint64) *smartmem.Future[uint64]
	walk = func(s smartmem.Scope, next uint64) *smartmem.Future[uint64] {
		return smartmem.Then(smartmem.Read[uint64](s, e.p.At(next)), walk)
	}
	f := walk(b, 0x1000)
	e.run(b)
	if err := getErr(t, f); !errors.Is(err, smartmem.ErrMaxRounds) {
		t.Fatalf("err=%v, want ErrMaxRounds", err)
	}
	if e.mem.Execs() != 8 {
		t.Fatalf("%d Execs, want 8", e.mem.Execs())
	}
}

// E7b: the default limit lets a long linked list finish, one round per element.
func TestE7b_DefaultMaxRoundsFitsLongList(t *testing.T) {
	const n = 1000
	e := newEnv(t)
	for i := range uint64(n) {
		next := 0x100000 + (i+1)*0x10
		if i == n-1 {
			next = 0
		}
		e.fp.U64(0x100000+i*0x10, next)
	}
	b := e.s.NewBatch()
	var walk func(s smartmem.Scope, addr uint64, count int) *smartmem.Future[int]
	walk = func(s smartmem.Scope, addr uint64, count int) *smartmem.Future[int] {
		return smartmem.Then(smartmem.Read[uint64](s, e.p.At(addr)), func(s smartmem.Scope, next uint64) *smartmem.Future[int] {
			if next == 0 {
				return smartmem.Resolved(s, count+1)
			}
			return walk(s, next, count+1)
		})
	}
	f := walk(b, 0x100000, 0)
	e.run(b)
	if got := get(t, f); got != n {
		t.Fatalf("walked %d elements, want %d", got, n)
	}
}

var errBoom = errors.New("boom")

// failingBackend fails every Exec of one round.
type failingBackend struct {
	*fake.Memory
	round, failRound int
}

func (f *failingBackend) BeginRound(r int) { f.round = r; f.Memory.BeginRound(r) }

func (f *failingBackend) Exec(ctx context.Context, pid uint32, flags uint64, w []backend.WriteOp, r []backend.ReadOp) error {
	if f.round == f.failRound {
		return errBoom
	}
	return f.Memory.Exec(ctx, pid, flags, w, r)
}

// E8: a fatal backend error ends the Run, and every pending Future gets it.
func TestE8_FatalBackendError(t *testing.T) {
	m := fake.New()
	m.Proc(pid).U64(0x1000, 0x2000).U64(0x2000, 0x3000).U32(0x3000, 1).U32(0x5000, 5)
	s := smartmem.New(&failingBackend{Memory: m, failRound: 2})
	p, _ := s.Process(pid)
	b := s.NewBatch()
	early := smartmem.Read[uint32](b, p.At(0x5000))
	inRound2 := smartmem.Read[uint32](b, p.Chain(p.At(0x1000), 0))
	queued := smartmem.Read[uint32](b, p.Chain(p.At(0x1000), 0, 0))
	then := smartmem.Then(early, func(s smartmem.Scope, _ uint32) *smartmem.Future[uint32] {
		return smartmem.Read[uint32](s, p.At(0x5000))
	})

	err := b.Run(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatalf("Run err=%v, want errBoom", err)
	}
	if v, err := early.Get(); err != nil || v != 5 {
		t.Errorf("round 1 result lost: %v, %v", v, err)
	}
	for name, f := range map[string]*smartmem.Future[uint32]{"inRound2": inRound2, "queued": queued, "then": then} {
		if _, err := f.Get(); !errors.Is(err, errBoom) {
			t.Errorf("%s: err=%v, want errBoom", name, err)
		}
	}
}

// E9: a context cancelled during round 1 stops the Run before round 2.
func TestE9_ContextCancel(t *testing.T) {
	e := newEnv(t)
	e.fp.U64(0x1000, 0x2000).U32(0x2000, 1).U32(0x3000, 3)
	ctx, cancel := context.WithCancel(e.ctx)
	b := e.s.NewBatch()
	f := smartmem.Read[uint32](b, e.p.Chain(e.p.At(0x1000), 0))
	first := smartmem.Map(smartmem.Read[uint32](b, e.p.At(0x3000)), func(v uint32) (uint32, error) {
		cancel()
		return v, nil
	})
	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v, want context.Canceled", err)
	}
	if get(t, first) != 3 {
		t.Fatal("round 1 result lost")
	}
	if _, err := f.Get(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Future err=%v, want context.Canceled", err)
	}
	e.assertTrace(`r1 pid=100 R 0x1000+8 R 0x3000+4`)
}

// E10: within a round, writes land before reads.
func TestE10_WritesBeforeReads(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	b := e.s.NewBatch()
	r := smartmem.Read[uint32](b, e.p.At(0x1000))
	w := smartmem.Write(b, e.p.At(0x1000), uint32(2))
	e.run(b)
	if _, err := w.Get(); err != nil {
		t.Fatal(err)
	}
	if get(t, r) != 2 {
		t.Fatalf("read %d, want the written 2", get(t, r))
	}
	e.assertTrace(`r1 pid=100 W 0x1000+4 R 0x1000+4`)
}

// E11: writes are never merged; the byte between them stays as it was.
func TestE11_WritesNotMerged(t *testing.T) {
	e := newEnv(t)
	e.fp.Bytes(0x1000, []byte{0, 0, 0, 0, 0xee, 0, 0, 0, 0})
	b := e.s.NewBatch()
	smartmem.Write(b, e.p.At(0x1000), uint32(0x11111111))
	smartmem.Write(b, e.p.At(0x1005), uint32(0x22222222))
	e.run(b)
	e.assertTrace(`r1 pid=100 W 0x1000+4 W 0x1005+4`)
	got, _ := e.fp.Read(0x1004, 1)
	if got[0] != 0xee {
		t.Fatalf("gap byte = %#x, want 0xee", got[0])
	}
}

// E12: a write through a chain of depth 2 lands in round 3.
func TestE12_WriteThroughChain(t *testing.T) {
	e := newEnv(t)
	e.fp.U64(0x1000, 0x2000).U64(0x2008, 0x3000).U32(0x3010, 0)
	b := e.s.NewBatch()
	w := smartmem.Write(b, e.p.Chain(e.p.At(0x1000), 8, 0x10), int32(-1))
	e.run(b)
	if _, err := w.Get(); err != nil {
		t.Fatal(err)
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+8
		r2 pid=100 R 0x2008+8
		r3 pid=100 W 0x3010+4`)
	if got, _ := e.fp.Read(0x3010, 4); got[0] != 0xff {
		t.Fatalf("memory = % x", got)
	}

	b = e.s.NewBatch()
	bad := smartmem.Write(b, e.p.At(0x9000), uint32(1))
	e.run(b)
	var we *smartmem.WriteError
	if err := getErr(t, bad); !errors.As(err, &we) || !errors.Is(err, fake.ErrUnmapped) {
		t.Fatalf("err=%v, want WriteError wrapping ErrUnmapped", err)
	}
}

// E13: requests to two processes give one Exec per process per round.
func TestE13_GroupByProcess(t *testing.T) {
	e := newEnv(t)
	e.fp.U64(0x1000, 0x2000).U32(0x2000, 1)
	e.mem.Proc(200).U32(0x1000, 2)
	p2, err := e.s.Process(200)
	if err != nil {
		t.Fatal(err)
	}
	b := e.s.NewBatch()
	a := smartmem.Read[uint32](b, e.p.Chain(e.p.At(0x1000), 0))
	c := smartmem.Read[uint32](b, p2.At(0x1000))
	e.run(b)
	if get(t, a) != 1 || get(t, c) != 2 {
		t.Fatal("wrong values")
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+8
		r1 pid=200 R 0x1000+4
		r2 pid=100 R 0x2000+4`)
}

// E14: the trace does not depend on the order requests were described in.
func TestE14_Deterministic(t *testing.T) {
	type req struct {
		base uint64
		offs []int64
	}
	reqs := []req{{0x1000, nil}, {0x1008, nil}, {0x2000, []int64{0x10}}, {0x2000, []int64{0x18, 0}}, {0x3ffe, nil}, {0x1000, nil}}
	var want string
	for i := range 100 {
		e := newEnv(t)
		e.fp.Map(0x1000, 3*fake.PageSize).U64(0x2000, 0x1100).U64(0x1118, 0x1200)
		order := rand.Perm(len(reqs))
		b := e.s.NewBatch()
		for _, j := range order {
			smartmem.Read[uint32](b, e.p.Chain(e.p.At(reqs[j].base), reqs[j].offs...))
		}
		e.run(b)
		if i == 0 {
			want = e.mem.Trace()
			continue
		}
		if got := e.mem.Trace(); got != want {
			t.Fatalf("order %v gave\n%s\nwant\n%s", order, got, want)
		}
	}
}

// E15: a request that Then creates in round k runs in round k+1, not k.
func TestE15_ThenRunsNextRound(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 0x30).U32(0x2030, 9).U32(0x2100, 1)
	b := e.s.NewBatch()
	f := smartmem.Then(smartmem.Read[uint32](b, e.p.At(0x1000)), func(s smartmem.Scope, off uint32) *smartmem.Future[uint32] {
		return smartmem.Read[uint32](s, e.p.At(0x2000+uint64(off)))
	})
	smartmem.Read[uint32](b, e.p.At(0x2100))
	e.run(b)
	if get(t, f) != 9 {
		t.Fatalf("got %d", get(t, f))
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+4 R 0x2100+4
		r2 pid=100 R 0x2030+4`)
}

// L1 on the trace: Chain and Deref/Add read the same addresses.
func TestL1_ChainTrace(t *testing.T) {
	var traces []string
	for _, explicit := range []bool{false, true} {
		e := newEnv(t)
		e.fp.U64(0x1000, 0x2000).U64(0x2030, 0x3000).U32(0x3090, 1)
		b := e.s.NewBatch()
		loc := e.p.Chain(e.p.At(0x1000), 0x30, 0x90)
		if explicit {
			loc = e.p.At(0x1000).Deref().Add(0x30).Deref().Add(0x90)
		}
		f := smartmem.Read[uint32](b, loc)
		e.run(b)
		if get(t, f) != 1 {
			t.Fatal("wrong value")
		}
		traces = append(traces, e.mem.Trace())
	}
	if traces[0] != traces[1] {
		t.Fatalf("Chain trace\n%s\n!= explicit trace\n%s", traces[0], traces[1])
	}
}

// L2: Cheat Engine semantics. Chain(B) is B; Chain(B, o) is [B]+o;
// Chain(B, o1, o2) is [[B]+o1]+o2.
func TestL2_CheatEngineSemantics(t *testing.T) {
	e := newEnv(t)
	e.fp.U64(0x1000, 0x2000)
	e.fp.U64(0x2030, 0x3000)
	e.fp.U32(0x3090, 111)
	b := e.s.NewBatch()
	b0 := smartmem.Read[uint64](b, e.p.Chain(e.p.At(0x1000)))
	b1 := smartmem.Read[uint64](b, e.p.Chain(e.p.At(0x1000), 0x30))
	b2 := smartmem.Read[uint32](b, e.p.Chain(e.p.At(0x1000), 0x30, 0x90))
	e.run(b)
	if v := get(t, b0); v != 0x2000 {
		t.Errorf("Chain(B) read %#x, want the value at B itself (0x2000)", v)
	}
	if v := get(t, b1); v != 0x3000 {
		t.Errorf("Chain(B, 0x30) read %#x, want the value at [B]+0x30 (0x3000)", v)
	}
	if v := get(t, b2); v != 111 {
		t.Errorf("Chain(B, 0x30, 0x90) read %d, want 111", v)
	}
}

// L3: a null pointer at step k fails with Step k, and later steps are not read.
func TestL3_NullPointer(t *testing.T) {
	e := newEnv(t)
	e.fp.U64(0x1000, 0x2000).U64(0x2008, 0)
	b := e.s.NewBatch()
	f := smartmem.Read[uint32](b, e.p.Chain(e.p.At(0x1000), 8, 0x10, 0x20))
	e.run(b)
	var ne *smartmem.NullPointerError
	if err := getErr(t, f); !errors.As(err, &ne) || ne.Step != 1 {
		t.Fatalf("err=%v, want NullPointerError{Step: 1}", err)
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+8
		r2 pid=100 R 0x2008+8`)
}

// L4: a kernel or non-canonical pointer is a BadPointerError.
func TestL4_BadPointer(t *testing.T) {
	e := newEnv(t)
	e.fp.U64(0x1000, 0xfffff80000001000)
	b := e.s.NewBatch()
	f := smartmem.Read[uint32](b, e.p.Chain(e.p.At(0x1000), 0))
	e.run(b)
	var be *smartmem.BadPointerError
	if err := getErr(t, f); !errors.As(err, &be) || be.Step != 0 || be.Value != 0xfffff80000001000 {
		t.Fatalf("err=%v, want BadPointerError", err)
	}
}

func TestModuleBase(t *testing.T) {
	e := newEnv(t)
	e.fp.Module("client.dll", 0x7ff600000000, 0x1000).U32(0x7ff600000010, 77)
	b := e.s.NewBatch()
	ok := smartmem.Read[uint32](b, e.p.Module("client.dll").Add(0x10))
	missing := smartmem.Read[uint32](b, e.p.Module("missing.dll"))
	e.run(b)
	if get(t, ok) != 77 {
		t.Fatal("wrong value")
	}
	var me *smartmem.ModuleError
	if err := getErr(t, missing); !errors.As(err, &me) {
		t.Fatalf("err=%v, want ModuleError", err)
	}
}

func TestRunMergesScopes(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1).U32(0x1010, 2)
	b1, b2 := e.s.NewBatch(), e.s.NewBatch()
	a := smartmem.Read[uint32](b1, e.p.At(0x1000))
	c := smartmem.Read[uint32](b2, e.p.At(0x1010))
	if err := smartmem.Run(e.ctx, b1, b2); err != nil {
		t.Fatal(err)
	}
	if get(t, a) != 1 || get(t, c) != 2 {
		t.Fatal("wrong values")
	}
	e.assertTrace(`r1 pid=100 R 0x1000+20`)
}

func TestBatchRunsOnce(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	b := e.s.NewBatch()
	smartmem.Read[uint32](b, e.p.At(0x1000))
	e.run(b)
	if err := b.Run(e.ctx); !errors.Is(err, smartmem.ErrBatchDone) {
		t.Fatalf("second Run err=%v, want ErrBatchDone", err)
	}
	mustPanic(t, "Read after Run", "already ran", func() { smartmem.Read[uint32](b, e.p.At(0x1000)) })

	// A done batch in a merged Run fails before anything runs.
	fresh := e.s.NewBatch()
	f := smartmem.Read[uint32](fresh, e.p.At(0x1000))
	if err := smartmem.Run(e.ctx, fresh, b); !errors.Is(err, smartmem.ErrBatchDone) {
		t.Fatalf("merged Run err=%v", err)
	}
	if err := fresh.Run(e.ctx); err != nil {
		t.Fatalf("fresh batch could not run after the failed merge: %v", err)
	}
	if get(t, f) != 1 {
		t.Fatal("wrong value")
	}
}

func TestReadNowWriteNow(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	if err := smartmem.WriteNow(e.ctx, e.p.At(0x1000), uint32(5)); err != nil {
		t.Fatal(err)
	}
	if v, err := smartmem.ReadNow[uint32](e.ctx, e.p.At(0x1000)); err != nil || v != 5 {
		t.Fatalf("ReadNow = %d, %v", v, err)
	}
	if err := smartmem.WriteNow(e.ctx, e.p.At(0x9000), uint32(5)); !errors.Is(err, fake.ErrUnmapped) {
		t.Fatalf("WriteNow to unmapped err=%v", err)
	}
}

func TestReadSlice(t *testing.T) {
	e := newEnv(t)
	for i := range 4 {
		e.fp.U32(0x1000+uint64(4*i), uint32(10+i))
	}
	b := e.s.NewBatch()
	f := smartmem.ReadSlice[uint32](b, e.p.At(0x1000), 4)
	empty := smartmem.ReadSlice[uint32](b, e.p.At(0x1000), 0)
	e.run(b)
	if got := fmt.Sprint(get(t, f)); got != "[10 11 12 13]" {
		t.Fatalf("got %s", got)
	}
	if v := get(t, empty); v == nil || len(v) != 0 {
		t.Fatalf("empty slice = %#v", v)
	}
	e.assertTrace(`r1 pid=100 R 0x1000+16`)
}
