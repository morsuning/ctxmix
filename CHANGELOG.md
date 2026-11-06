# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project adheres
to [Semantic Versioning](https://semver.org/). Until v1.0.0 the compressed format
may change in a minor release; such changes are always called out here, and the
format version byte is bumped so old readers fail loudly.

## [0.1.0] - 2026-11-06

First release as a standalone module. The code was extracted from the `ctxmix`
subpackage of [zpaq-go](https://github.com/morsuning/zpaq-go) v0.1.0 and hardened.
Streams written by that version are read unchanged (format version 1), with the
exceptions listed under "Stricter decoding".

### Added
- `WithMaxOutputSize` and `ErrTooLarge`: refuse streams that announce more
  decompressed data than a limit, before any work is spent on them.
- `Inspect`: report level, block size, block counts and sizes of a stream without
  decompressing it.
- `MemoryUsage`: the model memory one encoder or decoder needs, verified against
  measured allocation.
- `cmd/ctxmix`, a command-line tool (`c`, `d`, `l`, `t`).
- `FORMAT.md`, the format specification, and an independent reference parser in
  the tests that keeps it honest.
- Golden vectors for a second, more varied input (binary records, long repeats,
  zero runs, indentation, random bytes); fuzzers for `DecodeBlock`; big-endian
  (s390x), 32-bit and arm64 CI.

### Stricter decoding
- Block payloads must have exactly the length the encoder produces (previously up
  to four missing bytes were tolerated and read as zeros).
- Framing must be canonical: no redundant varint bytes, block size at least
  1 KiB, a compressed block strictly smaller than its input.
- `Reader` allocates the payload buffer as data arrives instead of trusting the
  announced length, and returns `io.ErrClosedPipe` after `Close`.
- The encoder converts an unexpected panic in a background goroutine into an
  error.

### Format
- Version 1 (`"CMX\x01"`): independent blocks (16 MiB by default) with CRC-32,
  stored fallback, levels `Best` and `Fast`. About 3.5% smaller than ZPAQ's
  strongest model and 27% smaller than `xz -9e` on Go source code.
