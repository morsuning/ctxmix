# Security policy

## Reporting a vulnerability

Please report security problems privately through GitHub's
[private vulnerability reporting](https://github.com/morsuning/ctxmix/security/advisories/new)
rather than in a public issue. You can expect an acknowledgement within a week.

Relevant problems include a crafted stream that makes the decoder panic, read or
write out of bounds, hang, or allocate much more memory or time than the documented
limits allow, and an input that does not round-trip.

## What the decoder guarantees

* Malformed or malicious input produces an error (`ErrCorrupt`,
  `io.ErrUnexpectedEOF` or a limit error), never a panic or an out-of-bounds access.
  This is covered by bit-flip, truncation and garbage tests and by fuzzing.
* Every block is checked against its CRC-32, the framing is strict, and the block
  payload length must be exact. CRC-32 detects accidental corruption; it is **not**
  a cryptographic integrity check, so authenticate streams you do not trust
  (for example with a MAC or a signature) before relying on their content.
* Decompression bombs are bounded only if you ask: a few hundred bytes can describe
  gigabytes of output. For untrusted input set `WithMaxBlockSize` (bounds memory:
  about 100 MiB per worker regardless, less for small blocks) and `WithMaxOutputSize`
  (bounds output and work), and use `WithContext` for a deadline.
* Decompression speed is the same as compression speed (a few hundred KB/s per
  core), so decoding is CPU-expensive by design. Budget for it.

## Supported versions

Only the latest release receives fixes.
