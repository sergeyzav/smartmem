// Package smartmem provides typed, batched access to the memory of another
// process, on top of MemProcFS or any other [backend.Backend].
//
// Reading memory through DMA or a memory dump is cheap per byte but expensive
// per request. smartmem describes reads lazily and runs them in rounds: every
// round is one scatter operation per process, so independent reads share a
// round, and each pointer that must be followed moves a read to the next one.
// Results arrive as typed Go values: numbers, C-layout structs, pointers,
// strings, and whole Go structs assembled from fields spread over memory.
//
// # Getting started
//
// A [Session] owns a backend and hands out processes. Describe requests in a
// [Batch], run it, and read the [Future] values:
//
//	vmm, err := gomemprocfs.NewVmm(libPath, gomemprocfs.WithDevice("fpga"))
//	be := memprocfs.New(vmm)
//	s := smartmem.New(be)
//	p, err := s.ProcessByName("app.exe")
//
//	b := s.NewBatch()
//	count := smartmem.Read[int32](b, p.Module("core.dll").Add(0x1A2B30))
//	origin := smartmem.Read[SomeStruct](b, p.Chain(p.Module("core.dll").Add(0x100), 0x30, 0x90))
//	if err := b.Run(ctx); err != nil {
//		// only ctx or a broken backend
//	}
//	n, err := count.Get() // the error of this one request, if any
//
// count and origin share the first round. origin then takes two more rounds,
// one for each pointer on its way.
//
// # Locations
//
// A [Loc] describes how to reach an address without reading anything: a base
// ([Process.At] for an address, [Process.Module] for a module base), then
// [Loc.Add] and [Loc.Deref] steps. [Process.Chain] is the usual bracket
// notation for pointer paths: Chain(B, 0x30, 0x90) is [[B]+0x30]+0x90, that
// is B.Deref().Add(0x30).Deref().Add(0x90). A read at a Loc with n Derefs
// takes n+1 rounds.
//
// # Scopes
//
// Requests are described in a [Scope], which decides when they run:
//
//   - [Batch]: describe once, [Batch.Run] once. For one-off reads.
//   - [Plan]: describe once, [Plan.Exec] many times. For polling: nodes,
//     buffers and decoded values are reused, and results of one Exec are
//     published together, so [Plan.View] sees a consistent snapshot.
//   - [Scheduler]: any goroutine describes requests and waits on its Futures;
//     requests that arrive within a short window share rounds. For
//     independent subsystems that do not know about each other.
//
// [Run] executes several Batches and Plans together, sharing their rounds.
// [ReadNow] and [WriteNow] run a single request in a Batch of its own.
//
// # Futures and dependent reads
//
// Every request returns a [Future]. [Future.Get] never blocks; [Future.Wait]
// and [Future.Done] let other goroutines wait, and [Future.Next] follows the
// values of a Plan. Futures compose:
//
//   - [Map] transforms a value.
//   - [Then] describes new requests from a value, for example to follow a
//     pointer whose offset is read first or to walk a linked list. They run
//     in the next round of the same execution.
//   - [All] collects several Futures of one type.
//   - [Resolved] and [Failed] end a Then with a computed value or an error,
//     without another round.
//
// Continuations run on the engine goroutine between rounds, so they must be
// fast and must not block.
//
// # Memory types
//
// [Read] and [ReadSlice] accept blittable types: sized integers and floats,
// arrays, and structs of them, plus [Ptr], [Addr] and [CBool]. Such a value is
// decoded with one copy, because Go lays out these structs the same way as
// x64 C, padding included; a CSize() uintptr method on the type makes the
// size checked against the C one. Go bool, int, uint, uintptr, strings,
// slices, maps and Go pointers are not allowed: describing a request with
// such a type panics with a [*TypeError] that names the field.
//
// A struct whose fields carry `mem:"0x48"` tags is sparse: each tagged field
// is read at its own offset, and the struct is published when all are in.
// Strings are read with [ReadString] and a [StringKind]: [CString], [UTF16]
// or [UnicodeString].
//
// # Business structs
//
// A Go struct that implements [Binder] says in Bind where each field lives,
// possibly behind pointers. [Load] reads such a struct in as few rounds as its
// pointers allow and publishes it only when every field is in, so nobody sees
// it half filled. [LoadEach] does the same for every pointer of a list.
// By default all fields are read by the last round's single scatter
// ([WithALAP]), and [Revalidate] can re-check the pointers in that round to
// detect an object that moved while it was read.
//
// # Errors
//
// [Batch.Run], [Plan.Exec] and [Run] fail only when ctx is done or the
// backend fails as a whole. Every other error belongs to the request that hit
// it and is returned by its Future, so one null pointer or unreadable page
// does not affect other requests in the same round. Errors are typed:
// [*NullPointerError], [*BadPointerError], [*PartialReadError], [*ReadError],
// [*WriteError], [*ModuleError] and [*FieldError] carry the details, and
// sentinels such as [ErrMaxRounds] and [ErrUnstable] match with errors.Is.
//
// # Concurrency
//
// [Session], [Process], [Scheduler] and [Future] are safe for concurrent use.
// Describing requests in a Batch or Plan is not: a Batch or Plan belongs to
// one goroutine until it runs. Executions are independent, so several
// goroutines can run their own Batches and Plans at the same time.
//
// # Backends and testing
//
// Package [github.com/sergeyzav/smartmem/backend/memprocfs] connects to
// MemProcFS. Package [github.com/sergeyzav/smartmem/backend/fake] is an
// in-memory process for tests: it runs on any OS without native libraries,
// and its trace shows what was read in each round. Other sources of memory,
// such as a dump file or /proc/<pid>/mem, implement [backend.Backend]; the
// guide at https://github.com/sergeyzav/smartmem/blob/main/docs/custom-backend.md
// builds one step by step.
//
// # Limits
//
// Only native x64 target processes are supported: [Session.Process] returns
// [ErrUnsupported] for 32-bit and WoW64 processes. The host running smartmem
// must be 64-bit little-endian (amd64, arm64 and similar); other hosts fail
// to build.
package smartmem
