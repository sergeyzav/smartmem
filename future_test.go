package smartmem_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/internal/testutil"
)

// F1: Get returns ErrPending before the run and the value after it.
func TestF1_GetBeforeAndAfter(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 4)
	b := e.s.NewBatch()
	f := smartmem.Read[uint32](b, e.p.At(0x1000))
	if _, err := f.Get(); !errors.Is(err, smartmem.ErrPending) || f.Ready() {
		t.Fatalf("before Run: err=%v ready=%v", err, f.Ready())
	}
	e.run(b)
	if get(t, f) != 4 || !f.Ready() {
		t.Fatal("after Run: no value")
	}
}

// F2: Done on a ready Future does not allocate.
func TestF2_DoneAfterResolveDoesNotAllocate(t *testing.T) {
	if testutil.RaceEnabled {
		t.Skip("the race detector allocates")
	}
	e := newEnv(t)
	e.fp.U32(0x1000, 4)
	b := e.s.NewBatch()
	f := smartmem.Read[uint32](b, e.p.At(0x1000))
	e.run(b)
	if allocs := testing.AllocsPerRun(100, func() { <-f.Done() }); allocs != 0 {
		t.Fatalf("Done allocates %.1f times", allocs)
	}
}

// F3: many goroutines waiting on one Future are all released.
func TestF3_ManyWaiters(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 4)
	b := e.s.NewBatch()
	f := smartmem.Read[uint32](b, e.p.At(0x1000))
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := f.Wait(e.ctx); err != nil || v != 4 {
				errs <- fmt.Errorf("Wait = %d, %w", v, err)
			}
		}()
	}
	e.run(b)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// F4: Wait returns when its context is done, even if the value never comes.
func TestF4_WaitHonoursContext(t *testing.T) {
	e := newEnv(t)
	f := smartmem.Read[uint32](e.s.NewBatch(), e.p.At(0x1000))
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := f.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want DeadlineExceeded", err)
	}
}

// F5: Map and Then pass errors on; a count followed by an array takes two rounds.
func TestF5_MapThen(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 3)
	e.fp.U32(0x2000, 7).U32(0x2004, 8).U32(0x2008, 9)
	b := e.s.NewBatch()
	count := smartmem.Read[uint32](b, e.p.At(0x1000))
	items := smartmem.Then(count, func(s smartmem.Scope, n uint32) *smartmem.Future[[]uint32] {
		return smartmem.ReadSlice[uint32](s, e.p.At(0x2000), int(n))
	})
	doubled := smartmem.Map(count, func(n uint32) (uint32, error) { return 2 * n, nil })
	failing := smartmem.Map(count, func(uint32) (uint32, error) { return 0, errBoom })
	broken := smartmem.Read[uint32](b, e.p.At(0x9000))
	mapped := smartmem.Map(broken, func(v uint32) (uint32, error) { return v, nil })
	thened := smartmem.Then(broken, func(s smartmem.Scope, _ uint32) *smartmem.Future[uint32] {
		t.Error("Then callback ran after an error")
		return nil
	})
	all := smartmem.All(count, doubled)
	e.run(b)

	if got := fmt.Sprint(get(t, items)); got != "[7 8 9]" {
		t.Errorf("items = %s", got)
	}
	if get(t, doubled) != 6 {
		t.Errorf("doubled = %d", get(t, doubled))
	}
	if got := fmt.Sprint(get(t, all)); got != "[3 6]" {
		t.Errorf("All = %s", got)
	}
	if !errors.Is(getErr(t, failing), errBoom) {
		t.Error("Map error lost")
	}
	readErr := getErr(t, broken)
	if !errors.Is(getErr(t, mapped), readErr) && getErr(t, mapped).Error() != readErr.Error() {
		t.Error("Map did not pass the read error on")
	}
	if getErr(t, thened).Error() != readErr.Error() {
		t.Error("Then did not pass the read error on")
	}
	e.assertTrace(`
		r1 pid=100 R 0x1000+4 R 0x9000+4
		r2 pid=100 R 0x2000+12`)
}

