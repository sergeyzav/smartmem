// Command watch polls values behind pointer chains and prints them when they
// change, like an address list in Cheat Engine:
//
//	go run ./examples/watch -proc game.exe \
//	    -w 'hp=game.dll+0x1A2B30,0x30,0x90:i32' \
//	    -w 'pos.x=game.dll+0x1A2B30,0x30,0x40:f32'
//
// A watch is name=base[,offset...]:type. The base is module+offset or an
// address, and the offsets follow Cheat Engine: game.dll+0x10,0x30,0x90 is
// [[game.dll+0x10]+0x30]+0x90.
//
// All watches are one Plan: described once, then run every tick in as many
// rounds as the longest chain, with every value read in the same last round.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/examples/internal/target"
)

type watch struct {
	name string
	loc  smartmem.Loc
	get  func() (string, error)
}

type watcher struct {
	plan *smartmem.Plan
	ws   []watch
	last []string
}

func newWatcher(s *smartmem.Session, p *smartmem.Process, specs []string) (*watcher, error) {
	w := &watcher{plan: s.NewPlan()}
	for _, spec := range specs {
		name, loc, typ, err := parse(p, spec)
		if err != nil {
			return nil, err
		}
		get, err := add(w.plan, loc, typ)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", spec, err)
		}
		w.ws = append(w.ws, watch{name, loc, get})
	}
	w.last = make([]string, len(w.ws))
	return w, nil
}

// poll runs the Plan once and returns a line for every watch whose value or
// error changed since the previous poll.
func (w *watcher) poll(ctx context.Context) ([]string, error) {
	if err := w.plan.Exec(ctx); err != nil {
		return nil, err
	}
	var out []string
	w.plan.View(func() {
		for i, wt := range w.ws {
			v, err := wt.get()
			if err != nil {
				v = "error: " + err.Error()
			}
			if v != w.last[i] {
				w.last[i] = v
				out = append(out, fmt.Sprintf("%s = %s", wt.name, v))
			}
		}
	})
	return out, nil
}

// parse reads name=base[,offset...]:type.
func parse(p *smartmem.Process, spec string) (string, smartmem.Loc, string, error) {
	name, rest, ok := strings.Cut(spec, "=")
	if !ok {
		return "", smartmem.Loc{}, "", fmt.Errorf("%q: want name=base[,offset...]:type", spec)
	}
	chain, typ, ok := strings.Cut(rest, ":")
	if !ok {
		return "", smartmem.Loc{}, "", fmt.Errorf("%q: missing :type", spec)
	}
	parts := strings.Split(chain, ",")
	base, err := parseBase(p, parts[0])
	if err != nil {
		return "", smartmem.Loc{}, "", fmt.Errorf("%q: %w", spec, err)
	}
	offs := make([]int64, 0, len(parts)-1)
	for _, s := range parts[1:] {
		off, err := strconv.ParseInt(strings.TrimSpace(s), 0, 64)
		if err != nil {
			return "", smartmem.Loc{}, "", fmt.Errorf("%q: offset %q: %w", spec, s, err)
		}
		offs = append(offs, off)
	}
	return name, p.Chain(base, offs...), typ, nil
}

func parseBase(p *smartmem.Process, s string) (smartmem.Loc, error) {
	s = strings.TrimSpace(s)
	if mod, off, ok := strings.Cut(s, "+"); ok {
		o, err := strconv.ParseInt(off, 0, 64)
		if err != nil {
			return smartmem.Loc{}, fmt.Errorf("base offset %q: %w", off, err)
		}
		return p.Module(mod).Add(o), nil
	}
	addr, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return smartmem.Loc{}, fmt.Errorf("base %q: %w", s, err)
	}
	return p.At(addr), nil
}

func add(plan *smartmem.Plan, loc smartmem.Loc, typ string) (func() (string, error), error) {
	switch typ {
	case "i8":
		return format(smartmem.Read[int8](plan, loc), "%d"), nil
	case "u8":
		return format(smartmem.Read[uint8](plan, loc), "%d"), nil
	case "i16":
		return format(smartmem.Read[int16](plan, loc), "%d"), nil
	case "u16":
		return format(smartmem.Read[uint16](plan, loc), "%d"), nil
	case "i32":
		return format(smartmem.Read[int32](plan, loc), "%d"), nil
	case "u32":
		return format(smartmem.Read[uint32](plan, loc), "%d"), nil
	case "i64":
		return format(smartmem.Read[int64](plan, loc), "%d"), nil
	case "u64":
		return format(smartmem.Read[uint64](plan, loc), "%d"), nil
	case "f32":
		return format(smartmem.Read[float32](plan, loc), "%g"), nil
	case "f64":
		return format(smartmem.Read[float64](plan, loc), "%g"), nil
	case "ptr":
		return format(smartmem.Read[uint64](plan, loc), "%#x"), nil
	case "str":
		return format(smartmem.ReadString(plan, loc, smartmem.CString(64)), "%q"), nil
	case "wstr":
		return format(smartmem.ReadString(plan, loc, smartmem.UTF16(64)), "%q"), nil
	}
	return nil, errors.New("unknown type " + typ + " (i8..u64, f32, f64, ptr, str, wstr)")
}

func format[T any](f *smartmem.Future[T], verb string) func() (string, error) {
	return func() (string, error) {
		v, err := f.Get()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(verb, v), nil
	}
}

type specs []string

func (s *specs) String() string     { return strings.Join(*s, " ") }
func (s *specs) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var ws specs
	flag.Var(&ws, "w", "a watch: name=base[,offset...]:type (repeatable)")
	every := flag.Duration("every", 100*time.Millisecond, "poll interval")
	flag.Parse()
	if len(ws) == 0 {
		log.Fatal("add at least one -w")
	}
	t, err := target.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer t.Close()

	w, err := newWatcher(t.S, t.P, ws)
	if err != nil {
		log.Fatal(err)
	}
	for _, wt := range w.ws {
		log.Printf("watching %s at %s", wt.name, wt.loc)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(*every)
	defer tick.Stop()
	for {
		lines, err := w.poll(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Print(err)
			}
			return
		}
		for _, l := range lines {
			log.Print(l)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
