package smartmem

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed is the error of requests to a Scheduler that was closed before
// they ran.
var ErrClosed = errors.New("smartmem: scheduler closed")

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*schedOptions)

type schedOptions struct {
	window time.Duration
	max    int
}

// FlushWindow sets how long the Scheduler waits after the first request of a
// flush for more requests to join it. The default is 500µs.
func FlushWindow(d time.Duration) SchedulerOption {
	return func(o *schedOptions) { o.window = max(0, d) }
}

// FlushMax flushes at once when this many requests are queued, without
// waiting for the window. The default is 4096.
func FlushMax(n int) SchedulerOption { return func(o *schedOptions) { o.max = max(1, n) } }

// Scheduler is a Scope that any goroutine can use. Requests from different
// subsystems that arrive within the flush window run together, sharing
// rounds; attached Plans join the flushes that are due for them.
//
// Wait on the Futures for results. Continuations (Then) must describe their
// requests in the Scope they are given: then they join the running flush.
// Requests described with the Scheduler itself go to the next flush.
type Scheduler struct {
	s    *Session
	g    graph // intake: requests from any goroutine
	eg   graph // continuations, used only by the loop goroutine
	opts schedOptions

	mu     sync.Mutex
	queue  []request
	spare  []request
	plans  []*attachedPlan
	closed bool

	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
}

type attachedPlan struct {
	p     *Plan
	every time.Duration
	next  time.Time
}

// NewScheduler starts a Scheduler. Close stops it.
func NewScheduler(s *Session, opts ...SchedulerOption) *Scheduler {
	sch := &Scheduler{
		s:    s,
		opts: schedOptions{window: 500 * time.Microsecond, max: 4096},
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	for _, o := range opts {
		o(&sch.opts)
	}
	sch.g = graph{s: s, kind: "Scheduler", submit: sch.submit, eng: &sch.eg}
	sch.eg = graph{s: s, kind: "Scheduler"}
	sch.ctx, sch.cancel = context.WithCancelCause(context.Background())
	go sch.loop()
	return sch
}

func (sch *Scheduler) scope() *graph { return &sch.g }

func (sch *Scheduler) submit(req request) {
	sch.mu.Lock()
	if sch.closed {
		sch.mu.Unlock()
		req.fail(ErrClosed)
		return
	}
	sch.queue = append(sch.queue, req)
	n := len(sch.queue)
	sch.mu.Unlock()
	if n == 1 || n >= sch.opts.max {
		sch.signal()
	}
}

func (sch *Scheduler) signal() {
	select {
	case sch.wake <- struct{}{}:
	default:
	}
}

// Attach runs p every interval, inside the Scheduler's flushes, so its rounds
// are shared with the requests queued at that time. The first run is due at
// once. Exec on an attached Plan panics; read it with View, Get or Next.
func (sch *Scheduler) Attach(p *Plan, every time.Duration) {
	if p.g.s != sch.s {
		panic("smartmem: Attach: the Plan belongs to another Session")
	}
	p.execMu.Lock()
	if p.attached {
		p.execMu.Unlock()
		panic("smartmem: Attach: the Plan is already attached")
	}
	p.attached = true
	p.execMu.Unlock()

	sch.mu.Lock()
	sch.plans = append(sch.plans, &attachedPlan{p: p, every: max(every, time.Microsecond), next: time.Now()})
	sch.mu.Unlock()
	sch.signal()
}

// Close stops the Scheduler. Queued requests and requests described later
// fail with ErrClosed; a running flush is cancelled. Attached Plans are
// detached. Close waits for the loop to stop.
func (sch *Scheduler) Close() {
	sch.mu.Lock()
	if sch.closed {
		sch.mu.Unlock()
		<-sch.done
		return
	}
	sch.closed = true
	queued := sch.queue
	sch.queue = nil
	sch.mu.Unlock()

	sch.cancel(ErrClosed)
	<-sch.done
	for _, req := range queued {
		req.fail(ErrClosed)
	}
	sch.mu.Lock()
	plans := sch.plans
	sch.plans = nil
	sch.mu.Unlock()
	for _, ap := range plans {
		ap.p.execMu.Lock()
		ap.p.attached = false
		ap.p.execMu.Unlock()
	}
}

func (sch *Scheduler) loop() {
	defer close(sch.done)
	planTimer := time.NewTimer(time.Hour)
	planTimer.Stop()
	defer planTimer.Stop()
	for {
		var planC <-chan time.Time
		if d, ok := sch.untilNextPlan(); ok {
			planTimer.Reset(d)
			planC = planTimer.C
		}
		select {
		case <-sch.ctx.Done():
			return
		case <-sch.wake:
			if !sch.waitWindow() {
				return
			}
		case <-planC:
		}
		planTimer.Stop()
		sch.flush()
	}
}

// waitWindow lets more requests join until the window ends or the queue is
// full. It returns false if the Scheduler was closed.
func (sch *Scheduler) waitWindow() bool {
	if sch.queued() == 0 || sch.queued() >= sch.opts.max || sch.opts.window == 0 {
		return sch.ctx.Err() == nil
	}
	t := time.NewTimer(sch.opts.window)
	defer t.Stop()
	for {
		select {
		case <-sch.ctx.Done():
			return false
		case <-t.C:
			return true
		case <-sch.wake:
			if sch.queued() >= sch.opts.max {
				return true
			}
		}
	}
}

func (sch *Scheduler) queued() int {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	return len(sch.queue)
}

func (sch *Scheduler) untilNextPlan() (time.Duration, bool) {
	sch.mu.Lock()
	defer sch.mu.Unlock()
	if len(sch.plans) == 0 {
		return 0, false
	}
	next := sch.plans[0].next
	for _, ap := range sch.plans[1:] {
		if ap.next.Before(next) {
			next = ap.next
		}
	}
	return max(0, time.Until(next)), true
}

func (sch *Scheduler) flush() {
	now := time.Now()
	sch.mu.Lock()
	reqs := sch.queue
	sch.queue = sch.spare[:0]
	var due []*Plan
	for _, ap := range sch.plans {
		if !ap.next.After(now) {
			due = append(due, ap.p)
			ap.next = ap.next.Add(ap.every)
			if ap.next.Before(now) { // fell behind: skip missed ticks
				ap.next = now.Add(ap.every)
			}
		}
	}
	sch.mu.Unlock()
	if len(reqs) == 0 && len(due) == 0 {
		return
	}

	r := sch.s.getRun()
	sch.eg.reqs = reqs
	graphs := append(make([]*graph, 0, 1+len(due)), &sch.eg)
	rs := append(make([]Runnable, 0, 1+len(due)), flushRunnable{})
	for _, p := range due {
		graphs = append(graphs, p.prepare())
		rs = append(rs, p)
	}
	_ = executeWith(sch.ctx, r, graphs, rs)
	sch.s.putRun(r)

	clear(reqs)
	sch.eg.reqs = nil
	sch.eg.frozen = false
	sch.mu.Lock()
	sch.spare = reqs[:0]
	sch.mu.Unlock()
}

// flushRunnable stands for the Scheduler's own graph in executeWith.
type flushRunnable struct{}

func (flushRunnable) check() error    { return nil }
func (flushRunnable) prepare() *graph { return nil }
func (flushRunnable) finish()         {}
