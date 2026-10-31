// Package engine executes request graphs in rounds. Each round is one backend
// Exec per (pid, flags) pair; a Deref feeds the next node of its chain into
// the following round.
package engine

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/sergeyzav/smartmem/backend"
)

const (
	// PageSize is the page granularity used for coalescing.
	PageSize = 4096
	// MaxUserAddr is the highest user-mode address on x64 Windows.
	MaxUserAddr = 0x00007FFFFFFFFFFF
	// PtrSize is the pointer size of v1 targets.
	PtrSize = 8
)

// Kind is the kind of a Node.
type Kind uint8

const (
	// Write writes Data at the node address.
	Write Kind = iota
	// Leaf reads Size bytes and hands them to its Sink.
	Leaf
	// Deref reads a pointer; the pointer plus Off is the address of Next.
	Deref
)

// Sink receives the result of a Leaf or Write. For a Leaf, b holds Size bytes
// and is only valid during the call.
type Sink interface {
	Done(b []byte, err error)
}

// DerefObserver sees every pointer a Deref reads.
type DerefObserver interface {
	Deref(n *Node, slot, value uint64)
}

// Node is one step of a request chain: zero or more Derefs ending in a Leaf
// or a Write. Nodes are reusable: a Run only sets the address and the round.
type Node struct {
	Kind  Kind
	PID   uint32
	Flags uint64
	Size  uint32 // bytes to read: Leaf, Deref (PtrSize)
	Off   int64  // Deref: added to the pointer value
	Step  int    // Deref: index in the chain, from 0
	Next  *Node  // Deref: the node at the pointer
	Data  []byte // Write: bytes to write
	Sink  Sink   // Leaf, Write
	// Skip, when set and true, drops the node and the rest of its chain
	// without calling any Sink, for example after its owner failed.
	Skip *bool
	// Delay holds the node back for this many rounds after it is added.
	Delay int
	// Observer, if set, sees the pointer this Deref reads.
	Observer DerefObserver

	addr      uint64
	notBefore int
}

// Addr returns the address the node was scheduled at.
func (n *Node) Addr() uint64 { return n.addr }

func (n *Node) skipped() bool { return n.Skip != nil && *n.Skip }

// Fail delivers err to the Sink at the end of n's chain.
func Fail(n *Node, err error) {
	for ; n != nil; n = n.Next {
		if n.skipped() {
			return
		}
		if n.Kind != Deref && n.Sink != nil {
			n.Sink.Done(nil, err)
		}
	}
}

// Options tune a Run.
type Options struct {
	MaxRounds int    // rounds before pending nodes fail with ErrMaxRounds
	MaxGap    uint64 // largest gap between reads merged into one range
}

// Run is one execution. It is not safe for concurrent use; Sinks run on the
// goroutine that calls Execute and may add nodes for later rounds.
type Run struct {
	be    backend.Backend
	opts  Options
	round int
	queue []*Node

	// scratch, reused across rounds and Resets
	cur    []*Node
	writes []*Node
	reads  []*Node
	spans  []span
	rops   []backend.ReadOp
	wops   []backend.WriteOp
	arena  []byte
	failed []*Node
}

// NewRun returns a Run over be.
func NewRun(be backend.Backend, opts Options) *Run {
	return &Run{be: be, opts: opts}
}

// Reset prepares the Run for another execution, keeping its buffers.
func (r *Run) Reset(be backend.Backend, opts Options) {
	r.be, r.opts, r.round = be, opts, 0
	clear(r.queue)
	r.queue = r.queue[:0]
}

// Round returns the current round: 0 before the first one.
func (r *Run) Round() int { return r.round }

// Add schedules n at addr, n.Delay rounds after the next one.
func (r *Run) Add(n *Node, addr uint64) {
	r.AddAt(n, addr, r.round+1+n.Delay)
}

// AddAt schedules n at addr, not before the given round (and never before the
// next one).
func (r *Run) AddAt(n *Node, addr uint64, round int) {
	n.addr = addr
	n.notBefore = max(round, r.round+1)
	r.queue = append(r.queue, n)
}

// Execute runs rounds until nothing is pending. Errors of single requests go
// to their Sinks. Execute returns an error only if ctx is done or the backend
// failed as a whole; every pending Sink then gets that error too.
func (r *Run) Execute(ctx context.Context) error {
	ra, _ := r.be.(backend.RoundAware)
	for len(r.queue) > 0 {
		r.round++
		if r.round > r.opts.MaxRounds {
			r.failNodes(nil, ErrMaxRounds)
			return nil
		}
		if err := ctx.Err(); err != nil {
			r.failNodes(nil, err)
			return err
		}

		cur, keep := r.cur[:0], r.queue[:0]
		for _, n := range r.queue {
			switch {
			case n.skipped():
			case n.notBefore <= r.round:
				cur = append(cur, n)
			default:
				keep = append(keep, n)
			}
		}
		clear(r.queue[len(keep):])
		r.queue, r.cur = keep, cur
		if len(cur) == 0 {
			continue
		}

		if ra != nil {
			ra.BeginRound(r.round)
		}
		slices.SortFunc(cur, compareNodes)
		for i := 0; i < len(cur); {
			j := i + 1
			for j < len(cur) && cur[j].PID == cur[i].PID && cur[j].Flags == cur[i].Flags {
				j++
			}
			if err := r.execGroup(ctx, cur[i:j]); err != nil {
				err = fmt.Errorf("smartmem: backend: %w", err)
				r.failNodes(cur[i:], err)
				clear(cur)
				return err
			}
			i = j
		}
		clear(cur)
	}
	return nil
}

