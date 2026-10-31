package smartmem

import (
	"context"
	"sync"

	"github.com/sergeyzav/smartmem/internal/engine"
)

// Plan is a reusable set of requests for polling: describe them once, then
// call Exec as often as needed. Nodes, buffers and decoded values are reused,
// so a Plan of fixed-size values, structs and unchanged strings runs without
// allocating. ReadSlice, LoadEach and All allocate one result slice per Exec,
// so slices handed out earlier never change under the caller.
//
// Results of one Exec are published together: values read inside View all
// come from the same Exec. Describing requests is not safe for concurrent
// use; Exec, View and the Futures are.
type Plan struct {
	g      graph
	execMu sync.Mutex
	run    *engine.Run
	gen    uint64 // guarded by g.pub

	nmu    sync.Mutex
	notify chan struct{}

	self  [1]*graph
	selfR [1]Runnable
}

// NewPlan returns an empty Plan.
func (s *Session) NewPlan() *Plan {
	p := &Plan{}
	p.g = graph{s: s, kind: "Plan", pub: newPublishLock(), plan: p}
	p.self[0], p.selfR[0] = &p.g, p
	return p
}

func (p *Plan) scope() *graph { return &p.g }

// Exec runs the Plan once and publishes its results. The first Exec freezes
// the Plan: describing more requests afterwards panics. Concurrent Execs run
// one after another.
func (p *Plan) Exec(ctx context.Context) error {
	if err := p.check(); err != nil {
		return err
	}
	p.prepare()
	opts := p.g.s.engineOptions()
	if p.run == nil {
		p.run = engine.NewRun(p.g.s.be, opts)
	} else {
		p.run.Reset(p.g.s.be, opts)
	}
	return executeWith(ctx, p.run, p.self[:], p.selfR[:])
}

// View runs fn while no Exec can publish, so all Futures read in fn hold
// values of the same Exec. Keep fn short: a pending publish waits for it.
func (p *Plan) View(fn func()) {
	p.g.pub.RLock()
	defer p.g.pub.RUnlock()
	fn()
}

// Gen returns the number of Execs published so far.
func (p *Plan) Gen() uint64 {
	p.g.pub.RLock()
	defer p.g.pub.RUnlock()
	return p.gen
}

func (p *Plan) check() error { return nil }

func (p *Plan) prepare() *graph {
	p.execMu.Lock()
	return &p.g
}

func (p *Plan) finish() {
	p.g.pub.Lock()
	p.gen++
	p.g.gen = p.gen
	for _, c := range p.g.commit {
		c.commit()
	}
	p.g.pub.Unlock()

	p.nmu.Lock()
	if p.notify != nil {
		close(p.notify)
		p.notify = nil
	}
	p.nmu.Unlock()
	p.execMu.Unlock()
}

func (p *Plan) updated() <-chan struct{} {
	p.nmu.Lock()
	defer p.nmu.Unlock()
	if p.notify == nil {
		p.notify = make(chan struct{})
	}
	return p.notify
}
