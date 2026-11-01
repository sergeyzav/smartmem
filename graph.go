package smartmem

import (
	"github.com/sergeyzav/smartmem/internal/engine"
)

// Scope is where requests are described: a *Batch, a *Plan, a *Scheduler, a
// *Loader, or the scope Then hands to its callback.
type Scope interface {
	scope() *graph
}

// graph holds the requests of one scope.
type graph struct {
	s      *Session
	reqs   []request
	run    *engine.Run
	pub    *publishLock
	frozen bool // a Plan after its first Exec, a Batch after Run
	resets []resetter
	commit []committer
	kind   string
}

func (g *graph) scope() *graph { return g }

// request is something that puts nodes into a Run.
type request interface {
	schedule(r *engine.Run)
}

type resetter interface{ reset() }

type committer interface {
	resetter
	commit()
}

// add registers a request, or schedules it at once during an execution.
func (g *graph) add(req request) {
	if g.run != nil {
		req.schedule(g.run)
		return
	}
	g.mustDescribe("a request")
	g.reqs = append(g.reqs, req)
}

func (g *graph) mustDescribe(what string) {
	if g.frozen && g.run == nil {
		panic("smartmem: cannot add " + what + " to a " + g.kind + " that already ran")
	}
}

// track registers a Future for the Plan's reset and commit steps. Futures
// made during an execution are internal to it and are not tracked.
func (g *graph) track(c committer) {
	if g.run != nil {
		return
	}
	g.mustDescribe("a Future")
	g.commit = append(g.commit, c)
}

func (g *graph) trackResetter(r resetter) {
	if g.run == nil {
		g.resets = append(g.resets, r)
	}
}

func (g *graph) begin(r *engine.Run) {
	for _, c := range g.commit {
		c.reset()
	}
	for _, x := range g.resets {
		x.reset()
	}
	g.frozen = true
	g.run = r
	for _, req := range g.reqs {
		req.schedule(r)
	}
}

func (g *graph) end() { g.run = nil }

func (g *graph) checkLoc(loc Loc) {
	loc.mustBeValid()
	if loc.proc.s != g.s {
		panic("smartmem: the Loc belongs to another Session")
	}
}

// chainReq is a Loc compiled into a chain of nodes that ends in tail.
type chainReq struct {
	loc  Loc
	head *engine.Node
}

func (g *graph) newChain(loc Loc, tail *engine.Node) *chainReq {
	g.checkLoc(loc)
	pid, flags := loc.proc.pid, g.s.opts.flags
	tail.PID, tail.Flags = pid, flags
	next := tail
	for k := len(loc.offs) - 2; k >= 0; k-- {
		next = &engine.Node{
			Kind: engine.Deref, PID: pid, Flags: flags, Size: engine.PtrSize,
			Off: loc.offs[k+1], Step: k, Next: next,
		}
	}
	return &chainReq{loc: loc, head: next}
}

func (c *chainReq) schedule(r *engine.Run) {
	base, err := c.loc.base()
	if err != nil {
		engine.Fail(c.head, err)
		return
	}
	r.Add(c.head, base+uint64(c.loc.offs[0]))
}

func (l Loc) base() (uint64, error) {
	switch l.kind {
	case baseModule:
		return l.proc.moduleBase(l.module)
	case baseCell:
		return *l.cell, nil
	}
	return l.addr, nil
}
