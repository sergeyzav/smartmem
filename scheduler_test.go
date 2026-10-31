package smartmem_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

// All Scheduler tests run in a synctest bubble: time is virtual, timers fire
// only when every goroutine is blocked, and a goroutine left running at the
// end fails the test.

// S1: the motivating case across goroutines. Subsystem A reads X, subsystem B
// follows Y -> Z, each from its own goroutine; both land in one flush and
// share round 1.
func TestS1_SubsystemsShareRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.fp.U32(0x1000, 7).U64(0x2000, 0x5000).U32(0x5010, 42)
		sch := smartmem.NewScheduler(e.s, smartmem.FlushWindow(time.Millisecond))
		defer sch.Close()

		var wg sync.WaitGroup
		var x, z uint32
		var errX, errZ error
		wg.Add(2)
		go func() {
			defer wg.Done()
			x, errX = smartmem.Read[uint32](sch, e.p.At(0x1000)).Wait(e.ctx)
		}()
		go func() {
			defer wg.Done()
			z, errZ = smartmem.Read[uint32](sch, e.p.Chain(e.p.At(0x2000), 0x10)).Wait(e.ctx)
		}()
		wg.Wait()
		if errX != nil || errZ != nil || x != 7 || z != 42 {
			t.Fatalf("x=%d %v, z=%d %v", x, errX, z, errZ)
		}
		e.assertTrace(`
			r1 pid=100 R 0x1000+4 R 0x2000+8
			r2 pid=100 R 0x5010+4`)
	})
}

// S2: a request after the window has closed goes into a flush of its own.
func TestS2_LateRequestNextFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.fp.U32(0x1000, 1).U32(0x3000, 3)
		sch := smartmem.NewScheduler(e.s, smartmem.FlushWindow(time.Millisecond))
		defer sch.Close()

		start := time.Now()
		if _, err := smartmem.Read[uint32](sch, e.p.At(0x1000)).Wait(e.ctx); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != time.Millisecond {
			t.Fatalf("the first flush ran after %v, want the 1ms window", waited)
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := smartmem.Read[uint32](sch, e.p.At(0x3000)).Wait(e.ctx); err != nil {
			t.Fatal(err)
		}
		e.assertTrace(`
			r1 pid=100 R 0x1000+4
			r1 pid=100 R 0x3000+4`)
	})
}

// S3: FlushMax flushes at once, without waiting for the window.
func TestS3_FlushMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.fp.U32(0x1000, 1).U32(0x3000, 3)
		sch := smartmem.NewScheduler(e.s, smartmem.FlushWindow(time.Hour), smartmem.FlushMax(2))
		defer sch.Close()

		start := time.Now()
		a := smartmem.Read[uint32](sch, e.p.At(0x1000))
		synctest.Wait() // the loop now waits for the 1h window
		b := smartmem.Read[uint32](sch, e.p.At(0x3000))
		if _, err := a.Wait(e.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Wait(e.ctx); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("flushed after %v, want at once", waited)
		}
		e.assertTrace(`r1 pid=100 R 0x1000+4 R 0x3000+4`)
	})
}

// S4: an attached Plan and an ad-hoc request share the rounds of one flush.
func TestS4_AttachedPlanJoinsFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.fp.U32(0x1000, 1).U32(0x1010, 2)
		plan := e.s.NewPlan()
		pf := smartmem.Read[uint32](plan, e.p.At(0x1000))
		sch := smartmem.NewScheduler(e.s, smartmem.FlushWindow(time.Millisecond))
		defer sch.Close()

		adhoc := smartmem.Read[uint32](sch, e.p.At(0x1010))
		sch.Attach(plan, 10*time.Millisecond)
		if v, err := adhoc.Wait(e.ctx); err != nil || v != 2 {
			t.Fatalf("adhoc = %d, %v", v, err)
		}
		if v, gen, err := pf.Next(e.ctx, 0); err != nil || v != 1 || gen != 1 {
			t.Fatalf("plan = %d gen %d, %v", v, gen, err)
		}
		e.assertTrace(`r1 pid=100 R 0x1000+20`)

		e.fp.U32(0x1000, 5)
		if v, gen, err := pf.Next(e.ctx, 1); err != nil || v != 5 || gen != 2 {
			t.Fatalf("second tick = %d gen %d, %v", v, gen, err)
		}
		mustPanic(t, "Exec attached", "attached", func() { _ = plan.Exec(e.ctx) })
		if err := smartmem.Run(e.ctx, plan); err == nil {
			t.Fatal("Run on an attached Plan did not fail")
		}
	})
}

