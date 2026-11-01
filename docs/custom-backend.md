# Custom memory access: writing a Backend

smartmem does not depend on MemProcFS. The core package talks to memory through one small interface, [`backend.Backend`](https://pkg.go.dev/github.com/sergeyzav/smartmem/backend#Backend), and `backend/memprocfs` is only one implementation of it. Anything that can read bytes at an address can be a backend:

- a memory dump or a raw memory image in a file;
- a live Linux process through `/proc/<pid>/mem` or `process_vm_readv`;
- `ReadProcessMemory` on Windows, a debugger, an emulator or a remote agent.

With your own backend you keep everything else: `Loc` and pointer chains, typed reads, rounds, `Plan`, `Scheduler`, `Load` and the error model.

This guide builds a complete backend in three steps. The code is the same as in the runnable examples of package `backend` ([`backend/example_test.go`](../backend/example_test.go)), so it is compiled and tested with the rest of the repository.

## What you implement

| Interface | Required | Gives you |
|---|---|---|
| `Backend` with `Exec` | yes | reads and writes at addresses |
| `ProcessInfo` with `Info`, `PidByName`, `ModuleBase` | no | `Session.ProcessByName`, `Process.Module`, `Process.PEB`, the x64 check |
| `RoundAware` with `BeginRound` | no | a call before each round, for tracing |

## Step 1: Exec

```go
type Backend interface {
	Exec(ctx context.Context, pid uint32, flags uint64, writes []WriteOp, reads []ReadOp) error
}
```

smartmem calls `Exec` once per round for each process, with every write and read of that round. One call is one scatter operation, so a backend that can read many ranges at once (a scatter API, `process_vm_readv`, parallel requests) gets the most out of smartmem.

- `pid` is the process passed to `Session.Process`. A backend for a single process can ignore it.
- `flags` are the MemProcFS read flags set with `smartmem.WithFlags`; other backends can ignore them.
- `writes` run first, then `reads`, as in MemProcFS.
- For each `ReadOp`, fill `Dst` and set `N` to the bytes read, or set `Err` if the range failed.
- For each `WriteOp`, write `Src` at `Addr`, or set `Err`.
- Return an error only if the whole operation could not run (ctx done, connection lost). That fails the whole execution.

Here is a backend over an `io.ReaderAt` whose offset 0 is address `base`:

```go
// errOutside is the error of a range outside the memory the backend has.
var errOutside = errors.New("address outside the image")

type readerAtBackend struct {
	mem  io.ReaderAt
	base uint64
}

func (b *readerAtBackend) Exec(ctx context.Context, _ uint32, _ uint64, writes []backend.WriteOp, reads []backend.ReadOp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range writes {
		w := &writes[i]
		wa, ok := b.mem.(io.WriterAt)
		if !ok {
			w.Err = errors.ErrUnsupported
			continue
		}
		if _, err := wa.WriteAt(w.Src, b.offset(w.Addr)); err != nil {
			w.Err = err
		}
	}
	for i := range reads {
		r := &reads[i]
		if r.Addr < b.base {
			r.Err = errOutside
			continue
		}
		n, err := b.mem.ReadAt(r.Dst, b.offset(r.Addr))
		r.N = n
		switch {
		case n > 0 || err == nil:
			// read in full, or a short read: smartmem fails the requests
			// past N with *PartialReadError
		case errors.Is(err, io.EOF):
			r.Err = errOutside
		default:
			r.Err = err // nothing read: the range fails
		}
	}
	return nil
}

func (b *readerAtBackend) offset(addr uint64) int64 { return int64(addr - b.base) }
```

Use it like any other backend:

```go
be := &readerAtBackend{mem: bytes.NewReader(img), base: 0x10000}
s := smartmem.New(be)
p, _ := s.Process(1) // without ProcessInfo, any pid is a native x64 process

b := s.NewBatch()
v := smartmem.Read[uint16](b, p.Chain(p.At(0x10008), 0))
if err := b.Run(ctx); err != nil { ... }
```

The same type reads a live Linux process, because `*os.File` is an `io.ReaderAt` and `io.WriterAt`:

```go
f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", pid), os.O_RDWR, 0)
if err != nil { ... }
defer f.Close()
s := smartmem.New(&readerAtBackend{mem: f}) // base 0: file offset = address
```

Opening another process's memory needs ptrace access to it: on most systems, root or `CAP_SYS_PTRACE`. Open it read-only (`os.Open`) if you do not write.

## Step 2: ProcessInfo (optional)

Without `ProcessInfo`, `Session.Process` accepts any pid as a native x64 process, and `ProcessByName`, `Process.Module` and `Process.PEB` return `smartmem.ErrUnsupported`. Implement it to make them work:

```go
type imageProcess struct {
	readerAtBackend
	pid     uint32
	name    string
	peb     uint64
	modules map[string]uint64 // lower-case name -> base
}

func (ip *imageProcess) Info(pid uint32) (backend.ProcInfo, error) {
	if pid != ip.pid {
		return backend.ProcInfo{}, fmt.Errorf("no process %d", pid)
	}
	return backend.ProcInfo{PID: pid, X64: true, PEB: ip.peb}, nil
}

func (ip *imageProcess) PidByName(name string) (uint32, error) {
	if !strings.EqualFold(name, ip.name) {
		return 0, fmt.Errorf("no process %q", name)
	}
	return ip.pid, nil
}

func (ip *imageProcess) ModuleBase(pid uint32, name string) (uint64, error) {
	base, ok := ip.modules[strings.ToLower(name)]
	if !ok || pid != ip.pid {
		return 0, fmt.Errorf("no module %q", name)
	}
	return base, nil
}
```

- `Info` is called by `Session.Process`. Processes with `X64` false or `Wow64` true are refused with `ErrUnsupported`. `PEB` is the address returned by `Process.PEB`; leave it 0 if you do not know it.
- `PidByName` is called by `Session.ProcessByName`. Match names case-insensitively, as Windows does.
- `ModuleBase` is called the first time a request uses `Process.Module(name)`. smartmem caches the result in the `Process` until `Process.Refresh`. Its errors reach the request as a `*smartmem.ModuleError`.

## Step 3: RoundAware (optional)

```go
func (b *myBackend) BeginRound(round int) { ... }
```

smartmem calls `BeginRound` before the first `Exec` of every round, starting at 1 for each execution. Use it for tracing or metrics. `backend/fake` uses it to number the rounds in its trace.

## The contract

What smartmem guarantees to your backend:

- Requests merged into one read range lie within one 4 KiB page. A request that itself crosses a page boundary gets a range of its own, which may overlap other ranges.
- `Dst` and `Src` are valid only during `Exec`. Do not keep them after it returns.
- `Exec` may be called from several goroutines at once: independent Batches, Plans and the Scheduler run their own executions. Make it safe for concurrent use.

How smartmem turns your results into the errors of single requests:

| Your backend | The request gets |
|---|---|
| `ReadOp.Err` set | `*smartmem.ReadError`, wrapping your error: `errors.Is` and `errors.As` see it |
| `ReadOp.N` shorter than the request needs | `*smartmem.PartialReadError{Addr, Want, Got}` |
| `WriteOp.Err` set | `*smartmem.WriteError`, wrapping your error |
| a pointer read as 0, or above `0x00007FFFFFFFFFFF` | `*smartmem.NullPointerError` or `*smartmem.BadPointerError` (smartmem checks this itself) |
| `Exec` returns an error | `Batch.Run`, `Plan.Exec` or `Run` returns it, wrapped as `smartmem: backend: ...` (`errors.Is` still sees it), and every pending request fails with it |

One failed range affects only the requests in it, so report errors per range whenever you can, and keep `Exec`'s own error for failures of the whole operation.

## Testing your backend

- Run your code against `backend/fake` first: it shows what smartmem asks for, round by round (`fake.Memory.Trace`), and needs no target.
- Then point the same code at your backend with known memory and compare the values and the errors.
- Run the tests with `-race`, since `Exec` is called concurrently.
- Check the edge cases from the table above: an unreadable range, a short read at the end of readable memory, a write you do not support.

## See also

- [Package `backend`](https://pkg.go.dev/github.com/sergeyzav/smartmem/backend): the interfaces and the contract.
- [`backend/memprocfs`](../backend/memprocfs/memprocfs.go): the MemProcFS backend, with a pool of scatter handles.
- [`backend/fake`](../backend/fake/fake.go): an in-memory backend with tracing and round hooks.
