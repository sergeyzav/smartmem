package fake_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sergeyzav/smartmem/backend"
	"github.com/sergeyzav/smartmem/backend/fake"
	"github.com/sergeyzav/smartmem/internal/testutil"
)

var ctx = context.Background()

// T1: within one Exec, writes land before reads, like VMMDLL_Scatter_Execute.
func TestT1_WritesBeforeReads(t *testing.T) {
	m := fake.New()
	m.Proc(1).U32(0x1000, 1)

	writes := []backend.WriteOp{{Addr: 0x1000, Src: []byte{2, 0, 0, 0}}}
	reads := []backend.ReadOp{{Addr: 0x1000, Dst: make([]byte, 4)}}
	if err := m.Exec(ctx, 1, 0, writes, reads); err != nil {
		t.Fatal(err)
	}
	if writes[0].Err != nil || reads[0].Err != nil {
		t.Fatalf("op errors: write %v, read %v", writes[0].Err, reads[0].Err)
	}
	if got := reads[0].Dst; !bytes.Equal(got, []byte{2, 0, 0, 0}) {
		t.Fatalf("read %v, want the written value", got)
	}
}

// T2: an unmapped page fails only the ranges that touch it.
func TestT2_UnmappedPageFailsOnlyItsRanges(t *testing.T) {
	newMem := func() *fake.Memory {
		m := fake.New()
		m.Proc(1).Map(0x1000, 2*fake.PageSize).Bytes(0x1ffc, []byte{1, 2, 3, 4}).Unmap(0x2000)
		return m
	}

	t.Run("isolation", func(t *testing.T) {
		m := newMem()
		reads := []backend.ReadOp{
			{Addr: 0x1000, Dst: make([]byte, 8)},
			{Addr: 0x2010, Dst: make([]byte, 8)},
		}
		if err := m.Exec(ctx, 1, 0, nil, reads); err != nil {
			t.Fatal(err)
		}
		if reads[0].Err != nil || reads[0].N != 8 {
			t.Errorf("mapped read: n=%d err=%v", reads[0].N, reads[0].Err)
		}
		if !errors.Is(reads[1].Err, fake.ErrUnmapped) || reads[1].N != 0 {
			t.Errorf("unmapped read: n=%d err=%v, want ErrUnmapped", reads[1].N, reads[1].Err)
		}
	})

	// A read from the last 4 bytes of a mapped page into an unmapped one.
	crossing := func() []backend.ReadOp { return []backend.ReadOp{{Addr: 0x1ffc, Dst: make([]byte, 8)}} }

	t.Run("FailWhole", func(t *testing.T) {
		m := newMem()
		reads := crossing()
		if err := m.Exec(ctx, 1, 0, nil, reads); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(reads[0].Err, fake.ErrUnmapped) || reads[0].N != 0 {
			t.Fatalf("n=%d err=%v, want n=0 and ErrUnmapped", reads[0].N, reads[0].Err)
		}
		if !bytes.Equal(reads[0].Dst, make([]byte, 8)) {
			t.Fatalf("dst %v, want zeroed", reads[0].Dst)
		}
	})

	t.Run("Truncate", func(t *testing.T) {
		m := newMem()
		m.SetPartialRead(fake.Truncate)
		reads := crossing()
		if err := m.Exec(ctx, 1, 0, nil, reads); err != nil {
			t.Fatal(err)
		}
		if reads[0].Err != nil || reads[0].N != 4 {
			t.Fatalf("n=%d err=%v, want n=4 and no error", reads[0].N, reads[0].Err)
		}
		if want := []byte{1, 2, 3, 4, 0, 0, 0, 0}; !bytes.Equal(reads[0].Dst, want) {
			t.Fatalf("dst %v, want %v", reads[0].Dst, want)
		}
	})

	t.Run("write to unmapped page writes nothing", func(t *testing.T) {
		m := newMem()
		writes := []backend.WriteOp{{Addr: 0x1ffe, Src: []byte{9, 9, 9, 9}}}
		if err := m.Exec(ctx, 1, 0, writes, nil); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(writes[0].Err, fake.ErrUnmapped) {
			t.Fatalf("err=%v, want ErrUnmapped", writes[0].Err)
		}
		got, err := m.Proc(1).Read(0x1ffc, 4)
		if err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
			t.Fatalf("memory changed: %v %v", got, err)
		}
	})
}

