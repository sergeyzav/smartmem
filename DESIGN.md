# smartmem — design v1

High-level typed access to process memory on top of
[gomemprocfs](https://github.com/sergeyzav/gomemprocfs). The package describes requests
lazily, runs them in rounds of scatter reads, merges requests from different subsystems into
the same rounds, and returns the results as ready Go values and structs.

## 0. Decisions

| Question | v1 decision |
|---|---|
| Load profile | Polling (the same requests every tick) **and** one-shot requests. The core is built around a request graph that can run once (`Batch`) or many times without allocations (`Plan`). |
| Who starts rounds | Both: an explicit `Run/Exec(ctx)` is the core; `Scheduler` with auto-flush is a thin wrapper over it. |
| Memory targets | x64 user-mode processes only. WoW64, kernel, physical memory and VM GPA are out of v1, but the pointer size lives in `Process` and is not hard-coded. |
| Consistency | Atomic publish always; ALAP on by default; chain revalidation is an option. |
| Strings | C string, UTF-16, `UNICODE_STRING`. |
| gomemprocfs | Changed in parallel (section 11). The design relies on a zero-alloc path and sentinel errors. |
| Go | 1.25+ (the minimum inherited from gomemprocfs) |

## 1. Goals and non-goals

**Goals**
- Reading/writing Go primitives, `[N]T`, C structs, pointers, strings.
- Pointer chains with clear semantics.
- Lazy description + execution in rounds: independent requests from different subsystems
  go into one scatter, dependent ones into later rounds.
- Future results that fit both synchronous and concurrent code.
- Composite business structs (`BusinessUnit`) assembled from many memory locations
  and published atomically when the slowest field is ready.
- Polling without allocations in the steady state.
- Unit tests without MemProcFS (pure-Go fake backend, runs on macOS).

**Non-goals for v1**
- Transparent lazy field access (loading on access).
- Chains in struct tags.
- Transactional writes; writes inside a `Plan`.
- Generating structs from PDB / C headers.
- Snapshot isolation (impossible for live memory, see section 8).
- Joining new requests to an execution that is already running (only to the next one).

## 2. Layers and packages

```
smartmem/                  public API: Session, Process, Loc, Future, Batch, Plan,
                           Scheduler, Read/Write/Load, Loader
smartmem/internal/engine   request graph, round planning, dedup/coalescing, ALAP
smartmem/internal/codec    type layout (reflect, cache), decoding/encoding
smartmem/backend           Backend interface + ReadOp/WriteOp types
smartmem/backend/memprocfs implementation over gomemprocfs (the only place with native libraries)
smartmem/backend/fake      in-memory implementation for tests
```

gomemprocfs works through purego, without cgo, so the whole module builds on any OS without a C toolchain.
The core (`smartmem`, `internal/*`, `backend`, `backend/fake`) does not import gomemprocfs, so its tests
need neither the native MemProcFS libraries nor a dump.

## 3. Backend

```go
package backend

type ReadOp struct {
    Addr uint64
    Dst  []byte // engine buffer; the backend writes here
    N    int    // how many bytes were actually read (out)
    Err  error  // the error of this operation only (out)
}

type WriteOp struct {
    Addr uint64
    Src  []byte
    Err  error  // out
}

type Backend interface {
    // Exec runs one scatter operation for one (pid, flags):
    // all writes first, then all reads (VMMDLL_Scatter_Execute semantics).
    // It returns an error only for fatal failures (broken handle, closed vmm);
    // errors of single operations go to ReadOp.Err / WriteOp.Err.
    Exec(ctx context.Context, pid uint32, flags uint64, writes []WriteOp, reads []ReadOp) error
}
```

**memprocfs backend**
- A pool of `ScatterHandle`s not bound to a key: `Clear(pid, flags)` in gomemprocfs
  retargets a handle to another pid/flags, so any free handle fits
  any group. Pool size = the maximum number of concurrent groups (usually 1–2).
- `PrepareWrite` copies the data at once (as documented in gomemprocfs), so the write buffer
  can be reused right after the call.
- `PrepareWrite` → `Prepare` → `Execute` → `ReadInto(va, dst)` (no allocations).
- Always scatter, even for a single read (see section 11).
- Default flags: `MemFlagNoCache`. `ZEROPAD_ON_FAIL` is not set, because
  otherwise an unreadable page cannot be told apart from zeros.

**fake backend**
- A sparse page map `map[pageAddr]*[4096]byte` per pid; unmapped pages give `ErrUnmapped`,
  an unknown pid gives the fatal `ErrNoProcess`.
- A read across an unmapped page: `FailWhole` (default, `N=0`) or `Truncate` (`N` = the readable
  prefix, no error; if no byte is readable, same as `FailWhole`). A write that touches an unmapped
  page writes nothing.
- The `OnRound(func(round int, m *Memory))` hook is called before the first `Exec` of a round and changes memory
  between rounds, to test revalidation and objects that "move".
- A trace and an `Exec` counter. Tests check exactly the number of rounds and scatter calls.
  `SetTrace(false)` turns tracing off, because the trace allocates and gets in the way of allocation tests (T4, P2).

**Rounds in the backend**: the fake needs the round number for the trace and `OnRound`. So `backend` has
an optional interface `RoundAware { BeginRound(round int) }`. The engine calls it before the first `Exec`
of every round, numbered from 1 in each execution. The memprocfs backend does not implement it.

## 4. Addresses: `Loc`

`Loc` is an immutable value that describes "how to get the address". A `Loc` does no I/O itself.

```go
type Loc struct { /* proc *Process; base; ops []op (copy-on-append) */ }

func (p *Process) At(addr uint64) Loc            // static address
func (p *Process) Module(name string) Loc        // module base
func (p *Process) Chain(base Loc, offs ...int64) Loc // sugar, CE notation
func LocOf[T any](f *Future[Ptr[T]]) Loc         // dynamic base from a future

func (l Loc) Add(off int64) Loc
func (l Loc) Deref() Loc   // read a pointer (of size Process.PtrSize) at the current address
func (l Loc) Index(i int, stride int) Loc        // = Add(i*stride)
func (l Loc) Depth() (n int, static bool)        // number of Derefs; static=false for LocOf
```

**Semantics (normative)**
- `At(x).Deref().Add(o)` means: read a u64 at address `x`, get `y`, and the target address is `y+o`.
- `Chain(B, o1, …, on)` ≡ `B.Deref().Add(o1).Deref().Add(o2)…Deref().Add(on)`.
  This matches the Cheat Engine pointer notation `[[[B]+o1]+o2]…+on`: the base
  **is dereferenced**, and `n` offsets give `n` dereferences. With `n=0` it is just `B`.
- A `Read` at a `Loc` of depth `d` takes `d+1` rounds: `d` rounds for pointers and 1 for the leaf.
- A pointer of 0 gives `*NullPointerError{Step: k}`, where `k` is the Deref number (from 0).
- A non-canonical or non-user-mode pointer (> `0x00007FFFFFFFFFFF`) gives `*BadPointerError{Step, Value}`.

**Modules**: the base is resolved through `Vmm.GetModuleBase(pid, name)` before round 1,
outside the scatter. The result is cached in `Process`; `p.Refresh()` invalidates the cache. If the module
is not found, every node that depends on it gets `*ModuleError{Name, Err}`.

## 5. Types (codec)

A type's layout is computed once through `reflect` and cached in a `sync.Map`.
An unsupported type causes a **panic when the request is described**. It is a programmer error,
like `regexp.MustCompile`, and the first test catches it.

| Category | Types | How it is read |
|---|---|---|
| Primitives | `int8…int64`, `uint8…uint64`, `float32/64` | little-endian, unsafe copy |
| `bool` | **not allowed** in blittable types: an invalid byte in a Go `bool` is UB | `CBool` (`uint8`) with a `Bool()` method |
| Arrays | `[N]T`, where T is blittable | one range |
| Blittable struct | fixed-size fields only, no `string`/slice/map/Go pointers/interfaces | one range, unsafe copy |
| Pointers | `Ptr[T]` (`type Ptr[T any] uint64`), `Addr` (`uint64`) | 8 bytes; `Ptr[T].Loc(p)`, `IsNil()` |
| Sparse struct | fields tagged `mem:"0x48"`, the rest have `mem:"-"` | each field is its own range, the engine merges them |
| Strings | `CString(max)`, `UTF16(maxChars)`, `UnicodeString` | see below |
| Dynamic arrays | `ReadSlice[T](s, loc, n)`; a count from a future goes through `Then` | one range of `n*sizeof(T)` |

- A Go struct with the same field types has on amd64 the same layout and
  padding as in C. `#pragma pack` cannot be expressed in Go, so packed structs
  use `[N]byte` fields or sparse tags.
- Optional size check: if the type has a `CSize() uintptr` method, it is compared
  with `unsafe.Sizeof` at registration. A mismatch panics.
- A sparse field must itself be blittable. Nested sparse structs go through `Loader`.
- **Strings**
  - `CString(max)`: reads `max` bytes in one round and cuts at the first `\0`. No `\0` → `ErrNoTerminator` (or a cut with the `Truncate` option).
  - `UTF16(maxChars)`: the same, with a `\0\0` terminator, decoded with `unicode/utf16`.
  - `UnicodeString`: reads `{Length u16, MaximumLength u16, _ [4]byte, Buffer u64}`, then in the next round `Length` bytes from `Buffer`. The length is capped by `Session.MaxStringLen` (default 4096).

## 6. Future

```go
type Future[T any] struct { /* value T; err error; state; done chan (lazy); gen uint64 */ }

func (f *Future[T]) Get() (T, error)                     // does not block; ErrPending if not ready yet
func (f *Future[T]) Wait(ctx context.Context) (T, error) // blocks until ready or ctx
func (f *Future[T]) Done() <-chan struct{}               // closed once; created lazily
func (f *Future[T]) Ready() bool
func (f *Future[T]) Gen() uint64                         // the number of the Plan execution that produced the value
```

- There is no channel per value (`chan T`). The `done` channel is closed once
  (like `context.Done()`), the value is kept in a field, and `close` gives
  happens-before. Many waiters work with `select`.
- After `Batch.Run` all futures are ready, `Get()` does not block, and no channel is created.
- Combinators are free functions, because Go methods cannot have type params:

```go
func Map[T, U any](f *Future[T], fn func(T) (U, error)) *Future[U]
func Then[T, U any](f *Future[T], fn func(s Scope, v T) *Future[U]) *Future[U] // dependent request
func All[T any](fs ...*Future[T]) *Future[[]T]
```

- Continuations (`Map`, `Then`, `Loader` bodies) run on the engine goroutine as soon as
  the result is ready, between the `Exec`s of a round. They must be fast and must not block.
  New requests they create always go into the next round of the same
  execution (E15). `Then` must return a future created in the given `Scope`, or `nil`
  (then the result is the zero value).

## 7. Scope: Batch, Plan, Scheduler

```go
type Scope interface{ scope() *engine.Graph } // sealed

func Read[T any](s Scope, loc Loc) *Future[T]
func ReadSlice[T any](s Scope, loc Loc, n int) *Future[[]T]
func ReadString(s Scope, loc Loc, kind StringKind) *Future[string]
func Write[T any](s Scope, loc Loc, v T) *Future[struct{}] // Batch and Scheduler; in a Plan → panic

func ReadNow[T any](ctx context.Context, loc Loc) (T, error)   // a Batch of one request
func WriteNow[T any](ctx context.Context, loc Loc, v T) error
```

### Batch — one-shot execution
```go
b := s.NewBatch()
hp  := smartmem.Read[int32](b, base.Deref().Add(0x100))
pos := smartmem.Read[Vec3](b, p.Chain(base, 0x30, 0x90))
if err := b.Run(ctx); err != nil { /* fatal error */ }
v, err := hp.Get()   // the error of this request
```
- `Run` can be called once. A second call returns `ErrBatchDone`.
- `Run` returns an error only for fatal failures (ctx, backend). Errors of single
  requests live in their futures.
- Not goroutine-safe while describing.

### Plan — repeated execution (polling)
```go
plan := s.NewPlan()
hp      := smartmem.Read[int32](plan, local.Add(0x100))
players := smartmem.LoadEach[Player](plan, smartmem.ReadSlice[smartmem.Ptr[Player]](plan, list, 64))

for range ticker.C {
    if err := plan.Exec(ctx); err != nil { ... }
    plan.View(func() {           // consistent read of the values of one Exec
        v, _ := hp.Get()
        ps, _ := players.Get()
    })
}
```
- The graph is frozen on the first `Exec`. Adding requests after that panics.
- Buffers, nodes, scatter handles and `Loader` staging copies are allocated once and
  reused. **Acceptance criterion:** a static Plan gives `0 allocs/op` in
  `BenchmarkPlanExec` (fake backend and memprocfs with `ReadInto`).
- Dynamic parts (`Then`, `LoadEach`) take nodes from Plan pools that
  are reset on every `Exec`. `LoadEach` reuses slots by index. With a
  stable element count, allocations ≈ 0. `Then` with new futures
  allocates, and this is documented.
- Results are published under the Plan's write lock. `Get()`/`View` take the read lock. So
  values read in one `View` always come from one `Exec`.

### Merging several scopes into one execution
```go
err := smartmem.Run(ctx, planA, planB, batchC) // shared rounds for all
```

### Scheduler — auto-flush for subsystems on different goroutines
```go
sch := smartmem.NewScheduler(s,
    smartmem.FlushWindow(500*time.Microsecond), // wait after the first request
    smartmem.FlushMax(4096))                    // or until N requests are queued
defer sch.Close()

sch.Attach(plan, 16*time.Millisecond) // a periodic Plan, merged with ad-hoc requests

// any goroutine:
hp, err := smartmem.Read[int32](sch, loc).Wait(ctx)
```
- `Scheduler` is a goroutine-safe scope. Its loop: wait for the first request → wait for the window or
  the limit → take the queue together with the Plans that are due → call one `smartmem.Run` → close the futures.
- Requests that arrive during an execution go into the next flush.
- This is how the use case "subsystem A reads X, subsystem B follows the chain Y→Z" runs:
  X and Y in round 1, Z in round 2, and 2 scatters in all.

## 8. Engine

An execution is a loop of rounds while there are pending nodes:

1. **Ready nodes**: the address is known, all Derefs of the previous step are done.
   ALAP can delay a node (see 9).
2. **Grouping** by `(pid, flags)`.
3. **Dedup**: identical `(pid, flags, addr, size)` are read once, and the result
   goes to every subscriber.
4. **Coalescing**: requests are sorted by address and merged if the gap ≤
   `MaxGap` (64 bytes by default) **and the merged range does not cross a 4 KiB page boundary**.
   This way an unreadable page fails only the requests on it. A request that itself
   crosses a page boundary is prepared as is and not merged.
5. **Exec** of the backend per group, sequentially. v1 does not run groups in parallel.
   Within a round, writes run before reads.
6. **Distributing results**: copying/decoding into node buffers, errors of single nodes
   (`*PartialReadError{Addr, Want, Got}`, `*ReadError` wrapping the backend error),
   resolving futures, running continuations, which create the nodes of the next round.
7. After `MaxRounds` (16 by default) the remaining nodes get `ErrMaxRounds`. This protects against cyclic lists.

**Writes through a chain**: the chain is resolved in the earlier rounds, and the write goes in the round where
the address became known. There is no read-after-write order between separate requests of one
execution, other than the rule "writes before reads within a round". If you need
RAW, use a separate `Run`.

**Write success**: scatter does not report whether each single write succeeded, so a write future
resolves without an error if `Execute` succeeded. v1 has no "was it written" check:
on a live target the value can be overwritten by the target itself before the re-read, and the check would give
a false error. Whoever needs it reads the address in the next `Run`.

**Concurrency**: `Session` is safe for parallel executions. Handles come from
a pool, and each execution has its own. Describing a `Batch`/`Plan` is single-threaded,
`Scheduler` is multi-threaded.

## 9. Composite structs (BusinessUnit) and consistency

```go
type Binder interface{ Bind(l *Loader, base Loc) }

type Player struct {
    HP   int32
    Team uint8
    Pos  Vec3
    Name string
}

func (p *Player) Bind(l *smartmem.Loader, base smartmem.Loc) {
    smartmem.Field(l, &p.HP,   base.Add(0x100))
    smartmem.Field(l, &p.Team, base.Add(0x13C))                          // merges with HP
    smartmem.Field(l, &p.Pos,  base.Add(0x30).Deref().Add(0x90))
    smartmem.FieldString(l, &p.Name, base.Add(0x50).Deref(), smartmem.CString(32))
}

func Load[T any, PT interface{ *T; Binder }](s Scope, base Loc, opts ...LoadOption) *Future[T]
func LoadEach[T any, PT interface{ *T; Binder }](s Scope, p *Process, ptrs *Future[[]Ptr[T]], opts ...LoadOption) *Future[[]T]
func Field[T any](l *Loader, dst *T, loc Loc, opts ...FieldOption)
func FieldString(l *Loader, dst *string, loc Loc, kind StringKind, opts ...FieldOption)
func Sub[T any, PT interface{ *T; Binder }](l *Loader, dst *T, base Loc) // nested Binder

p1 := smartmem.Load[Player](b, entity)          // PT is inferred
```

- `Bind` is called **once** at registration (for `LoadEach`, once per element slot).
  It describes the shape, not the data, and cannot depend on values read. In v1 `Loader` **is not**
  a `Scope`: all fields are static. Reads that depend on data go through `Then` outside `Bind`.
- `LoadEach` takes a `*Process`, because `[]Ptr[T]` does not know which process it belongs to.
  An element's base is a `Loc` with a cell base, which the engine fills with the pointer before starting the slot.
- `Read[T]` for a sparse struct (`mem` tags) uses the same mechanism: each field is
  a separate chain from `loc + offset`, and the publish is atomic.
- `Loader` writes into a **staging copy** of `T`. The future resolves only when every field
  is done. Then the staging copy is copied into the published value. Nobody sees a half filled
  struct.
- The first error of a non-`Optional` field fails the struct with `FieldError{Loc, Err}`, and the rest of its nodes
  are skipped (fail-fast), so there is one error, no `errors.Join`.
  `Optional()` on a field means its error does not fail the struct, and the field stays zero.
- `LoadEach`: by default nil pointers and failed elements
  are skipped. `Strict()` fails the whole result.
- Fields can be in different processes. The engine groups by pid either way.

**Consistency guarantees** (instead of an unreachable atomic snapshot):

1. **Atomic publish**: a value is seen either whole or as an error.
2. **ALAP** (on by default, `WithALAP(false)` turns it off): within one
   `Loader`, `D` is the maximum static depth of its fields. A chain of depth `d`
   starts in round `r0 + (D − d)`, where `r0` is the round when the `Loader` base is known.
   Then **all leaves are read by one scatter in the last round**. Latency does not
   grow, because it is `D+1` rounds anyway. Fields with dynamic depth (`LocOf`, `Then`)
   are not part of `D` and are read as soon as they are ready. The guarantee is weaker for them, and this is documented.
3. **Revalidation** (`Revalidate(retries)` on `Load`): in the last round, together with
   the leaves, all pointer slots of the chains are read again. A mismatch means the object moved
   or was freed: the chain is then repeated up to `retries` times, after which it fails with
   `ErrUnstable`. Without mismatches it costs 8 bytes per Deref and zero extra rounds.

## 10. Errors

| Error | When |
|---|---|
| `ErrPending` | `Get()` before the value is ready |
| `*NullPointerError{Step}` | a Deref read 0 |
| `*BadPointerError{Step, Value}` | a non-canonical or non-user-mode pointer |
| `*PartialReadError{Addr, Want, Got}` | the backend read fewer bytes |
| `*ReadError{Addr, Err}` / `*WriteError` | a backend error for the operation (wraps a gomemprocfs sentinel) |
| `ErrMaxRounds` | `MaxRounds` exceeded |
| `ErrUnstable` | revalidation did not settle |
| `ErrNoTerminator` | a string without a terminator within `max` |
| `ErrBatchDone` | a second `Batch.Run` |
| `*FieldError{Loc, Err}` | the wrapper in `Loader` |
| `ctx.Err()` | cancellation; pending futures get this error |

All errors work with `errors.Is/As`.

## 11. Dependency on gomemprocfs

Everything v1 needs is in the `v0.3.0` tag:

| What | Where | How smartmem uses it |
|---|---|---|
| `ScatterHandle.ReadInto(va, dst) (int, error)` | `scatter.go` | the main read path without allocations; `n < len(dst)` → `PartialReadError` |
| Sentinel errors `ErrClosed`, `ErrReadFailed`, `ErrWriteFailed`, `ErrScatterFailed` | `errors.go` | `ErrReadFailed` → an error of one request; `ErrClosed`/`ErrScatterFailed` on `Execute`/`Clear` → a fatal execution error |
| `size == 0` no longer panics | `rw.go`, `scatter.go` | the engine does not generate zero-size requests anyway |
| `PidPhysical`, fixed doc of `PidProcessWithKernelMemory` | `options.go` | not used in v1 (x64 user mode only) |
| `GetModuleBase`, `GetPidByName`, `GetProcessInfo` | `process.go` | `Process.Module`, `ProcessByName`, the x64 check |
| `Clear(pid, flags)` retargets a handle | `scatter.go` | a shared handle pool |

Consequences for the design:
- `ReadInto` passes a Go buffer to native code (through purego) only for the duration of a synchronous call,
  so the rules for passing Go pointers hold and no pinning is needed.
- `MemReadEx` still allocates, so v1 has **no** "single read through `MemReadEx`" optimization:
  the backend always goes through scatter. If a benchmark shows that scatter is noticeably
  more expensive for one request, `MemReadExInto` should be added to gomemprocfs.
- The native library reports only "success/failure", so `ReadError` does not explain the cause
  (an unreadable page or an invalid address). This is documented.
- Deferred: zero-copy `PrepareEx` (writing straight into the caller's buffer during `Execute`, needs `runtime.Pinner` or a C arena).

## 12. Session and options

```go
s := smartmem.New(backend, opts...)   // backend.Backend; memprocfs.New(vmm) for the real one
p, err := s.Process(pid)              // checks that the process is x64
p, err := s.ProcessByName("app.exe")  // GetPidByName
```

`Process` calls `GetProcessInfo` once. If `MemoryModel != MemoryModelX64` or
`Win.IsWow64 != 0`, it returns `ErrUnsupported`. `PtrSize` is kept in `Process`
(always 8 in v1), so that WoW64 in v2 does not touch the engine.

For this, `Backend` has an optional interface `ProcessInfo`
(`Info(pid) (ProcInfo, error)`, `ModuleBase(pid, name) (uint64, error)`).
The fake backend implements it with a module table.

| Option | Default |
|---|---|
| `WithFlags(uint64)` | `MemFlagNoCache` |
| `WithMaxRounds(int)` | 16 |
| `WithMaxGap(int)` | 64 |
| `WithMaxStringLen(int)` | 4096 |

## 13. Testing

### 13.1 Principles

- **A test checks a concept, not the implementation.** The key properties of the package (number
  of rounds, merging, error isolation, atomic publish, no allocations) are checked
  explicitly, through a record of what the engine actually sent to the backend (the trace).
- **No dump and no native libraries.** Everything except `backend/memprocfs` is tested on `backend/fake`
  on any OS. Tests with a dump skip themselves if the environment variables are not set (as in gomemprocfs).
- **A reference implementation.** The engine is compared with a naive sequential resolver
  (`FuzzEngineVsNaive`). This is the main safety net for all optimizations.
- **Deterministic time.** The Scheduler is tested in `testing/synctest` (Go 1.25): timers
  are virtual, tests have no `sleep`, and leaked goroutines fail the test.
- Every test has an ID (C1, E2, …). The stage acceptance criteria in section 16 refer to these IDs.

### 13.2 Test infrastructure

**`backend/fake`** is a public package, because users will test their `Bind` methods without a dump:

```go
mem := fake.New()
p := mem.Proc(100)                          // an x64 process
p.Module("client.dll", 0x7FF6_0000_0000, 0x10000)
p.U64(0x1000, 0x5000)                       // a pointer
p.I32(0x5010, 42)
p.Bytes(0x6000, []byte("hello\x00"))
p.Unmap(0x9000)                             // the whole page is unreadable
mem.SetPartialRead(fake.FailWhole)          // or fake.Truncate (see M7)
mem.OnRound(func(round int, m *fake.Memory) { /* changes between rounds */ })

s := smartmem.New(mem)
```

**Trace** is a canonical text record of all `Exec`s: one line per `Exec`, writes first,
then reads, both sorted by address:

```
r1 pid=100 R 0x1000+8 R 0x2000+8
r2 pid=100 R 0x5010+4
```

Tests compare the trace with an inline string (`assertTrace(t, mem, "...")`, whitespace is normalized).
This way the concept is visible right in the test, and no separate golden files are needed.

**Other**:
- `internal/testutil.RaceEnabled` (build tag `race`): allocation tests skip under `-race`,
  because the race detector allocates itself.
- `naiveResolve(mem, query)` is the reference: it reads each step of a chain with a separate access to the fake, without the engine.

### 13.3 Key tests

**Fake backend (T)**: the fake must be correct, because the rest of the tests stand on it.

| ID | What it checks |
|---|---|
| T1 | `Exec` runs writes before reads (as `VMMDLL_Scatter_Execute` does) |
| T2 | an unmapped page fails only its own ops; `FailWhole` and `Truncate` modes |
| T3 | the trace format is stable (sorting, writes first) |
| T4 | `Exec` without allocations in the steady state (`AllocsPerRun == 0`); P2 is impossible without it |

**Codec (C)**

| ID | What it checks |
|---|---|
| C1 | the type table: allowed types are accepted; `bool`, `string`, slice, Go pointer, `uintptr`, map, interface → panic with the type name and the path to the field |
| C2 | the layout matches C: `struct{int8; int64}` = 16 bytes, offset 8; `Vec3` = 12; nested arrays; hand-written C offsets |
| C3 | encode→decode roundtrip for all primitives and `[N]T` (fuzz) |
| C4 | strings: `CString` with a terminator, without one (`ErrNoTerminator` or `Truncate`), exactly `max`; `UTF16` with surrogate pairs and an odd length; `UnicodeString` with `Length > MaxStringLen`, an odd `Length`, `Buffer == 0` |
| C5 | `CSize()` does not match `unsafe.Sizeof` → panic |

**Loc (L)**

| ID | What it checks |
|---|---|
| L1 | `Chain(B, o1, o2)` gives the same trace as `B.Deref().Add(o1).Deref().Add(o2)` |
| L2 | CE semantics on the fake with hand-placed pointers: `Chain(B)` = `B`; `Chain(B, o)` = `[B]+o` |
| L3 | zero at step k → `NullPointerError{Step: k}`, and the trace shows that the steps after k **were not read** |
| L4 | a non-canonical / kernel pointer → `BadPointerError` |
| L5 | immutability: `a := b.Add(1); x := a.Deref(); y := a.Add(2)` — `x` and `y` do not affect each other (the classic bug of a shared backing array on append) |

**Engine (E)**: the heart of the concept.

| ID | Scenario | Expected |
|---|---|---|
| E1 | 3 independent reads | 1 round, 1 `Exec` |
| E2 | **the motivating case**: A reads X, B follows the chain Y→Z | the trace is exactly `r1: X, Y` / `r2: Z` |
| E3 | the same range from 3 futures | 1 `ReadOp`, all three got the value |
| E4 | coalescing: gap ≤ MaxGap on one page / across a page boundary / gap > MaxGap / a request crosses a page | 1 / 2 / 2 ops / a separate op, not merged |
| E5 | one of the round's addresses is on an unmapped page | an error only in the futures on that page, the rest succeed |
| E6 | a partial read (`Truncate`) | `PartialReadError{Want, Got}` |
| E7 | a cyclic list through a recursive `Then` | `ErrMaxRounds`, `Run` finishes |
| E8 | a fatal backend error in round 2 | `Run` returns it, **all** pending futures get an error, no `Wait` hangs |
| E9 | ctx cancelled between rounds | pending → `ctx.Err()`, no new `Exec` |
| E10 | write X and read X in one round | the read sees the new value (writes before reads) |
| E11 | two writes with a 1-byte gap | 2 `WriteOp`s, the byte in the gap is unchanged |
| E12 | a write through a chain of depth 2 | `W` in round 3 at the resolved address |
| E13 | requests in two processes | one `Exec` per pid in each round |
| E14 | determinism: the same set of requests in 100 random registration orders | the same trace |
| E15 | `Then` creates a request during round k | the request appears in round k+1, not earlier |

**Future (F)**

| ID | What it checks |
|---|---|
| F1 | `Get` before `Run` → `ErrPending`; after it, the value |
| F2 | `Done()` after resolve does not allocate (a global closed channel) |
| F3 | 100 goroutines in `Wait` are all released (`-race`) |
| F4 | `Wait` respects ctx cancellation |
| F5 | `Map`/`Then`: the error propagates; `count → ReadSlice(count)` takes 2 rounds |
| F6 | stress: concurrent `Done()` and resolve, 10k iterations under `-race`, no missed closes |

**Loader / BusinessUnit (B)**

| ID | What it checks |
|---|---|
| B1 | **atomic publish**: in `OnRound` after round 1, `Get()` gives `ErrPending`, although some fields are already read |
| B2 | a field error → `FieldError` with the `Loc`; an `Optional` field → zero, the struct is published |
| B3 | fail-fast: after an error the remaining fields are not read (trace) |
| B4 | **ALAP**: the `Player` example from A.6 gives exactly the trace from the right column of the table; `WithALAP(false)` the one from the left; the number of rounds is the same |
| B5 | ALAP with a dynamic base (`LoadEach` after `ReadSlice`): the leaves are in round `r0 + D` |
| B6 | **revalidation**: `OnRound` repoints a pointer between rounds → retry, the value comes from the new object; a change every round → `ErrUnstable` after N attempts |
| B7 | revalidation without changes: zero extra rounds, the checks are merged into existing spans (trace) |
| B8 | a nested `Sub` |
| B9 | `LoadEach`: nil is skipped, a failed element is dropped; `Strict()` fails everything |
| B10 | `Bind` is called exactly once per registration, not on every `Exec` |

**Plan (P)**

| ID | What it checks |
|---|---|
| P1 | two `Exec`s with a memory change between them → new values, `Gen` grows |
| P2 | **zero-alloc**: `AllocsPerRun(100, plan.Exec) == 0` for a static Plan. This is a regular test, not just a benchmark, so a regression fails CI |
| P3 | an unchanged string does not allocate; a changed one does |
| P4 | `LoadEach` with a stable count: `Bind` is not called again, and only the result slices are allocated (the pointer list and `[]T`), i.e. ≤ 2 per `Exec` |
| P5 | **`View` consistency**: the fake keeps fields A and B always equal within one `Exec`; a reader in `View` checks `A == B` in a loop (`-race`) |
| P6 | a request after freeze → panic; `Write` in a Plan → panic |
| P7 | parallel `Exec`s are serialized, without races |

**Scheduler (S)**: all in `synctest.Test`.

| ID | What it checks |
|---|---|
| S1 | **the motivating case across goroutines**: two goroutines within `FlushWindow` (X and Y→Z) → one `Run`, the trace as in E2 |
| S2 | a request after the window → a separate `Run` |
| S3 | `FlushMax` fires at once, without waiting for the window |
| S4 | `Attach(plan)` + an ad-hoc request → shared rounds |
| S5 | `Close` → pending get `ErrClosed`, the loop has finished (synctest catches leaked goroutines) |
| S6 | a request during an execution is not lost, it goes into the next flush |

**Fuzz (Z)**: the seed corpus runs in a regular `go test`.

| ID | Invariants |
|---|---|
| Z1 `FuzzEngineVsNaive` | random memory (objects, pointers, zeros, unmapped pages, cycles) + random requests (depth 0–4, sizes, offsets, Loader groups, ALAP on/off): values and error types (with `Step`) == `naiveResolve`; rounds == max depth + 1; ≤ 1 `Exec` per pid in a round; ALAP does not change values on static memory |
| Z2 `FuzzCoalesce` | each request is fully covered by exactly one span; a span does not cross a page (except a single request that crosses it itself); spans do not overlap |
| Z3 `FuzzDecodeString` | no panic on any bytes; the result ≤ `max` |

**MemProcFS integration (M)**: `backend/memprocfs`, skipped without `VMM_DLL_PATH` / `MEMDUMP_PATH`.
The tests rely on Windows invariants, not on specific addresses in the dump.

| ID | What it checks |
|---|---|
| M1 | **differential**: N random ranges in a module image — `Batch` gives the same bytes as `vmm.MemRead` |
| M2 | PE header: `[base] == "MZ"`, `[base+0x3C]` = `e_lfanew`, `Then` → `[base+e_lfanew] == "PE\0\0"` |
| M3 | **walking PEB→Ldr→InLoadOrderModuleList** (`PEB+0x18`, `Ldr+0x10`, entry: `DllBase +0x30`, `BaseDllName +0x58` `UNICODE_STRING`) through `Loader` + `Then`; names and bases == `GetModuleList`; the walk stops at the list head, not at `ErrMaxRounds` |
| M4 | `Process` accepts x64; a WoW64 process, if the dump has one, → `ErrUnsupported` |
| M5 | `Plan.Exec` on the real backend: 0 allocations |
| M6 | an invalid address → `ReadError`, `errors.Is(err, memprocfs.ErrReadFailed)` |
| M7 | **exploratory**: a read across a mapped/unmapped page boundary records the real scatter behavior (`cbRead` < `cb` or an error). Its result sets the default of `fake.SetPartialRead` |

**Examples (X)**: `example_test.go` with `// Output:` on the fake backend: `ExampleRead`, `ExampleChain`,
`ExampleLoad`, `ExamplePlan`, `ExampleScheduler`. They are both tests and the source of the README examples.

## 14. CI and linters (the basic minimum)

### 14.1 `ci.yml` — required, on every push to `main` and every PR

| Job | OS | Steps |
|---|---|---|
| `lint` | ubuntu | `golangci-lint-action@v8`, version `v2.15.0` (as in gomemprocfs) |
| `test` | ubuntu | `go mod tidy -diff` → `go build ./...` → `go test -race -shuffle=on -count=1 -coverprofile=cover.out ./...` → total coverage in the log |
| `test` | macos, windows | `go build ./...` → `go test -count=1 ./...` (no `-race`: ubuntu is enough, and on the others it needs a C toolchain) |

- `go-version-file: go.mod`; `go 1.25` (the minimum inherited from gomemprocfs).
- Tests with a dump (M) skip themselves here, so CI does not depend on secrets.
- Fuzzing in CI runs only the seed corpus. Long fuzzing runs locally (`go test -fuzz=Z1 -fuzztime=5m ./internal/engine`).
- Benchmarks do not run in CI, because they are noisy. Tests P2/P4/T4 check the allocation guarantees.
- Required checks for merging into `main`: `lint`, `test (ubuntu)`, `test (macos)`, `test (windows)`.

### 14.2 `integration.yml` — added in stage 7

A copy of the gomemprocfs `test.yml` approach: a matrix of the two latest MemProcFS releases, a dump
from GCS with a cache, `VMM_DLL_PATH` / `MEMDUMP_PATH`, running `go test -count=1 ./backend/memprocfs/...`.
Does not run for PRs from forks. At first it is an optional check.
Prerequisite: the smartmem repo needs `secrets.GCP_WORKLOAD_IDENTITY_PROVIDER`,
`secrets.GCP_SERVICE_ACCOUNT` and `vars.GCS_MEMDUMP_PATH`, and the WIF provider must allow
the `sergeyzav/smartmem` repository (attribute condition). Compatibility badges as in gomemprocfs, optionally.

### 14.3 `.golangci.yml`

As in gomemprocfs, plus `errorlint`: the design relies on `errors.Is/As` and `%w`.
`unsafeptr` stays **enabled**, because smartmem needs no `uintptr → unsafe.Pointer` conversions.

```yaml
version: "2"
run:
  tests: true
linters:
  default: none
  enable: [errcheck, govet, ineffassign, staticcheck, unused, nilnil, nilerr, errorlint]
  settings:
    govet:
      enable-all: true
      disable:
        - fieldalignment # blittable structs mirror C, fields cannot be reordered
        - shadow
  exclusions:
    generated: lax
    paths: [.claude]
    presets: [std-error-handling]
formatters:
  enable: [gofumpt]
  exclusions:
    paths: [.claude]
issues:
  max-issues-per-linter: 0
  max-same-issues: 0
```

## 15. README

In English, in the gomemprocfs style. Every code snippet in the README has a matching
`ExampleXxx` in `example_test.go`, so the examples compile and are checked in CI.
The README grows with the stages: stage 0 has only the title, status and Development,
the rest appears when the matching API is ready.

1. Title, CI badge (later Integration and MemProcFS compatibility), a one-line description (the current one).
2. **Status**: experimental, the API may change until v1.0.
3. **Features**: typed reads/writes, pointer chains, scatter rounds, allocation-free `Plan`, `Loader`, `Scheduler`.
4. **Requirements**: Go 1.25+, x64 processes only. The native MemProcFS libraries are needed only for
   `backend/memprocfs` (a link to the gomemprocfs Requirements).
5. **Install**.
6. **Quick start**: `Batch` + `Read` + `Chain` (= `ExampleChain`).
7. **Concepts**: `Loc` and CE semantics (the table `Chain(B, o1, o2)` ≡ `[[B]+o1]+o2`); a round diagram; Future.
8. **Polling with Plan** (= `ExamplePlan`) + `View`.
9. **Business units**: `Bind` / `Load` / `LoadEach` (= `ExampleLoad`).
10. **Concurrent subsystems**: `Scheduler` (= `ExampleScheduler`).
11. **Consistency guarantees**: the three guarantees from section 9 and a plain "what is not guaranteed".
12. **Errors**: the table from section 10.
13. **Testing your code**: `backend/fake` for testing your own `Bind` methods without a dump.
14. **Development**: `go test ./...`, `golangci-lint run`, environment variables for the integration tests.
15. License.

## 16. Implementation order and acceptance criteria

A stage is done when CI is green and the listed tests exist and pass.

| # | Stage | Done when |
|---|---|---|
| 0 | Repo skeleton: `go.mod` (`go 1.25`), `doc.go`, `internal/testutil.RaceEnabled`, `.golangci.yml`, `ci.yml`, README (title, status, Development). `gomemprocfs v0.3.0` is added to `go.mod` in stage 7, because `go mod tidy` removes it until the first import | CI is green on 3 OSes |
| 1 | ~~gomemprocfs~~ | done (`v0.3.0`) |
| 2 | `codec`, `Loc`, `backend`, `backend/fake` (with trace and `OnRound`), minimal `Session`/`Process` (`Session` options in stage 3) | T1–T4, C1–C5, L1 (structurally: `Chain` and `Deref/Add` give the same `Loc`; on the trace level together with the engine), L5, Z3 |
| 3 | `engine`, `Future`, `Batch`, `Write` | L2–L4, E1–E15, F1–F6, Z1, Z2, `ExampleRead`, `ExampleChain` |
| 4 | `Loader`, `Bind`, `Sub`, `LoadEach`, ALAP, revalidation | B1–B10, `ExampleLoad` |
| 5 | `Plan`: freeze, pools, string reuse, `View` | P1–P7, `ExamplePlan` |
| 6 | `Scheduler`, `smartmem.Run` | S1–S6, `ExampleScheduler` |
| 7 | `backend/memprocfs`, `integration.yml`, the full README | M1–M7; the `fake.SetPartialRead` default from M7; tag `v0.1.0` |

## 17. Deferred to v2

WoW64 (`Ptr32[T]`), kernel, physical memory, VM GPA; lazy fields; writes in a `Plan`;
joining requests to a running execution; zero-copy `PrepareEx`; generating
structs from PDB/C headers; maybe `WriteVerify` (re-reading what was written), if a real use case appears.

---

# Appendix A. Internals (for the implementation)

## A.0 How it is implemented (stage 3) and how it differs from the sketch below

Sections A.1–A.9 are the initial sketch. The implementation differs in these places:

- **Nodes are linked by pointers** (`*engine.Node`), not by indexes into a slab. A node's state is just its address
  and the round it can run in; there is no separate state machine. A Plan reuses the same
  nodes between `Exec`s, so no template needs to be copied.
- `internal/engine` knows nothing about `Loc`, `Future` or the codec: it runs chains
  `Deref → … → Leaf|Write` and hands the bytes to a `Sink`. Compiling a `Loc` into nodes, futures and decoding
  live in the root package.
- `Node.Delay` (ALAP), `Node.Skip` (nodes of a failed Loader) and `Node.Observer` (revalidation sees
  the pointers read) are the hooks for stage 4.
- The writes and reads of a group are separated explicitly, not through the sort order.
- Coalescing: a request that itself crosses a page boundary gets its own span and ends the current one.
- A fatal error, a cancelled ctx and `MaxRounds` give the error to every node in the queue, including
  those that sinks add while handling errors. So no future is left hanging.
- **`publishLock`** is a reader-preferring RW lock on a `sync.Cond`. `sync.RWMutex` does not fit:
  `Get` inside `Plan.View` takes the read lock again, and with `RWMutex` that deadlocks if a writer is already waiting
  between the two `RLock`s.
- A single `engine.Run` pool per Session (`sync.Pool`), so round buffers are reused.

## A.1 Request graph

Each public call (`Read`, `Field`, `Write`, `Then`…) compiles into
graph nodes. A `Loc` of depth `d` expands into `d` `deref` nodes and one leaf.

```go
type nodeKind uint8
const (
    kDeref nodeKind = iota // reads 8 bytes, result + off = the address of the next node
    kLeaf                  // reads size bytes, decodes into a future / a Loader field
    kWrite                 // writes bytes at an address known after the derefs
    kCont                  // Map/Then: no I/O, runs when the parent future is ready
)

type nodeState uint8
const (
    sWaiting  nodeState = iota // the address is not known yet
    sReady                     // the address is known, waits for a round
    sDeferred                  // the address is known, but ALAP: not before notBefore
    sInflight                  // included in the current round
    sDone
    sFailed
    sSkipped                   // the Loader already failed, no I/O needed
)

type node struct {
    kind      nodeKind
    state     nodeState
    pid       uint32
    flags     uint64
    addr      uint64   // valid from sReady
    off       int64    // for kDeref: Add after the dereference
    size      uint32
    step      uint16   // the Deref number in the chain (for NullPointerError.Step)
    notBefore uint16   // ALAP: the minimum round (relative to the start of the execution)
    next      nodeID   // the next node of the chain (0 = none)
    sink      sink     // where to hand the leaf result: a future, a Loader field, a revalidation check
    loader    *loaderState
    err       error
}
```

- Nodes live in a slab `[]node`, linked by `nodeID uint32` indexes, not pointers.
  This makes a reset in a Plan cheap (a template copy) and gives the GC less work.
- A chain is linear (`next`), so no dependency counter is needed: a deref
  sets `addr` on the next node and moves it to `sReady`.
- `kCont` is bound to a future, not to an address. When the future resolves, the engine puts
  the continuation into the `conts` queue and runs it at the end of the round.

**Node life cycle**

```
sWaiting ──addr known──► sReady ──ALAP──► sDeferred ──round≥notBefore──┐
                             │                                         ▼
                             └────────────────────────────────► sInflight ──► sDone
                                                                      └─────► sFailed ─► the chain's next is sFailed too
                                                                               (wrapped with Step)
```

## A.2 The algorithm of one execution

```
exec(ctx, graphs...):
    resolveModules(graphs)                  // GetModuleBase, cached in Process
    ready := roots with a known address
    for round := 1; there are ready or deferred; round++ {
        if round > MaxRounds { failPending(ErrMaxRounds); break }
        if err := ctx.Err(); err != nil { failPending(err); return err }

        cur := ready with notBefore ≤ round (+ deferred that are due)
        ready = ready[:0]

        for g in groupBy(cur, pid, flags) {   // usually one group
            spans := coalesce(g.reads)         // A.3
            ops   := spans→ReadOp, g.writes→WriteOp
            if err := backend.Exec(ctx, g.pid, g.flags, ops.w, ops.r); err != nil {
                failPending(err); return err   // fatal
            }
            for span in spans { for n in span.nodes { complete(n, span.buf[n.addr-span.start:][:n.size], span.err) } }
        }
        runConts()                             // Map/Then/Loader publishes; new nodes → ready
        checkRevalidation()                    // A.6
    }
```

`complete(n, b, err)`:
- `kDeref`: `v := binary.LittleEndian.Uint64(b)`. `v==0` gives `NullPointerError{n.step}`.
  `v > 0x00007FFFFFFFFFFF` gives `BadPointerError`. Otherwise `next.addr = v + n.off`, and `next` goes
  to `ready` (or to `deferred` under ALAP).
- `kLeaf`: `codec.decode(sink.dst, b)`, then `sink.resolve()`. For a Loader this is `loader.pending--`.
- Error: the node and all nodes further along `next` move to `sFailed`, and the sink gets the error.

New nodes that appear during a round (from `Then`/`LoadEach`) always go into
the **next** round. This keeps a round deterministic.

## A.3 Coalescing and buffers

```
coalesce(reqs):
    slices.SortFunc(reqs, byAddr)              // reqs is a reused slice
    for r in reqs:
        crossPage := page(r.addr) != page(r.addr+r.size-1)
        if crossPage { emit a separate span; continue }
        if cur != nil && r.addr <= cur.end+MaxGap && page(r.addr) == page(cur.start) {
            cur.end = max(cur.end, r.addr+r.size); cur.nodes += r
        } else { emit(cur); cur = newSpan(r) }
```

- Dedup comes for free: identical ranges land in one span.
- Each span gets a piece of the **round arena** (a `[]byte` that grows to the maximum and never shrinks).
  The arena is reset at the start of every round: results are decoded right after
  the `Exec`, so there is no need to keep it longer. Memory ≈ the largest volume of one round.
- **Writes are never merged**: the gap between them would overwrite someone else's memory.
  Each `Write` is a separate `WriteOp`, and its bytes are encoded when the request is described.

## A.4 Future: internals

```go
type Future[T any] struct {
    state atomic.Uint32   // pending / ok / failed
    val   T
    err   error
    gen   uint64
    mu    sync.Mutex      // only for the lazy done
    done  chan struct{}
    pub   *sync.RWMutex   // the Plan publish lock (nil for Batch/Scheduler)
}
```

- `resolve(v, err)`: writes `val/err/gen`, then `state.Store(ready)`, then under `mu`
  closes `done` if it was already created.
- `Done()`: checks the state under `mu`. If ready, it returns a global closed channel (no
  allocation). Otherwise it creates `done`. Checking and creating under the same `mu` removes the race
  "created the channel after resolve".
- `Get()`: reads `state` atomically. For a Plan it takes `pub.RLock`, so the value is not overwritten
  in the middle of copying `T`.
- **Plan futures** are reused between `Exec`s. In addition there is
  `Next(ctx, afterGen uint64) (T, uint64, error)`, which waits for a value newer than `afterGen`.
  This is for subscribers of `Scheduler.Attach`. Mechanics: one channel per Plan per execution,
  created lazily, so it allocates only when someone waits.

## A.5 Plan: freezing and zero-alloc

On the first `Exec` (freeze):
1. All `Loc`s compile into nodes. The codec of each leaf is resolved once and kept in the `sink`,
   so there is no `sync.Map` lookup on every Exec.
2. ALAP (`notBefore`) is computed for the static parts (A.6).
3. A **template** `[]node` in the initial state is saved.
4. The round arena is preallocated for the static maximum.

On every `Exec`:
- `copy(nodes, template)` resets all states with one memmove.
- Dynamic nodes (`Then`, `LoadEach` elements) come from the slab tail, which does `nodes = nodes[:len(template)]`
  and grows again each time within the same capacity.
- `LoadEach` keeps `slots []*loaderState`. Slot `i` is created when the count first reaches
  `i+1`, and is then reused together with its staging copy of `T`.
- **Strings**: the decoder compares the new bytes with the previous value in this slot. If the bytes
  match, it reuses the same `string` and does not allocate. So HP changes every frame, but the name does not.
- Remaining allocations: `Then` with new futures; growth of the `LoadEach` element count; changed strings;
  **every slice result** (`ReadSlice`, `LoadEach`, `All`) is a new slice on each `Exec`.
  Double buffering of slices is rejected: a slice returned by `Get` would change "under your hands"
  two `Exec`s later. Safety matters more than one allocation.

**How it is implemented (stage 5)**:
- `Plan` has its own `engine.Run` (not from the pool, because `sync.Pool` may hand out a new object after a GC).
- Futures write their result into staging (`sv`/`serr`), and `Plan.finish` under the `publishLock` write lock
  commits all futures together and increments `Gen`. `View` takes the read lock, and so does `Get` inside `View`,
  which is why the lock is reader-preferring (A.0).
- `Future.Gen()` and `Future.Next(ctx, after)` subscribe to new Plan values without polling:
  the update channel is taken **before** the generation check, so a publish cannot be missed.
- Strings: `stringMemo` compares the raw bytes with the previous `Exec`; unchanged gives the same `string`.
- `Run(ctx, plan, batch)` publishes the Plan like `Exec` does; the same scope twice panics.

`Plan.Exec` is not reentrant: a parallel call waits on an internal mutex.
A Plan attached to a `Scheduler` cannot be run by hand (panic).

## A.6 Loader: ALAP and revalidation by example

```go
func (p *Player) Bind(l *smartmem.Loader, base smartmem.Loc) {
    smartmem.Field(l, &p.HP,   base.Add(0x100))                         // d=0
    smartmem.Field(l, &p.Team, base.Add(0x13C))                         // d=0
    smartmem.Field(l, &p.Pos,  base.Add(0x30).Deref().Add(0x90))        // d=1
    smartmem.FieldString(l, &p.Name, base.Add(0x50).Deref(), smartmem.CString(32)) // d=1
}
```

`D = 1`. `UnicodeString` would add +1, because its buffer is one more step with a statically known depth.

| Round | Without ALAP | With ALAP |
|---|---|---|
| 1 | HP+Team (1 span), `[base+0x30]`, `[base+0x50]` | `[base+0x30]`, `[base+0x50]` (1 span) |
| 2 | Pos, Name | **HP+Team, Pos, Name** — all leaves from one scatter |

The number of rounds is the same (2), but with ALAP HP and Pos are read at the same moment.

Computation: `notBefore = r0 + (D − d)`, where `r0` is the round in which the `base` address became known.
For a static base `r0 = 1`. For `LoadEach` over a list from `ReadSlice` `r0 = 2`, and ALAP shifts.

**Revalidation** (`Revalidate(1)`): in the leaf round (`r0 + D`) the engine adds check
nodes `kLeaf` with `sink = check{expected}` for each slot `[base+0x30]` and `[base+0x50]`. They cost
8 bytes and usually land in the same span as HP. If a value differs, the state
of this Loader's nodes is reset, `r0 = round+1`, `retry++`. When the attempts run out, `ErrUnstable`.

**Publish**: `loader.pending` = the number of leaves. When it reaches 0 and there are no errors,
`*published = *staging` is done, and the future resolves. The first non-`Optional` error moves
the rest of the Loader's nodes to `sSkipped` (saving I/O), and the future gets a `FieldError`.

## A.7 Scheduler

```
loop:
    wait(the first request in the queue OR time for an attached Plan OR Close)
    if there are ad-hoc requests { a FlushWindow timer; wait for the timer or len(queue) ≥ FlushMax }
    mu.Lock(); g := queue; queue = spare; mu.Unlock()   // double buffering of graphs
    due := plans with nextAt ≤ now
    smartmem.Run(ctx, g, due...)
    g.reset(); spare = g                                 // the graph is reused
```

- `Read(sch, …)` takes `sch.mu`, adds nodes to `queue` and wakes the loop through a `chan struct{}` with a buffer of 1.
- Continuations of ad-hoc requests run within the same `Run`. A new
  request from another goroutine waits for the next flush.
- `Close()` stops the loop, and pending futures get `ErrClosed`.

## A.8 Codec

```go
type layout struct {
    size, align uintptr
    kind        kindTag            // prim, array, blittable, sparse, ptr, string
    fields      []sparseField      // sparse only: {offset in memory, offset in the Go struct, codec}
}
type codec struct {
    layout
    decode func(dst unsafe.Pointer, src []byte)   // for blittable: memmove
    encode func(dst []byte, src unsafe.Pointer)
}
func codecFor[T any]() *codec // sync.Map[reflect.Type]*codec; called when describing, not when executing
```

- The blittable check walks `reflect.Type` recursively: allowed are `Int*`, `Uint*`, `Float*`, `Array`
  with a blittable element, `Struct` with blittable fields. Forbidden are `Bool`, `String`, `Slice`, `Map`, `Ptr`,
  `Interface`, `Chan`, `Func`, `UnsafePointer`, `Uintptr` (addresses use `Addr`/`Ptr[T]`), and also
  `int`/`uint`, because their size depends on the platform.
- `memmove` decoding is correct only on a **64-bit** little-endian host. On 32-bit hosts
  (386, arm) Go aligns `int64` to 4 bytes, and the layout differs from x64 C. So `internal/codec` has
  `//go:build amd64 || arm64 || riscv64 || loong64 || ppc64le || mips64le`, and on other platforms
  the build fails with `undefined: smartmemNeedsA64BitLittleEndianHost`.
- Sparse structs are not decoded whole: each field is a separate leaf in the graph with
  a `sink` that writes at `unsafe.Add(base, field.goOffset)`.

## A.9 Concurrency: who holds what

| Object | Goroutine-safe | Protection |
|---|---|---|
| `Session` | yes | handle pool under a mutex; codec cache is a `sync.Map` |
| `Process` | yes | module cache under a `sync.RWMutex` |
| `Batch` (describing) | no | — |
| `Plan.Exec` | serialized | an internal mutex |
| `Plan` futures / `View` | yes | `publishLock`: our own reader-preferring RW lock (see A.0) |
| `Scheduler` | yes | `mu` on the queue |
| `Future.Get/Wait/Done` | yes | atomic state + `mu` for done |

Lock order (to avoid deadlocks): `Scheduler.mu` → `Plan.exec` → `Plan.pub` → `Future.mu`.
Continuations run **without** holding `Plan.pub`, so `Map`/`Then` can
read other futures.