func compareNodes(a, b *Node) int {
	if c := cmp.Compare(a.PID, b.PID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Flags, b.Flags); c != 0 {
		return c
	}
	if aw, bw := a.Kind == Write, b.Kind == Write; aw != bw {
		if aw {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(a.addr, b.addr); c != 0 {
		return c
	}
	return cmp.Compare(a.Size, b.Size)
}

// failNodes fails nodes, then everything left in the queue, including nodes
// that Sinks add while failing, so no Sink is left waiting.
func (r *Run) failNodes(nodes []*Node, err error) {
	for _, n := range nodes {
		Fail(n, err)
	}
	for len(r.queue) > 0 {
		r.failed = append(r.failed[:0], r.queue...)
		clear(r.queue)
		r.queue = r.queue[:0]
		for _, n := range r.failed {
			Fail(n, err)
		}
	}
	clear(r.failed)
}

func (r *Run) execGroup(ctx context.Context, nodes []*Node) error {
	writes, reads := r.writes[:0], r.reads[:0]
	for _, n := range nodes {
		if n.Kind == Write {
			writes = append(writes, n)
		} else {
			reads = append(reads, n)
		}
	}
	r.writes, r.reads = writes, reads
	defer func() { clear(writes); clear(reads) }()

	r.wops = r.wops[:0]
	for _, n := range writes {
		r.wops = append(r.wops, backend.WriteOp{Addr: n.addr, Src: n.Data})
	}
	r.spans = coalesce(reads, r.opts.MaxGap, r.spans[:0])
	total := 0
	for _, s := range r.spans {
		total += int(s.end - s.start)
	}
	if cap(r.arena) < total {
		r.arena = make([]byte, total)
	}
	r.rops = r.rops[:0]
	off := 0
	for i := range r.spans {
		s := &r.spans[i]
		size := int(s.end - s.start)
		s.off = off
		r.rops = append(r.rops, backend.ReadOp{Addr: s.start, Dst: r.arena[off : off+size : off+size]})
		off += size
	}

	if err := r.be.Exec(ctx, nodes[0].PID, nodes[0].Flags, r.wops, r.rops); err != nil {
		return err
	}

	for i, n := range writes {
		if n.skipped() {
			continue
		}
		var err error
		if e := r.wops[i].Err; e != nil {
			err = &WriteError{Addr: n.addr, Err: e}
		}
		n.Sink.Done(nil, err)
	}
	for i, s := range r.spans {
		op := r.rops[i]
		for _, n := range reads[s.first:s.last] {
			rel := int(n.addr - s.start)
			size := int(n.Size)
			switch {
			case op.Err != nil:
				r.complete(n, nil, &ReadError{Addr: n.addr, Err: op.Err})
			case rel+size > op.N:
				r.complete(n, nil, &PartialReadError{Addr: n.addr, Want: size, Got: max(0, op.N-rel)})
			default:
				b := r.arena[s.off+rel : s.off+rel+size : s.off+rel+size]
				r.complete(n, b, nil)
			}
		}
	}
	return nil
}

func (r *Run) complete(n *Node, b []byte, err error) {
	if n.skipped() {
		return
	}
	if err != nil {
		Fail(n, err)
		return
	}
	if n.Kind == Leaf {
		n.Sink.Done(b, nil)
		return
	}
	v := binary.LittleEndian.Uint64(b)
	if n.Observer != nil {
		n.Observer.Deref(n, n.addr, v)
	}
	switch {
	case v == 0:
		Fail(n.Next, &NullPointerError{Step: n.Step})
	case v > MaxUserAddr:
		Fail(n.Next, &BadPointerError{Step: n.Step, Value: v})
	default:
		r.AddAt(n.Next, v+uint64(n.Off), 0)
	}
}

// span is one coalesced read range: reads[first:last] fall inside it.
type span struct {
	start, end  uint64
	first, last int
	off         int
}

func page(a uint64) uint64 { return a &^ (PageSize - 1) }

// coalesce merges reads, sorted by address, into ranges. A range never crosses
// a page boundary, so an unreadable page fails only the reads on it; a read
// that crosses a page boundary itself gets its own range.
func coalesce(reads []*Node, gap uint64, spans []span) []span {
	cur := -1
	for i, n := range reads {
		end := n.addr + uint64(n.Size)
		if page(n.addr) != page(end-1) {
			spans = append(spans, span{start: n.addr, end: end, first: i, last: i + 1})
			cur = -1
			continue
		}
		if cur >= 0 {
			s := &spans[cur]
			if page(n.addr) == page(s.start) && n.addr <= s.end+gap {
				s.end = max(s.end, end)
				s.last = i + 1
				continue
			}
		}
		spans = append(spans, span{start: n.addr, end: end, first: i, last: i + 1})
		cur = len(spans) - 1
	}
	return spans
}