// T3: the trace format is stable: one line per Exec, writes first, then reads,
// each sorted by address.
func TestT3_TraceFormat(t *testing.T) {
	m := fake.New()
	m.Proc(1).Map(0x1000, 3*fake.PageSize)
	m.Proc(2).Map(0x1000, fake.PageSize)

	m.BeginRound(1)
	err := m.Exec(ctx, 1, 0,
		[]backend.WriteOp{{Addr: 0x3000, Src: make([]byte, 4)}, {Addr: 0x2000, Src: make([]byte, 2)}},
		[]backend.ReadOp{{Addr: 0x3000, Dst: make([]byte, 8)}, {Addr: 0x1000, Dst: make([]byte, 8)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Exec(ctx, 2, 0, nil, []backend.ReadOp{{Addr: 0x1010, Dst: make([]byte, 4)}}); err != nil {
		t.Fatal(err)
	}
	m.BeginRound(2)
	if err := m.Exec(ctx, 1, 0, nil, []backend.ReadOp{{Addr: 0x1000, Dst: make([]byte, 1)}}); err != nil {
		t.Fatal(err)
	}

	want := "r1 pid=1 W 0x2000+2 W 0x3000+4 R 0x1000+8 R 0x3000+8\n" +
		"r1 pid=2 R 0x1010+4\n" +
		"r2 pid=1 R 0x1000+1\n"
	if got := m.Trace(); got != want {
		t.Fatalf("trace:\n%s\nwant:\n%s", got, want)
	}
	if m.Execs() != 3 {
		t.Fatalf("Execs() = %d, want 3", m.Execs())
	}
	m.ResetTrace()
	if m.Trace() != "" || m.Execs() != 0 {
		t.Fatal("ResetTrace did not clear")
	}
}

// T4: Exec does not allocate in steady state with tracing off. Plan's
// zero-alloc test (P2) depends on it.
func TestT4_ExecDoesNotAllocate(t *testing.T) {
	if testutil.RaceEnabled {
		t.Skip("the race detector allocates")
	}
	m := fake.New()
	m.SetTrace(false)
	m.Proc(1).Map(0x1000, 2*fake.PageSize)
	writes := []backend.WriteOp{{Addr: 0x1100, Src: make([]byte, 16)}}
	reads := []backend.ReadOp{{Addr: 0x1000, Dst: make([]byte, 64)}, {Addr: 0x1ff0, Dst: make([]byte, 32)}}

	allocs := testing.AllocsPerRun(100, func() {
		if err := m.Exec(ctx, 1, 0, writes, reads); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Exec allocates %.1f times per call, want 0", allocs)
	}
}

func TestOnRoundRunsBeforeRound(t *testing.T) {
	m := fake.New()
	p := m.Proc(1).U32(0x1000, 1)
	m.OnRound(func(round int, _ *fake.Memory) {
		if round == 2 {
			p.U32(0x1000, 2)
		}
	})
	read := func() byte {
		reads := []backend.ReadOp{{Addr: 0x1000, Dst: make([]byte, 4)}}
		if err := m.Exec(ctx, 1, 0, nil, reads); err != nil {
			t.Fatal(err)
		}
		return reads[0].Dst[0]
	}
	m.BeginRound(1)
	if v := read(); v != 1 {
		t.Fatalf("round 1 read %d, want 1", v)
	}
	m.BeginRound(2)
	if v := read(); v != 2 {
		t.Fatalf("round 2 read %d, want 2 (changed by OnRound)", v)
	}
}

func TestExecFatalErrors(t *testing.T) {
	m := fake.New()
	if err := m.Exec(ctx, 42, 0, nil, nil); !errors.Is(err, fake.ErrNoProcess) {
		t.Errorf("unknown pid: err=%v, want ErrNoProcess", err)
	}
	m.Proc(1)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.Exec(canceled, 1, 0, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled ctx: err=%v, want context.Canceled", err)
	}
}

func TestProcessInfo(t *testing.T) {
	m := fake.New()
	m.Proc(1).Name("app.exe").Module("Core.dll", 0x7ff600000000, 0x2000)
	m.Proc(2).Name("old.exe").Wow64()

	if pid, err := m.PidByName("APP.EXE"); err != nil || pid != 1 {
		t.Errorf("PidByName = %d, %v; want 1", pid, err)
	}
	if _, err := m.PidByName("none.exe"); !errors.Is(err, fake.ErrNoProcess) {
		t.Errorf("PidByName(unknown) err=%v", err)
	}
	if info, err := m.Info(2); err != nil || !info.Wow64 || !info.X64 {
		t.Errorf("Info(2) = %+v, %v; want Wow64", info, err)
	}
	if base, err := m.ModuleBase(1, "core.DLL"); err != nil || base != 0x7ff600000000 {
		t.Errorf("ModuleBase = %#x, %v", base, err)
	}
	if _, err := m.ModuleBase(1, "none.dll"); !errors.Is(err, fake.ErrNoModule) {
		t.Errorf("ModuleBase(unknown) err=%v", err)
	}
	if _, err := m.Proc(1).Read(0x7ff600001ff8, 8); err != nil {
		t.Errorf("module memory is not mapped: %v", err)
	}
}

func TestPut(t *testing.T) {
	type someStruct struct{ X, Y, Z float32 }
	m := fake.New()
	p := fake.Put(m.Proc(1), 0x1000, someStruct{1, 2, 3})
	got, err := p.Read(0x1000, 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0x40, 0, 0, 0x40, 0x40}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
}
