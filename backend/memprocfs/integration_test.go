package memprocfs_test

// Integration tests against a real memory dump. They need the MemProcFS
// native library and a Windows dump, the same as gomemprocfs:
//
//	VMM_DLL_PATH=/path/to/vmm.so MEMDUMP_PATH=/path/to/memdump.raw go test ./backend/memprocfs
//
// Without them the tests skip. They rely on things true in any Windows dump
// (PE headers, the PEB loader list of explorer.exe), not on fixed addresses.

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	gm "github.com/sergeyzav/gomemprocfs"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/memprocfs"
)

type target struct {
	vmm  *gm.Vmm
	be   *memprocfs.Backend
	s    *smartmem.Session
	p    *smartmem.Process
	pid  uint32
	exe  gm.Module
	ctx  context.Context
	mods []gm.Module
}

func setup(t *testing.T, opts ...smartmem.Option) *target {
	t.Helper()
	dll, dump := os.Getenv("VMM_DLL_PATH"), os.Getenv("MEMDUMP_PATH")
	if dll == "" || dump == "" {
		t.Skip("VMM_DLL_PATH and/or MEMDUMP_PATH not set")
	}
	vmm, err := gm.NewVmm(dll, gm.WithDevice(dump), gm.WithWaitInitialize())
	if err != nil {
		t.Fatalf("NewVmm: %v", err)
	}
	be := memprocfs.New(vmm)
	t.Cleanup(func() { be.Close(); _ = vmm.Close() })

	s := smartmem.New(be, opts...)
	p, err := s.ProcessByName("explorer.exe")
	if err != nil {
		t.Fatalf("ProcessByName: %v", err)
	}
	list, err := vmm.GetModuleList(p.PID(), gm.ModuleFlagNone)
	if err != nil || len(list.Modules) == 0 {
		t.Fatalf("GetModuleList: %v", err)
	}
	tg := &target{vmm: vmm, be: be, s: s, p: p, pid: p.PID(), ctx: context.Background(), mods: list.Modules}
	for _, m := range list.Modules {
		if strings.EqualFold(m.Name, "explorer.exe") {
			tg.exe = m
		}
	}
	if tg.exe.BaseAddress == 0 {
		t.Fatal("explorer.exe module not found")
	}
	return tg
}

// M1: smartmem reads the same bytes as a plain MemRead, or fails where it fails.
func TestM1_DifferentialAgainstMemRead(t *testing.T) {
	tg := setup(t)
	rng := rand.New(rand.NewPCG(1, 2))
	type probe struct {
		addr uint64
		f    *smartmem.Future[[16]byte]
	}
	b := tg.s.NewBatch()
	var probes []probe
	for _, m := range tg.mods[:min(8, len(tg.mods))] {
		for range 16 {
			addr := m.BaseAddress + rng.Uint64N(uint64(max(m.ImageSize, 16))-16)
			probes = append(probes, probe{addr, smartmem.Read[[16]byte](b, tg.p.At(addr))})
		}
	}
	if err := b.Run(tg.ctx); err != nil {
		t.Fatal(err)
	}
	same, failedBoth := 0, 0
	for _, pr := range probes {
		want, wantErr := tg.vmm.MemRead(tg.pid, pr.addr, 16)
		got, err := pr.f.Get()
		switch {
		case wantErr == nil && err == nil:
			if !bytes.Equal(got[:], want) {
				t.Errorf("0x%x: smartmem % x, MemRead % x", pr.addr, got, want)
			}
			same++
		case wantErr != nil && err != nil:
			failedBoth++
		default:
			t.Errorf("0x%x: smartmem err %v, MemRead err %v", pr.addr, err, wantErr)
		}
	}
	if same == 0 {
		t.Fatalf("no readable probes (%d failed in both)", failedBoth)
	}
	t.Logf("%d probes equal, %d unreadable in both", same, failedBoth)
}

// M2: a dependent read: the PE header offset decides where the "PE" signature
// is read in the next round.
func TestM2_PEHeaderChain(t *testing.T) {
	tg := setup(t)
	base := tg.p.Module("explorer.exe")
	b := tg.s.NewBatch()
	mz := smartmem.Read[[2]byte](b, base)
	sig := smartmem.Then(smartmem.Read[int32](b, base.Add(0x3C)), func(s smartmem.Scope, lfanew int32) *smartmem.Future[[4]byte] {
		return smartmem.Read[[4]byte](s, base.Add(int64(lfanew)))
	})
	if err := b.Run(tg.ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := mz.Get(); err != nil || string(v[:]) != "MZ" {
		t.Fatalf("MZ = %q, %v", v, err)
	}
	if v, err := sig.Get(); err != nil || string(v[:]) != "PE\x00\x00" {
		t.Fatalf("PE signature = %q, %v", v, err)
	}
}

// ldrEntry is the part of LDR_DATA_TABLE_ENTRY (x64) the walk needs.
type ldrEntry struct {
	Next    uint64 // InLoadOrderLinks.Flink
	DllBase uint64
	Name    string // BaseDllName
}

func (e *ldrEntry) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &e.Next, base)
	smartmem.Field(l, &e.DllBase, base.Add(0x30))
	smartmem.FieldString(l, &e.Name, base.Add(0x58), smartmem.UnicodeString)
}

