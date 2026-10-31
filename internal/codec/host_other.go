//go:build !(amd64 || arm64 || riscv64 || loong64 || ppc64le || mips64le)

package codec

// smartmem needs a 64-bit little-endian host; see host.go.
var _ = smartmemNeedsA64BitLittleEndianHost
