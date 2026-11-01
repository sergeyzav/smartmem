// Command modules lists the modules of a process by walking the loader list
// in its PEB: PEB.Ldr -> InLoadOrderModuleList -> LDR_DATA_TABLE_ENTRY.
//
// The list is linked, so the walk takes one round per entry: an entry's
// address is known only after the previous one is read. The names are left
// for the end and read for all entries at once, in two shared rounds
// (the UNICODE_STRING headers, then their buffers).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"text/tabwriter"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/examples/internal/target"
)

const (
	pebLdr         = 0x18 // PEB.Ldr
	ldrInLoadOrder = 0x10 // PEB_LDR_DATA.InLoadOrderModuleList
	entryName      = 0x58 // LDR_DATA_TABLE_ENTRY.BaseDllName
)

// ldrEntry is the part of LDR_DATA_TABLE_ENTRY (x64) the walk needs.
// The fields are close together, so the engine reads them as one range.
type ldrEntry struct {
	Next        uint64 `mem:"0x0"` // InLoadOrderLinks.Flink
	DllBase     uint64 `mem:"0x30"`
	SizeOfImage uint32 `mem:"0x40"`
}

// Module is one loaded module.
type Module struct {
	Name string
	Base uint64
	Size uint32
}

type entry struct {
	addr uint64
	ldrEntry
}

func listModules(ctx context.Context, s *smartmem.Session, p *smartmem.Process) ([]Module, error) {
	peb, err := p.PEB()
	if err != nil {
		return nil, err
	}
	b := s.NewBatch()
	ldr := smartmem.Read[uint64](b, peb.Add(pebLdr))
	mods := smartmem.Then(ldr, func(s smartmem.Scope, ldr uint64) *smartmem.Future[[]Module] {
		head := ldr + ldrInLoadOrder
		first := smartmem.Read[uint64](s, p.At(head))
		return smartmem.Then(first, func(s smartmem.Scope, first uint64) *smartmem.Future[[]Module] {
			if first == head {
				return smartmem.Resolved(s, []Module{})
			}
			return smartmem.Then(walk(s, p, first, head, nil), func(s smartmem.Scope, es []entry) *smartmem.Future[[]Module] {
				return withNames(s, p, es)
			})
		})
	})
	if err := b.Run(ctx); err != nil {
		return nil, err
	}
	return mods.Get()
}

// walk reads the entry at addr and follows Flink until it comes back to
// the list head. Each step is a Then, so each entry takes one round.
func walk(s smartmem.Scope, p *smartmem.Process, addr, head uint64, acc []entry) *smartmem.Future[[]entry] {
	e := smartmem.Read[ldrEntry](s, p.At(addr))
	return smartmem.Then(e, func(s smartmem.Scope, v ldrEntry) *smartmem.Future[[]entry] {
		acc = append(acc, entry{addr, v})
		if v.Next == head {
			return smartmem.Resolved(s, acc)
		}
		return walk(s, p, v.Next, head, acc)
	})
}

func withNames(s smartmem.Scope, p *smartmem.Process, es []entry) *smartmem.Future[[]Module] {
	names := make([]*smartmem.Future[string], len(es))
	for i, e := range es {
		names[i] = smartmem.ReadString(s, p.At(e.addr+entryName), smartmem.UnicodeString)
	}
	return smartmem.Map(smartmem.All(names...), func(names []string) ([]Module, error) {
		mods := make([]Module, len(es))
		for i, e := range es {
			mods[i] = Module{Name: names[i], Base: e.DllBase, Size: e.SizeOfImage}
		}
		return mods, nil
	})
}

func main() {
	flag.Parse()
	t, err := target.Open(smartmem.WithMaxRounds(4096))
	if err != nil {
		log.Fatal(err)
	}
	defer t.Close()

	mods, err := listModules(context.Background(), t.S, t.P)
	if err != nil {
		log.Fatal(err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BASE\tSIZE\tNAME")
	for _, m := range mods {
		fmt.Fprintf(w, "%#x\t%#x\t%s\n", m.Base, m.Size, m.Name)
	}
	_ = w.Flush()
}
