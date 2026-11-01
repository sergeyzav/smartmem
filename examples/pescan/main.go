// Command pescan reads the PE headers of every module of a process, one
// goroutine per module.
//
// The goroutines know nothing about each other: each one follows its own
// chain (DOS header -> NT headers -> section table) and waits for its
// result. The Scheduler merges them, so the whole scan takes three rounds
// however many modules there are.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	gm "github.com/sergeyzav/gomemprocfs"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/examples/internal/target"
)

type fileHeader struct {
	Machine              uint16
	NumberOfSections     uint16
	TimeDateStamp        uint32
	PointerToSymbolTable uint32
	NumberOfSymbols      uint32
	SizeOfOptionalHeader uint16
	Characteristics      uint16
}

// ntHeaders picks three parts of IMAGE_NT_HEADERS64; the engine merges them
// into one read.
type ntHeaders struct {
	Signature   [4]byte    `mem:"0x0"`
	File        fileHeader `mem:"0x4"`
	SizeOfImage uint32     `mem:"0x50"`
}

type sectionHeader struct {
	Name                 [8]byte
	VirtualSize          uint32
	VirtualAddress       uint32
	SizeOfRawData        uint32
	PointerToRawData     uint32
	PointerToRelocations uint32
	PointerToLinenumbers uint32
	NumberOfRelocations  uint16
	NumberOfLinenumbers  uint16
	Characteristics      uint32
}

// Module is a module to scan.
type Module struct {
	Name string
	Base uint64
}

// PE is what the scan finds in a module's headers.
type PE struct {
	Module
	Machine     uint16
	Built       time.Time
	SizeOfImage uint32
	Sections    []string
}

func inspect(s smartmem.Scope, p *smartmem.Process, m Module) *smartmem.Future[PE] {
	base := p.At(m.Base)
	lfanew := smartmem.Read[int32](s, base.Add(0x3C))
	return smartmem.Then(lfanew, func(s smartmem.Scope, off int32) *smartmem.Future[PE] {
		nt := base.Add(int64(off))
		return smartmem.Then(smartmem.Read[ntHeaders](s, nt), func(s smartmem.Scope, h ntHeaders) *smartmem.Future[PE] {
			if string(h.Signature[:]) != "PE\x00\x00" {
				return smartmem.Failed[PE](s, fmt.Errorf("%s: no PE signature at %#x", m.Name, m.Base+uint64(off)))
			}
			table := nt.Add(0x18 + int64(h.File.SizeOfOptionalHeader))
			secs := smartmem.ReadSlice[sectionHeader](s, table, int(h.File.NumberOfSections))
			return smartmem.Map(secs, func(secs []sectionHeader) (PE, error) {
				pe := PE{
					Module:      m,
					Machine:     h.File.Machine,
					Built:       time.Unix(int64(h.File.TimeDateStamp), 0).UTC(),
					SizeOfImage: h.SizeOfImage,
				}
				for _, sec := range secs {
					pe.Sections = append(pe.Sections, string(bytes.TrimRight(sec.Name[:], "\x00")))
				}
				return pe, nil
			})
		})
	})
}

type result struct {
	pe  PE
	err error
}

// scan inspects every module from its own goroutine, as independent
// subsystems would, and returns the results in module order.
func scan(ctx context.Context, sch *smartmem.Scheduler, p *smartmem.Process, mods []Module) []result {
	out := make([]result, len(mods))
	var wg sync.WaitGroup
	for i, m := range mods {
		wg.Go(func() {
			pe, err := inspect(sch, p, m).Wait(ctx)
			out[i] = result{pe, err}
		})
	}
	wg.Wait()
	return out
}

func main() {
	flag.Parse()
	t, err := target.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer t.Close()

	list, err := t.Vmm.GetModuleList(t.P.PID(), gm.ModuleFlagNone)
	if err != nil {
		log.Fatal(err)
	}
	mods := make([]Module, len(list.Modules))
	for i, m := range list.Modules {
		mods[i] = Module{m.Name, m.BaseAddress}
	}
	slices.SortFunc(mods, func(a, b Module) int { return strings.Compare(a.Name, b.Name) })

	sch := smartmem.NewScheduler(t.S)
	defer sch.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBASE\tMACHINE\tBUILT\tSIZE\tSECTIONS")
	for _, r := range scan(context.Background(), sch, t.P, mods) {
		if r.err != nil {
			fmt.Fprintf(w, "\t\t\t\t\t%v\n", r.err)
			continue
		}
		pe := r.pe
		fmt.Fprintf(w, "%s\t%#x\t%#x\t%s\t%#x\t%s\n",
			pe.Name, pe.Base, pe.Machine, pe.Built.Format(time.DateOnly), pe.SizeOfImage, strings.Join(pe.Sections, " "))
	}
	_ = w.Flush()
}
