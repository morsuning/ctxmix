# ctxmix

[![Go Reference](https://pkg.go.dev/badge/github.com/morsuning/ctxmix.svg)](https://pkg.go.dev/github.com/morsuning/ctxmix)
[![CI](https://github.com/morsuning/ctxmix/actions/workflows/ci.yml/badge.svg)](https://github.com/morsuning/ctxmix/actions/workflows/ci.yml)

**A pure-Go, dependency-free, maximum-ratio compressor** based on context mixing, the
technique behind PAQ, lpaq and ZPAQ. On source code it produces output about **27%
smaller than `xz -9e`** and about **3.5% smaller than ZPAQ's strongest built-in model**
(`zpaq -m5`), at several times the CPU time of xz.

[简体中文](README.zh-CN.md) · [Format specification](FORMAT.md) · [Changelog](CHANGELOG.md)

```go
import "github.com/morsuning/ctxmix"

// Streaming, like compress/flate:
w, _ := ctxmix.NewWriter(dst, ctxmix.WithConcurrency(4))
w.Write(data)
w.Close() // required

r, _ := ctxmix.NewReader(src)
io.Copy(out, r)

// Or in memory:
comp, _ := ctxmix.EncodeAll(data)
back, _ := ctxmix.DecodeAll(comp)
```

Command-line tool:

```
go install github.com/morsuning/ctxmix/cmd/ctxmix@latest

ctxmix c -o notes.txt.cmx notes.txt     # compress  (-level fast, -block MiB, -j N)
ctxmix d -o notes.txt notes.txt.cmx     # decompress (-max-output 1G for untrusted input)
ctxmix l notes.txt.cmx                  # show the structure of a stream
ctxmix t notes.txt.cmx                  # decompress and verify every checksum
```

## Is it for you?

Use it when **bytes matter more than seconds**: release artifacts, backups or
incremental sync bundles that are written once and sent over a slow or metered link,
archives of source code and text.

Do not use it for latency-sensitive or high-throughput paths, for data that is already
compressed (JPEG, MP4, zip, ...), or when other tools must read the result: the format is
specific to this package. If you need the standard ZPAQ format, use
[zpaq-go](https://github.com/morsuning/zpaq-go), which writes archives that the reference
`zpaq` extracts, and reads its output.

## Measurements

Single thread on a 2.1 GHz cloud Xeon (2 vCPU), Go 1.24, ctxmix at the default block size
(16 MiB), `xz -9e -T1` 5.4.5, `bzip2 -9` 1.0.8, `zpaq` 7.15 `-m5 -t1` (one-file archive,
journaling overhead included). Every ctxmix result was decompressed and compared byte for
byte. Sizes in bytes.

| Input | Size | gzip -9 | bzip2 -9 | xz -9e | zpaq -m5 | ctxmix `Fast` | ctxmix `Best` |
|---|---:|---:|---:|---:|---:|---:|---:|
| Go `cmd/compile/internal/ssa` (tar) | 1,505,280 | 369,215 | 290,775 | 286,600 | 216,333 | 226,377 | **208,873** |
| Go `net/…` (tar) | 4,075,520 | 930,426 | 750,811 | 693,600 | 523,382 | 547,218 | **503,885** |
| Go `doc/` (HTML, Markdown; tar) | 20,346,880 | 3,632,156 | 2,297,775 | 895,188 | **667,769** | 784,891 | 723,093 |

Relative to `xz -9e`, `Best` is 27.1% and 27.4% smaller on the two source-code inputs, and
3.5% and 3.7% smaller than `zpaq -m5`.

The third input is larger than the 16 MiB default block, so ctxmix compressed it as two
independent blocks and the second one started without any history: that is why it loses to
`zpaq -m5` in the table (8.3% larger). Compressed as a single block (`-block 32`, i.e.
`WithBlockSize(32 << 20)`), `Best` produces **629,351** bytes in 61 s: 29.7% smaller than
`xz -9e` and 5.8% smaller than `zpaq -m5`. Choose the block size to cover your data when
you can afford the memory, and measure on your own data.

Time, same machine, seconds (decompression takes as long as compression, because the
decoder runs the same model):

| Input | xz -9e | zpaq -m5 | ctxmix `Fast` | ctxmix `Best` | `Best` decompress |
|---|---:|---:|---:|---:|---:|
| ssa.tar | 0.7 | 3.7 | 2.4 | 4.7 | 4.8 |
| net.tar | 2.0 | 8.1 | 6.3 | 12.7 | 12.7 |
| doc/ | 6.3 | 35.9 | 28.1 | 54.6 | 56.8 |

That is about 0.3 to 0.4 MB/s per core for `Best` and twice that for `Fast`, whose output
is roughly 8% larger. `WithConcurrency` runs independent blocks in parallel (it helps only
for inputs larger than one block).

To reproduce: `ctxmix c -j 1 -o out file`, `xz -9e -T1 -c file | wc -c`,
`zpaq a out.zpaq file -m5 -t1`.

## Things to know

* **Memory.** Each block being processed needs about 100 MiB of model tables at the default
  block size (`MemoryUsage` returns the exact figure; small blocks need much less), plus
  its data. Budget that per `WithConcurrency` worker.
* **Blocks are independent**, so the model forgets everything between blocks. That makes
  parallelism possible, and costs ratio on inputs larger than one block (see the
  measurements: 15% larger on the third input). Use `WithBlockSize` (up to 64 MiB) if you have the
  memory.
* **Determinism.** The model is integer-only, so the output is bit-identical on every
  architecture and operating system. CI checks amd64, arm64, 32-bit x86 and big-endian
  s390x against frozen golden vectors.
* **Untrusted input.** Corrupt data yields `ErrCorrupt`, never a panic; every block has a
  CRC-32 and the framing is strict. A small stream can still describe a lot of output, so
  set `WithMaxBlockSize` and `WithMaxOutputSize` and use `WithContext` for a deadline. See
  [SECURITY.md](SECURITY.md). CRC-32 is not authentication.
* **Stability.** Version 0.x: the API and the format may still change in a minor release;
  every format change bumps the version byte so old readers fail loudly, and is listed in
  the [changelog](CHANGELOG.md).

## API

| | |
|---|---|
| `NewWriter(w, opts...)`, `NewReader(r, opts...)` | streaming; `Close` the writer to finish the stream |
| `EncodeAll(src, opts...)`, `DecodeAll(comp, opts...)` | whole streams in memory |
| `Inspect(r)` | structure of a stream (level, blocks, sizes) without decompressing |
| `EncodeBlock`, `DecodeBlock` | headerless single blocks, for your own container |
| `MemoryUsage(blockSize, level)` | model memory per worker |
| `WithLevel`, `WithBlockSize`, `WithConcurrency` | compression settings |
| `WithMaxBlockSize`, `WithMaxOutputSize`, `WithContext`, `WithProgress` | limits, cancellation, progress |
| `ErrCorrupt`, `ErrTooLarge` | errors to test with `errors.Is` |

The full documentation is on [pkg.go.dev](https://pkg.go.dev/github.com/morsuning/ctxmix).

## How it works

Each bit of every byte is predicted by a mix of many models and then arithmetic coded: hashed
order-1 to order-8 contexts, word and line/indent-aware contexts (bit histories mapped to
probabilities by adaptive state maps), two match models for long repeats, a two-layer
gated mixing network, and two APM/SSE refinement stages. The bit-history state machine
comes from libzpaq (public domain). [FORMAT.md](FORMAT.md) specifies the stream
container and the arithmetic coder exactly and summarises the model.

## Relationship to zpaq-go

ctxmix started as the `ctxmix` subpackage of [zpaq-go](https://github.com/morsuning/zpaq-go)
and became its own module in v0.1.0 of this repository. Streams written by that subpackage
are read unchanged.

## Development

```
go test ./...                                    # about a minute
go test -fuzz FuzzDecode -fuzztime 1m .          # also FuzzDecodeBlock, FuzzRoundtrip
go test -bench . -run '^$' .
```

See [CONTRIBUTING.md](CONTRIBUTING.md); the one rule is that compressed bytes must not
change by accident. The module has no dependencies.

## License

MIT, see [LICENSE](LICENSE). Third-party material and lineage: [NOTICE.md](NOTICE.md).