// F6: Done and Wait racing with the run never miss the result.
func TestF6_DoneRacesResolve(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 4)
	for range 2000 {
		b := e.s.NewBatch()
		f := smartmem.Read[uint32](b, e.p.At(0x1000))
		got := make(chan uint32)
		go func() {
			<-f.Done()
			v, _ := f.Get()
			got <- v
		}()
		go func() { _ = b.Run(context.Background()) }()
		select {
		case v := <-got:
			if v != 4 {
				t.Fatalf("got %d", v)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Done was never closed")
		}
	}
}

func TestAllErrors(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	b := e.s.NewBatch()
	ok := smartmem.Read[uint32](b, e.p.At(0x1000))
	bad := smartmem.Read[uint32](b, e.p.At(0x9000))
	all := smartmem.All(ok, bad)
	e.run(b)
	if getErr(t, all).Error() != getErr(t, bad).Error() {
		t.Fatal("All did not fail with the first error")
	}
	mustPanic(t, "All()", "at least one", func() { smartmem.All[uint32]() })
	mustPanic(t, "All mixed", "different scopes", func() {
		smartmem.All(smartmem.Read[uint32](e.s.NewBatch(), e.p.At(0)), smartmem.Read[uint32](e.s.NewBatch(), e.p.At(0)))
	})
}

// F9: Resolved and Failed are ready after Run without any I/O.
func TestF9_ResolvedAndFailed(t *testing.T) {
	e := newEnv(t)
	b := e.s.NewBatch()
	v := smartmem.Resolved(b, 7)
	boom := errors.New("boom")
	f := smartmem.Failed[string](b, boom)
	e.run(b)
	if get(t, v) != 7 {
		t.Fatal("Resolved: wrong value")
	}
	if _, err := f.Get(); !errors.Is(err, boom) {
		t.Fatalf("Failed: err=%v", err)
	}
	if n := e.mem.Execs(); n != 0 {
		t.Fatalf("%d Execs, want 0", n)
	}
}

// F10: ending a Then with Resolved or Failed costs no extra round.
func TestF10_ResolvedEndsThen(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 4).U32(0x2000, 0)
	b := e.s.NewBatch()
	double := func(loc smartmem.Loc) *smartmem.Future[uint32] {
		return smartmem.Then(smartmem.Read[uint32](b, loc), func(s smartmem.Scope, v uint32) *smartmem.Future[uint32] {
			if v == 0 {
				return smartmem.Failed[uint32](s, errors.New("zero"))
			}
			return smartmem.Resolved(s, v*2)
		})
	}
	ok, bad := double(e.p.At(0x1000)), double(e.p.At(0x2000))
	e.run(b)
	if get(t, ok) != 8 {
		t.Fatal("wrong value")
	}
	if _, err := bad.Get(); err == nil || err.Error() != "zero" {
		t.Fatalf("err=%v", err)
	}
	if n := e.mem.Execs(); n != 1 {
		t.Fatalf("%d Execs, want 1", n)
	}
}

// F11: in a Plan, Resolved inside Then follows the value of every Exec.
func TestF11_ResolvedInPlan(t *testing.T) {
	e := newEnv(t)
	e.fp.U32(0x1000, 1)
	p := e.s.NewPlan()
	c := smartmem.Resolved(p, "const")
	f := smartmem.Then(smartmem.Read[uint32](p, e.p.At(0x1000)), func(s smartmem.Scope, v uint32) *smartmem.Future[string] {
		return smartmem.Resolved(s, fmt.Sprint(v))
	})
	for _, want := range []uint32{1, 2, 3} {
		e.fp.U32(0x1000, want)
		if err := p.Exec(e.ctx); err != nil {
			t.Fatal(err)
		}
		if got := get(t, f); got != fmt.Sprint(want) {
			t.Fatalf("got %q, want %d", got, want)
		}
		if get(t, c) != "const" {
			t.Fatal("constant lost")
		}
	}
}

// F12: Resolved works from any goroutine through a Scheduler.
func TestF12_ResolvedInScheduler(t *testing.T) {
	e := newEnv(t)
	sch := smartmem.NewScheduler(e.s)
	defer sch.Close()
	v, err := smartmem.Resolved(sch, 5).Wait(e.ctx)
	if err != nil || v != 5 {
		t.Fatalf("got %v, %v", v, err)
	}
}
