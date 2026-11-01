package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

// putPE lays out the headers of a module with the given sections at base.
func putPE(p *fake.Proc, base uint64, sections ...string) {
	const lfanew = 0x100
	nt := base + lfanew
	p.Bytes(base, []byte("MZ")).I32(base+0x3C, lfanew).Bytes(nt, []byte("PE\x00\x00"))
	fake.Put(p, nt+4, fileHeader{
		Machine:              0x8664,
		NumberOfSections:     uint16(len(sections)),
		TimeDateStamp:        1700000000,
		SizeOfOptionalHeader: 0xF0,
	})
	p.U32(nt+0x50, 0x20000)
	for i, name := range sections {
		var sec sectionHeader
		copy(sec.Name[:], name)
		fake.Put(p, nt+0x18+0xF0+uint64(i*40), sec)
	}
}

func TestScanSharesRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := fake.New()
		p := mem.Proc(100)
		mods := []Module{
			{"a.dll", 0x7ff600000000},
			{"b.dll", 0x7ff700000000},
			{"c.dll", 0x7ff800000000},
			{"broken.dll", 0x7ff900000000},
		}
		putPE(p, mods[0].Base, ".text", ".data")
		putPE(p, mods[1].Base, ".text", ".rdata", ".pdata")
		putPE(p, mods[2].Base, ".text")
		p.Bytes(mods[3].Base, []byte("MZ")).I32(mods[3].Base+0x3C, 0x80)

		s := smartmem.New(mem)
		proc, err := s.Process(100)
		if err != nil {
			t.Fatal(err)
		}
		sch := smartmem.NewScheduler(s)
		defer sch.Close()

		res := scan(context.Background(), sch, proc, mods)
		want := [][]string{{".text", ".data"}, {".text", ".rdata", ".pdata"}, {".text"}}
		for i, w := range want {
			if res[i].err != nil || !slices.Equal(res[i].pe.Sections, w) || res[i].pe.Machine != 0x8664 {
				t.Errorf("%s: %+v, %v", mods[i].Name, res[i].pe, res[i].err)
			}
		}
		if err := res[3].err; err == nil || !strings.Contains(err.Error(), "no PE signature") {
			t.Errorf("broken.dll: err=%v", err)
		}
		// Four goroutines, three rounds: e_lfanew, NT headers, section tables.
		if n := mem.Execs(); n != 3 {
			t.Fatalf("%d rounds, want 3\n%s", n, mem.Trace())
		}
	})
}