// S5: Close fails queued requests and later ones with ErrClosed, and stops
// the loop goroutine (synctest fails the test if it leaks).
func TestS5_Close(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.fp.U32(0x1000, 1)
		sch := smartmem.NewScheduler(e.s, smartmem.FlushWindow(time.Hour))
		plan := e.s.NewPlan()
		smartmem.Read[uint32](plan, e.p.At(0x1000))
		sch.Attach(plan, time.Hour)
		synctest.Wait()

		queued := smartmem.Read[uint32](sch, e.p.At(0x1000))
		synctest.Wait()
		sch.Close()
		sch.Close()
		if _, err := queued.Get(); !errors.Is(err, smartmem.ErrClosed) {
			t.Fatalf("queued: err=%v, want ErrClosed", err)
		}
		after := smartmem.Read[uint32](sch, e.p.At(0x1000))
		if _, err := after.Get(); !errors.Is(err, smartmem.ErrClosed) {
			t.Fatalf("after Close: err=%v, want ErrClosed", err)
		}
		if err := plan.Exec(context.Background()); err != nil {
			t.Fatalf("Close did not detach the Plan: %v", err)
		}
	})
}

// S6: a request that arrives while a flush runs is not lost; it runs in the
// next flush. A Then continuation, on the other hand, joins the running one.
func TestS6_RequestDuringFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.fp.U32(0x1000, 1).U32(0x3000, 3).U32(0x5000, 0x10).U32(0x6010, 6)
		sch := smartmem.NewScheduler(e.s, smartmem.FlushWindow(time.Millisecond))
		defer sch.Close()

		var late *smartmem.Future[uint32]
		e.mem.OnRound(func(round int, _ *fake.Memory) {
			if round == 1 && late == nil {
				late = smartmem.Read[uint32](sch, e.p.At(0x3000))
			}
		})
		first := smartmem.Read[uint32](sch, e.p.At(0x1000))
		dependent := smartmem.Then(smartmem.Read[uint32](sch, e.p.At(0x5000)), func(s smartmem.Scope, off uint32) *smartmem.Future[uint32] {
			return smartmem.Read[uint32](s, e.p.At(0x6000+uint64(off)))
		})
		if _, err := first.Wait(e.ctx); err != nil {
			t.Fatal(err)
		}
		if v, err := dependent.Wait(e.ctx); err != nil || v != 6 {
			t.Fatalf("dependent = %d, %v", v, err)
		}
		if v, err := late.Wait(e.ctx); err != nil || v != 3 {
			t.Fatalf("late = %d, %v", v, err)
		}
		e.assertTrace(`
			r1 pid=100 R 0x1000+4 R 0x5000+4
			r2 pid=100 R 0x6010+4
			r1 pid=100 R 0x3000+4`)
	})
}

// Concurrent goroutines describing requests and continuations on one
// Scheduler is safe (run with -race).
func TestSchedulerConcurrentUse(t *testing.T) {
	e := newEnv(t)
	for i := range 16 {
		e.fp.U32(0x1000+uint64(i)*4, uint32(i))
	}
	sch := smartmem.NewScheduler(e.s)
	defer sch.Close()
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				f := smartmem.Read[uint32](sch, e.p.At(0x1000+uint64(g)*4))
				m := smartmem.Map(f, func(v uint32) (uint32, error) { return v + 1, nil })
				if v, err := m.Wait(e.ctx); err != nil || v != uint32(g)+1 {
					t.Errorf("goroutine %d: %d, %v", g, v, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