// M3: walk PEB -> Ldr -> InLoadOrderModuleList with Load and Then. Every
// entry must match a module from GetModuleList, and the walk must end at
// the list head, not at MaxRounds.
func TestM3_LoaderListWalk(t *testing.T) {
	tg := setup(t)
	peb, err := tg.p.PEB()
	if err != nil {
		t.Fatal(err)
	}
	b := tg.s.NewBatch()
	head := peb.Add(0x18).Deref().Add(0x10) // &Ldr->InLoadOrderModuleList
	headAddr := smartmem.Map(smartmem.Read[uint64](b, peb.Add(0x18)), func(ldr uint64) (uint64, error) {
		return ldr + 0x10, nil
	})
	first := smartmem.Read[uint64](b, head)

	var walk func(s smartmem.Scope, entry, stop uint64) *smartmem.Future[[]ldrEntry]
	walk = func(s smartmem.Scope, entry, stop uint64) *smartmem.Future[[]ldrEntry] {
		return smartmem.Then(smartmem.Load[ldrEntry](s, tg.p.At(entry)), func(s smartmem.Scope, e ldrEntry) *smartmem.Future[[]ldrEntry] {
			if e.Next == stop {
				return smartmem.Resolved(s, []ldrEntry{e})
			}
			return smartmem.Map(walk(s, e.Next, stop), func(rest []ldrEntry) ([]ldrEntry, error) {
				return append([]ldrEntry{e}, rest...), nil
			})
		})
	}
	all := smartmem.Then(headAddr, func(s smartmem.Scope, h uint64) *smartmem.Future[[]ldrEntry] {
		return smartmem.Then(first, func(s smartmem.Scope, f uint64) *smartmem.Future[[]ldrEntry] {
			return walk(s, f, h)
		})
	})
	if err := b.Run(tg.ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := all.Get()
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	byName := map[string]uint64{}
	for _, m := range tg.mods {
		byName[strings.ToLower(m.Name)] = m.BaseAddress
	}
	for _, e := range entries {
		if base, ok := byName[strings.ToLower(e.Name)]; !ok || base != e.DllBase {
			t.Errorf("entry %q at 0x%x: module list has 0x%x (found %v)", e.Name, e.DllBase, base, ok)
		}
	}
	if len(entries) < 2 || !strings.EqualFold(entries[0].Name, "explorer.exe") {
		t.Fatalf("walked %d entries, first %+v", len(entries), entries[:min(1, len(entries))])
	}
	t.Logf("walked %d loader entries", len(entries))
}

// M4: native x64 processes are accepted, WoW64 ones are refused.
func TestM4_ProcessCheck(t *testing.T) {
	tg := setup(t)
	all, err := tg.vmm.GetProcessInfoAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, pi := range all {
		if pi.Win.IsWow64 != 0 {
			if _, err := tg.s.Process(pi.PID); !errors.Is(err, smartmem.ErrUnsupported) {
				t.Fatalf("WoW64 pid %d: err=%v, want ErrUnsupported", pi.PID, err)
			}
			return
		}
	}
	t.Log("no WoW64 process in the dump; only the x64 case was checked")
}

// M5: a static Plan on the real backend runs without allocating.
func TestM5_PlanZeroAlloc(t *testing.T) {
	tg := setup(t)
	base := tg.p.Module("explorer.exe")
	plan := tg.s.NewPlan()
	smartmem.Read[[2]byte](plan, base)
	smartmem.Read[int32](plan, base.Add(0x3C))
	smartmem.Read[[16]byte](plan, base.Add(0x80))
	if err := plan.Exec(tg.ctx); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(50, func() {
		if err := plan.Exec(tg.ctx); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Exec allocates %.1f times, want 0", allocs)
	}
}

// M6: an unreadable address is a ReadError wrapping gomemprocfs.ErrReadFailed.
func TestM6_ReadErrorWrapsSentinel(t *testing.T) {
	tg := setup(t)
	_, err := smartmem.ReadNow[uint64](tg.ctx, tg.p.At(0x10))
	var re *smartmem.ReadError
	if !errors.As(err, &re) || !errors.Is(err, gm.ErrReadFailed) {
		t.Fatalf("err=%v (%T), want ReadError wrapping ErrReadFailed", err, err)
	}
}

// M7 (exploratory): what a scatter read across a readable and an unreadable
// page returns. The result decides how the fake backend should behave by
// default (fake.FailWhole or fake.Truncate); the test logs it and checks
// that smartmem turns it into one of the two documented errors.
func TestM7_PartialReadBehaviour(t *testing.T) {
	tg := setup(t)
	var boundary uint64
	for page := tg.exe.BaseAddress; page+0x2000 <= tg.exe.BaseAddress+uint64(tg.exe.ImageSize); page += 0x1000 {
		_, errA := tg.vmm.MemRead(tg.pid, page, 0x1000)
		_, errB := tg.vmm.MemRead(tg.pid, page+0x1000, 0x1000)
		if errA == nil && errB != nil {
			boundary = page + 0x1000
			break
		}
	}
	if boundary == 0 {
		t.Skip("no readable page followed by an unreadable one in explorer.exe")
	}
	_, err := smartmem.ReadNow[uint64](tg.ctx, tg.p.At(boundary-4))
	var pe *smartmem.PartialReadError
	var re *smartmem.ReadError
	switch {
	case errors.As(err, &pe):
		t.Logf("MemProcFS returns the readable prefix: %v (fake.Truncate)", err)
	case errors.As(err, &re):
		t.Logf("MemProcFS fails the whole range: %v (fake.FailWhole)", err)
	default:
		t.Fatalf("err=%v (%T), want PartialReadError or ReadError", err, err)
	}
}
