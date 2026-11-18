// Package ebpf is the in-kernel half of the eBPF I/O latency collector: the
// program in iolat.bpf.c and the loader around it.
package ebpf

// The object is committed so that building bbbench needs no clang; run
// `go generate ./internal/iotrace/ebpf` after changing bpf/iolat.bpf.c (BPF2GO_STRIP=llvm-strip-21 where llvm-strip has a suffix).
//
//go:generate go tool bpf2go -tags linux -target bpfel -cc clang -cflags "-O2 -g -Wall -Werror" iolat bpf/iolat.bpf.c -- -Ibpf/headers
//go:generate sh -c "sha256sum bpf/iolat.bpf.c | cut -d' ' -f1 > bpf/iolat.bpf.c.sha256"
