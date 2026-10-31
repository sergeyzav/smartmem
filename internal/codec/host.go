//go:build amd64 || arm64 || riscv64 || loong64 || ppc64le || mips64le

package codec

// Blittable decoding copies x64 little-endian memory straight into Go values.
// That is only correct on 64-bit little-endian hosts, where Go aligns int64 and
// float64 to 8 bytes like x64 C does. Other hosts fail to build (host_other.go).
