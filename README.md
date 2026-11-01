# smartmem

[![CI](https://github.com/sergeyzav/smartmem/actions/workflows/ci.yml/badge.svg)](https://github.com/sergeyzav/smartmem/actions/workflows/ci.yml)
[![Integration](https://github.com/sergeyzav/smartmem/actions/workflows/integration.yml/badge.svg)](https://github.com/sergeyzav/smartmem/actions/workflows/integration.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/sergeyzav/smartmem.svg)](https://pkg.go.dev/github.com/sergeyzav/smartmem)

Fast typed memory access for [MemProcFS](https://github.com/ufrisk/MemProcFS): scatter-batched reads and writes of primitives, structs, strings and pointer chains.

Built on top of [gomemprocfs](https://github.com/sergeyzav/gomemprocfs).

## Status

Experimental. The API may change until v1.0. The API reference with runnable examples is on [pkg.go.dev](https://pkg.go.dev/github.com/sergeyzav/smartmem); the design is in [docs/DESIGN.md](docs/DESIGN.md).

## Features

- **Typed reads and writes**: Go numbers, arrays and C-layout structs, `Ptr[T]`, C strings, UTF-16 and `UNICODE_STRING`. No `[]byte` juggling.
- **Pointer chains** in bracket notation: `p.Chain(base, 0x30, 0x90)` is `[[base]+0x30]+0x90`.
- **Rounds**: requests are described lazily and run in rounds. Each round is one scatter operation per process. Independent requests share a round; each pointer step moves a request to the next round.
- **Business structs**: `Load` assembles a Go struct from fields spread over memory and publishes it only when every field is in.
- **Plans** for polling: describe once, `Exec` every tick, with no allocations.
- **Scheduler**: subsystems on different goroutines share rounds without knowing about each other.
- **Fake backend** for tests: no dump, no native libraries, any OS.
- **Any memory source**: MemProcFS is one backend; a dump file, `/proc/<pid>/mem` or your own API is another. See [Custom memory access](docs/custom-backend.md).

## Requirements

- Go 1.25+
- 64-bit little-endian host (amd64, arm64, …). x64 target processes only (WoW64 is not supported yet).
- The MemProcFS native libraries, only for `backend/memprocfs`. See the [gomemprocfs requirements](https://github.com/sergeyzav/gomemprocfs#requirements). The core package and its tests run without them, and so does your own backend: see [Custom memory access](docs/custom-backend.md).

## Install

```sh
go get github.com/sergeyzav/smartmem
```

## Quick start

```go
import (
    gm "github.com/sergeyzav/gomemprocfs"
    "github.com/sergeyzav/smartmem"
    "github.com/sergeyzav/smartmem/backend/memprocfs"
)

vmm, err := gm.NewVmm("./libs/vmm.dylib", gm.WithDevice("fpga"))
if err != nil { ... }
defer vmm.Close()
be := memprocfs.New(vmm)
defer be.Close()

s := smartmem.New(be)
p, err := s.ProcessByName("app.exe")

type SomeStruct struct{ X, Y, Z float32 }

b := s.NewBatch()
count  := smartmem.Read[int32](b, p.Module("core.dll").Add(0x1A2B30))
origin := smartmem.Read[SomeStruct](b, p.Chain(p.Module("core.dll").Add(0x100), 0x30, 0x90))
if err := b.Run(ctx); err != nil { ... } // only fatal errors: ctx, backend

v, err := count.Get() // errors of single requests live in their Futures
```

`count` and `origin` share round 1, which reads `count` and the first pointer of `origin`. `origin` then takes two more rounds, one per pointer.

## Concepts

### Locations

A `Loc` describes how to reach an address. It does no I/O.

| Code | Means |
|---|---|
| `p.At(0x1000)` | the address 0x1000 |
| `p.Module("core.dll").Add(0x10)` | module base + 0x10 (resolved once, cached) |
| `loc.Deref()` | the pointer stored at `loc` |
| `p.Chain(B, o1, o2)` | `[[B]+o1]+o2` = `B.Deref().Add(o1).Deref().Add(o2)` |

`Chain(B)` with no offsets is `B` itself; `n` offsets mean `n` dereferences. `String()` prints a Loc in the same notation.

### Rounds

```
               round 1        round 2           round 3
subsystem A:   read X ✓
subsystem B:   read [Y] ─► Y' read [Y'+0x10] ─► Z  read Z+0x8 ✓
               └ 1 scatter ┘  └── 1 scatter ──┘    └ 1 scatter ┘
```

Within a round, reads on the same page are merged and deduplicated, and writes go before reads. A null or kernel pointer, an unreadable page or a short read fails only the request it belongs to.

### Futures

`Read` returns a `*Future[T]`. `Get` never blocks; `Wait(ctx)` and `Done()` work across goroutines. Compose with `Map`, `Then` (a dependent request in the next round) and `All`; `Resolved` and `Failed` end a `Then` with a computed value or an error.

```go
count := smartmem.Read[uint32](b, p.At(listAddr))
items := smartmem.Then(count, func(s smartmem.Scope, n uint32) *smartmem.Future[[]uint32] {
    return smartmem.ReadSlice[uint32](s, p.At(itemsAddr), int(n))
})
```

## Business structs

`Bind` says where each field lives. `Load` reads them in as few rounds as the pointers allow, and the Future gets the struct only when every field is in: nobody sees it half filled.

```go
type Record struct {
    ID     int32
    Name   string
    Origin SomeStruct
}

func (r *Record) Bind(l *smartmem.Loader, base smartmem.Loc) {
    smartmem.Field(l, &r.ID, base.Add(0x10))
    smartmem.FieldString(l, &r.Name, base.Add(0x18).Deref(), smartmem.CString(16))
    smartmem.Field(l, &r.Origin, base.Add(0x20).Deref().Add(0x40))
}

rec  := smartmem.Load[Record](b, p.At(addr))
recs := smartmem.LoadEach[Record](b, p, smartmem.ReadSlice[smartmem.Ptr[Record]](b, p.At(list), 64))
```

Structs that mirror C exactly can be read in one range with `Read[T]`. Use explicit `_ [N]byte` padding, `CBool` instead of `bool`, and sized integers instead of `int`. A struct with `mem:"0x48"` tags is read field by field.

### Consistency

A live process cannot be snapshotted atomically. smartmem gives three concrete guarantees instead:

1. **Atomic publish**: a struct is seen whole or as an error.
2. **ALAP** (on by default): shorter chains are delayed so that all fields of a struct are read by the same, last scatter.
3. **`Revalidate(n)`**: the chain pointers are re-read in that last round. If one changed, the object moved, and the struct is read again; after `n` retries it fails with `ErrUnstable`. This costs no extra round when nothing changed.

## Polling with a Plan

```go
plan := s.NewPlan()
count := smartmem.Read[int32](plan, p.At(0x1000))
limit := smartmem.Read[int32](plan, p.At(0x1004))

for range ticker.C {
    if err := plan.Exec(ctx); err != nil { ... }
    plan.View(func() { // values of the same Exec
        n, _ := count.Get()
        l, _ := limit.Get()
    })
}
```

A Plan of fixed-size values, structs and unchanged strings runs without allocating. `ReadSlice`, `LoadEach` and `All` allocate their result slice on each `Exec`. `Future.Next(ctx, gen)` waits for the next value without polling.

## Concurrent subsystems

```go
sch := smartmem.NewScheduler(s) // flush window 500µs by default
defer sch.Close()
sch.Attach(plan, 16*time.Millisecond)

// any goroutine:
v, err := smartmem.Read[uint32](sch, p.Chain(p.At(y), 0x10)).Wait(ctx)
```

Requests from different goroutines that arrive within the flush window run together, sharing rounds with each other and with attached Plans that are due.

## Errors

| Error | When |
|---|---|
| `ErrPending` | `Get` before the value is ready |
| `*NullPointerError{Step}` | a dereference read 0 |
| `*BadPointerError{Step, Value}` | a pointer is not a user-mode x64 address |
| `*PartialReadError{Addr, Want, Got}` | the backend read fewer bytes |
| `*ReadError` / `*WriteError` | the backend failed this range (wraps e.g. `gomemprocfs.ErrReadFailed`) |
| `*FieldError{Loc, Err}` | a field of a loaded struct failed |
| `*ModuleError{Name, Err}` | a module base could not be resolved |
| `ErrMaxRounds` | a request needed more than `WithMaxRounds` rounds |
| `ErrUnstable` | `Revalidate` retries ran out |
| `ErrNoTerminator`, `ErrNullBuffer` | string decoding |
| `ErrBatchDone`, `ErrClosed`, `ErrUnsupported` | API misuse, closed Scheduler, unsupported target |

All work with `errors.Is` and `errors.As`.

## Testing your code

`backend/fake` is an in-memory backend. Use it to test your `Bind` methods without a dump:

```go
mem := fake.New()
mem.Proc(4242).
    I32(0x10010, 250).
    U64(0x10018, 0x20000).Bytes(0x20000, []byte("alpha\x00"))

s := smartmem.New(mem)
p, _ := s.Process(4242)
m, err := smartmem.ReadNow[int32](ctx, p.At(0x10010))

mem.Trace() // what was read, round by round: "r1 pid=4242 R 0x10010+4"
```

## Custom memory access

smartmem reaches memory through one interface, `backend.Backend`, with a single method `Exec`. Implement it to read a memory dump, a live Linux process through `/proc/<pid>/mem`, `ReadProcessMemory`, a debugger or a remote agent, and keep everything else: pointer chains, rounds, Plans, the Scheduler and `Load`. The optional `ProcessInfo` interface adds `ProcessByName` and `Process.Module`.

The guide [docs/custom-backend.md](docs/custom-backend.md) builds a complete backend step by step and lists the contract: what smartmem guarantees to your backend and how your errors reach the requests.

## Development

```sh
go test ./...                 # unit tests, no memory dump needed
go test -race ./...           # with the race detector
golangci-lint run             # linters and gofumpt (golangci-lint v2)

# integration tests against a real dump
VMM_DLL_PATH=/path/to/vmm.so MEMDUMP_PATH=/path/to/memdump.raw go test ./backend/memprocfs
```

CI runs the linters and the tests on Linux (with `-race`), macOS and Windows. The Integration workflow runs `backend/memprocfs` against a dump with the two latest MemProcFS releases, once its GCP secrets are configured.

## License

[MIT](LICENSE)
