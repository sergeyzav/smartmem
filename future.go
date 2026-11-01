package smartmem

import (
	"context"
	"sync"
	"sync/atomic"
)

// Future holds the result of a request. It is filled when the request's
// round runs. Get never blocks; Wait and Done let other goroutines wait.
// A Future is safe for concurrent use.
type Future[T any] struct {
	g     *graph
	state atomic.Uint32 // 0: pending, 1: has a value
	val   T
	err   error
	gen   uint64

	// staged is the result of the current execution. In a Batch it is
	// committed at once; a Plan commits all Futures together.
	sv     T
	serr   error
	staged bool
	subs   []func(T, error)

	mu     sync.Mutex
	done   chan struct{}
	closed bool
}

var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func newFuture[T any](g *graph) *Future[T] {
	f := &Future[T]{g: g}
	g.track(f)
	return f
}

// Get returns the value without blocking. Before the value is ready it
// returns ErrPending.
func (f *Future[T]) Get() (T, error) {
	if l := f.g.pub; l != nil {
		l.RLock()
		defer l.RUnlock()
	}
	if f.state.Load() == 0 {
		var zero T
		return zero, ErrPending
	}
	return f.val, f.err
}

// Gen returns the execution the current value comes from: for a Plan, the
// Plan.Gen of the Exec that published it; for other scopes, 1 once ready.
// It is 0 before the first value.
func (f *Future[T]) Gen() uint64 {
	if l := f.g.pub; l != nil {
		l.RLock()
		defer l.RUnlock()
	}
	if f.state.Load() == 0 {
		return 0
	}
	return f.gen
}

// Next waits for a value newer than generation after and returns it with its
// generation. With after = 0 it waits for the first value. It is meant for
// Plans: a subscriber keeps the generation it saw and calls Next again.
func (f *Future[T]) Next(ctx context.Context, after uint64) (T, uint64, error) {
	for {
		var ch <-chan struct{}
		if p := f.g.plan; p != nil {
			ch = p.updated() // taken before the check, so no publish is missed
		} else {
			ch = f.Done()
		}
		v, gen, err := f.snapshot()
		if gen > after {
			return v, gen, err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			var zero T
			return zero, after, ctx.Err()
		}
	}
}

func (f *Future[T]) snapshot() (v T, gen uint64, err error) {
	if l := f.g.pub; l != nil {
		l.RLock()
		defer l.RUnlock()
	}
	if f.state.Load() == 0 {
		return v, 0, nil
	}
	return f.val, f.gen, f.err
}

// Ready reports whether the value is ready.
func (f *Future[T]) Ready() bool { return f.state.Load() != 0 }

// Done returns a channel that is closed when the value is ready.
func (f *Future[T]) Done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedChan
	}
	if f.done == nil {
		f.done = make(chan struct{})
	}
	return f.done
}

// Wait blocks until the value is ready or ctx is done.
func (f *Future[T]) Wait(ctx context.Context) (T, error) {
	select {
	case <-f.Done():
		return f.Get()
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// resolve stages the result and runs the continuations. It runs on the
// engine goroutine.
func (f *Future[T]) resolve(v T, err error) {
	f.sv = v
	f.resolveStaged(err)
}

func (f *Future[T]) resolveStaged(err error) {
	if f.staged {
		return
	}
	if err != nil {
		var zero T
		f.sv = zero
	}
	f.serr, f.staged = err, true
	if f.g.pub == nil {
		f.commit()
	}
	for _, fn := range f.subs {
		fn(f.sv, f.serr)
	}
}

// commit publishes the staged result. In a Plan the caller holds the publish lock.
func (f *Future[T]) commit() {
	if !f.staged {
		return
	}
	f.val, f.err = f.sv, f.serr
	f.gen = max(f.g.gen, 1)
	f.state.Store(1)
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		if f.done != nil {
			close(f.done)
		}
	}
	f.mu.Unlock()
}

func (f *Future[T]) reset() {
	var zero T
	f.sv, f.serr, f.staged = zero, nil, false
}

// subscribe runs fn with the result: now if it is staged, else when it is.
func (f *Future[T]) subscribe(fn func(T, error)) {
	if f.staged {
		fn(f.sv, f.serr)
		return
	}
	f.g.mustDescribe("a continuation")
	f.subs = append(f.subs, fn)
}

// Map returns a Future with fn applied to f's value. An error from f or from
// fn becomes the error of the result. fn runs on the engine goroutine between
// rounds, so it must be fast and must not block.
func Map[T, U any](f *Future[T], fn func(T) (U, error)) *Future[U] {
	out := newFuture[U](f.g)
	f.subscribe(func(v T, err error) {
		if err != nil {
			var zero U
			out.resolve(zero, err)
			return
		}
		out.resolve(fn(v))
	})
	return out
}

// Then runs a dependent request: when f is ready, fn describes new requests
// in s, and their result becomes the result of Then. The new requests run in
// the next round of the same execution. fn must return a Future it created in
// s, or nil for the zero value. Like Map, fn runs between rounds and must not
// block.
func Then[T, U any](f *Future[T], fn func(s Scope, v T) *Future[U]) *Future[U] {
	out := newFuture[U](f.g)
	f.subscribe(func(v T, err error) {
		var zero U
		if err != nil {
			out.resolve(zero, err)
			return
		}
		inner := fn(f.g, v)
		if inner == nil {
			out.resolve(zero, nil)
			return
		}
		inner.subscribe(out.resolve)
	})
	return out
}

// All returns a Future with the values of fs in order. It fails with the first
// error, in the order of fs. All Futures must come from the same scope.
func All[T any](fs ...*Future[T]) *Future[[]T] {
	if len(fs) == 0 {
		panic("smartmem: All needs at least one Future")
	}
	g := fs[0].g
	a := &allState[T]{out: newFuture[[]T](g), fs: fs, vals: make([]T, len(fs)), pending: len(fs)}
	g.trackResetter(a)
	for i, f := range fs {
		if f.g != g {
			panic("smartmem: All: Futures come from different scopes")
		}
		f.subscribe(func(v T, err error) { a.done(i, v, err) })
	}
	return a.out
}

type allState[T any] struct {
	out     *Future[[]T]
	fs      []*Future[T]
	vals    []T
	pending int
}

func (a *allState[T]) reset() { a.pending = len(a.fs) }

func (a *allState[T]) done(i int, v T, _ error) {
	a.vals[i] = v
	a.pending--
	if a.pending > 0 {
		return
	}
	for _, f := range a.fs {
		if f.serr != nil {
			a.out.resolve(nil, f.serr)
			return
		}
	}
	a.out.resolve(append([]T(nil), a.vals...), nil)
}
