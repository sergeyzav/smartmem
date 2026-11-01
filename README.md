# smartmem

[![CI](https://github.com/sergeyzav/smartmem/actions/workflows/ci.yml/badge.svg)](https://github.com/sergeyzav/smartmem/actions/workflows/ci.yml)

Fast typed memory access for [MemProcFS](https://github.com/ufrisk/MemProcFS): scatter-batched reads and writes of primitives, structs, strings and pointer chains.

Built on top of [gomemprocfs](https://github.com/sergeyzav/gomemprocfs).

## Status

Experimental. The API is not implemented yet and will change until v1.0.
The v1 design is in [DESIGN.md](DESIGN.md).

## Requirements

- Go 1.25+
- x64 target processes only (v1)
- MemProcFS native libraries are needed only for the MemProcFS backend; see the
  [gomemprocfs requirements](https://github.com/sergeyzav/gomemprocfs#requirements).
  The core package and its tests run without them.

## Development

```sh
go test ./...                 # unit tests, no memory dump needed
go test -race ./...           # with the race detector
golangci-lint run             # linters and gofumpt (golangci-lint v2)
```

CI runs the linters and the tests on Linux (with `-race`), macOS and Windows.

## License

[MIT](LICENSE)
