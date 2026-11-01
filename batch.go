package smartmem

import (
	"context"
	"sync"

	"github.com/sergeyzav/smartmem/internal/engine"
)

// Batch is a one-shot set of requests: describe them, call Run once, then
// read the Futures. Describing requests is not safe for concurrent use.
type Batch struct {
	g    graph
	mu   sync.Mutex
	done bool
}

// NewBatch returns an empty Batch.
func (s *Session) NewBatch() *Batch {
	return &Batch{g: graph{s: s, kind: "Batch"}}
}

func (b *Batch) scope() *graph { return &b.g }

// Run executes the Batch. It returns an error only if ctx is done or the
// backend failed as a whole; errors of single requests are in their Futures.
// A second call returns ErrBatchDone.
func (b *Batch) Run(ctx context.Context) error { return Run(ctx, b) }

func (b *Batch) check() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return ErrBatchDone
	}
	return nil
}

func (b *Batch) prepare() *graph {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
	return &b.g
}

func (b *Batch) finish() {}

// Runnable is something Run can execute: a *Batch or a *Plan. A Plan run
// through Run is published like after Exec.
type Runnable interface {
	check() error    // without side effects
	prepare() *graph // after every check passed
	finish()
}

// Run executes several Batches and Plans together: their requests share
// rounds, so independent requests of different subsystems go into the same
// scatter operation. All of them must come from the same Session.
func Run(ctx context.Context, rs ...Runnable) error {
	if len(rs) == 0 {
		return nil
	}
	for _, x := range rs {
		if err := x.check(); err != nil {
			return err
		}
	}
	for i := range rs {
		for j := range i {
			if rs[i] == rs[j] {
				panic("smartmem: Run: the same scope is passed twice")
			}
		}
	}
	graphs := make([]*graph, 0, len(rs))
	for _, x := range rs {
		g := x.prepare()
		if len(graphs) > 0 && g.s != graphs[0].s {
			panic("smartmem: Run: scopes come from different Sessions")
		}
		graphs = append(graphs, g)
	}
	s := graphs[0].s
	r := s.getRun()
	defer s.putRun(r)
	return executeWith(ctx, r, graphs, rs)
}

func executeWith(ctx context.Context, r *engine.Run, graphs []*graph, rs []Runnable) error {
	for _, g := range graphs {
		g.begin(r)
	}
	err := r.Execute(ctx)
	for _, g := range graphs {
		g.end()
	}
	for _, x := range rs {
		x.finish()
	}
	return err
}
