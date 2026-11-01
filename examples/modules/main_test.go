package main

import (
	"context"
	"slices"
	"testing"
	"unicode/utf16"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

const (
	peb  = 0x10000
	ldr  = 0x20000
	head = ldr + ldrInLoadOrder
)

// putEntry lays out an LDR_DATA_TABLE_ENTRY at addr, with its name in a
// buffer on another page.
func putEntry(p *fake.Proc, addr, next uint64, m Module, nameBuf uint64) {
	name := utf16.Encode([]rune(m.Name))
	p.U64(addr, next).
		U64(addr+0x30, m.Base).
		U32(addr+0x40, m.Size).
		U32(addr+entryName, uint32(len(name)*2)|uint32(len(name)*2)<<16).
		U64(addr+entryName+8, nameBuf)
	b := make([]byte, 0, len(name)*2)
	for _, c := range name {
		b = append(b, byte(c), byte(c>>8))
	}
	p.Bytes(nameBuf, b)
}

func TestListModules(t *testing.T) {
	want := []Module{
		{"app.exe", 0x7ff600000000, 0x5000},
		{"ntdll.dll", 0x7ffa00000000, 0x1f8000},
		{"kernel32.dll", 0x7ff900000000, 0xc2000},
	}
	mem := fake.New()
	p := mem.Proc(100)
	p.PEB(peb).U64(peb+pebLdr, ldr).U64(head, 0x30000)
	for i, m := range want {
		addr := uint64(0x30000 + i*0x1000)
		next := addr + 0x1000
		if i == len(want)-1 {
			next = head
		}
		putEntry(p, addr, next, m, uint64(0x40000+i*0x1000))
	}

	s := smartmem.New(mem)
	proc, err := s.Process(100)
	if err != nil {
		t.Fatal(err)
	}
	got, err := listModules(context.Background(), s, proc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// Ldr, the first Flink, one round per entry, then two rounds for all names.
	if n := mem.Execs(); n != 2+len(want)+2 {
		t.Fatalf("%d rounds, want %d\n%s", n, 2+len(want)+2, mem.Trace())
	}
}

func TestListModulesEmpty(t *testing.T) {
	mem := fake.New()
	mem.Proc(100).PEB(peb).U64(peb+pebLdr, ldr).U64(head, head)
	s := smartmem.New(mem)
	proc, _ := s.Process(100)
	got, err := listModules(context.Background(), s, proc)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}
