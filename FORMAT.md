# The ctxmix stream format

This document describes format version 1 (the fourth magic byte is `0x01`). The
key words MUST, MUST NOT and MAY are used as in RFC 2119.

**Status.** There is exactly one implementation, this Go package. The container
(sections 1 and 2) and the arithmetic coder (section 3) are specified here
completely. The probability model (section 4) is specified by the reference
source (`model.go`, `tables.go`) and pinned by test vectors (section 7); this
document summarises it but does not replace it. Until v1.0.0 the format may
change in a minor release; any change that alters the bytes produced or accepted
gets a new version byte.

All multi-byte integers are little-endian, except **uvarint**, the unsigned
LEB128 variable-length integer of Go's `encoding/binary` (7 bits per byte, least
significant group first, high bit set on all but the last byte). A uvarint MUST
be canonical: the last byte MUST NOT be `0x00` unless it is the only byte, and a
value MUST fit in 64 bits. Decoders MUST reject anything else.

## 1. Stream

```
stream  = header block* end
header  = magic level blocksize
magic   = 43 4d 58 01          ; "CMX" and the format version
level   = byte                 ; 0 = Best, 1 = Fast; anything else is invalid
blocksize = uvarint            ; 1024 .. 67108864 (64 MiB)
block   = kind rawlen complen crc32 payload
kind    = byte                 ; 0 = stored, 1 = compressed
rawlen  = uvarint              ; 1 .. blocksize
complen = uvarint              ; 1 .. rawlen
crc32   = uint32               ; CRC-32 (IEEE) of the block's original data
payload = complen bytes
end     = 02 total             ; total = uvarint, the sum of all rawlen
```

Rules:

* The input is cut into blocks of `blocksize` bytes; every block except the last
  is full. A writer MUST NOT emit empty blocks, so an empty input is the
  header followed directly by the end marker (`43 4d 58 01 <level> <blocksize> 02 00`).
* A *stored* block (`kind` 0) has `complen == rawlen` and its payload is the data
  itself. A *compressed* block (`kind` 1) has `complen < rawlen`. A writer MUST
  store a block whenever compression would not make it strictly smaller, so the
  worst-case expansion is a few bytes per block.
* The payload of a compressed block is decoded as described in sections 3 and 4,
  with the block's `rawlen` and the stream's `level`. Decoding MUST produce
  exactly `rawlen` bytes whose CRC-32 equals `crc32`.
* The `total` in the end marker MUST equal the sum of the `rawlen` fields.
* A reader MUST stop at the end marker and MUST NOT read beyond it, so streams
  can be concatenated or followed by other data.
* Blocks are independent: the model starts from the same initial state for every
  block, so blocks may be compressed and decompressed in any order or in
  parallel, and the output does not depend on the amount of parallelism.
* A reader MAY enforce limits on `blocksize` and on the total decoded size
  before doing any decoding work; both are known from the headers.

A reader MUST reject, with an error that is not a clean end of stream: a wrong
magic or an unsupported version byte, an invalid level, a block size outside the
range above, a non-canonical or overflowing uvarint, an unknown `kind`, lengths
violating the ranges above, a `total` that does not match, a checksum mismatch,
and a stream that ends before the end marker.

## 2. Why this is strict

A stream has one valid framing, so two parsers cannot disagree about where a
block starts or ends, and an attacker cannot hide data in padding between
blocks. The payload bytes themselves are *almost* unique: the final four bytes
of a compressed block (section 3.3) may take any value inside the coder's last
interval. The payload *length* is exact: a decoder MUST reject a compressed
payload that is shorter or longer than the encoder would have produced for the
decoded data, which is equivalent to requiring that decoding consumes exactly
`complen` bytes.

## 3. The arithmetic coder

A compressed block is a bitwise, carry-less binary arithmetic code of the
block's bytes, most significant bit first. Probabilities have 16 bits: `p` is
the model's probability that the next bit is 1, scaled to `1 .. 65535` (the
model clamps it to `8 .. 65528`).

### 3.1 State

Two 32-bit unsigned integers `x1` and `x2` bound the current interval; they
start at `x1 = 0`, `x2 = 0xffffffff`. All arithmetic is modulo 2^32 on unsigned
values.

### 3.2 Encoding one bit `y` with probability `p`

```
r    = x2 - x1
xmid = x1 + (r >> 16) * p + (((r & 0xffff) * p) >> 16)
if y == 1 { x2 = xmid } else { x1 = xmid + 1 }
while ((x1 ^ x2) & 0xff000000) == 0 {   ; leading bytes equal: shift one out
    output byte (x2 >> 24)
    x1 = x1 << 8
    x2 = (x2 << 8) | 255
}
```

### 3.3 Flush

After the last bit the encoder outputs the four bytes of `x1`, most significant
first. Any value within `[x1, x2]` decodes identically, so these four bytes are
the only part of the payload that is not unique.

### 3.4 Decoding

The decoder keeps `x1`, `x2` as above and a 32-bit `x` loaded from the first four
payload bytes (most significant first). For each bit it computes the same `xmid`
from the model's `p`; the bit is 1 if `x <= xmid` (then `x2 = xmid`) and 0
otherwise (then `x1 = xmid + 1`); then it runs the same shifting loop, shifting
the next payload byte into `x` each time. The decoder therefore reads exactly
four bytes plus one byte per shift the encoder made, which is the payload length.
Reading beyond the payload, or finishing before the last byte is consumed, is an
error.

## 4. The model

The probability `p` for each bit is produced by a context-mixing model that is
updated after each bit with the bit's actual value. In outline (the source is
authoritative):

1. **Contexts.** Each byte, a set of hashes is computed from the preceding bytes:
   orders 1 to 5 and 8, the current word and the word pair (previous word, current
   word), the byte above in the previous line together with the previous byte, the
   indentation level, and the byte predicted by the long-range match model.
   `Best` uses eleven contexts, `Fast` five of them.
   Each context selects a 16-byte slot in a hash table holding a check byte and
   15 bit-history states, one per node of the current nibble's binary tree.
2. **Bit histories.** A state is a byte from the nonstationary state machine of
   libzpaq (`tables.go`). An adaptive StateMap per context turns a state into a
   probability, which is stretched (`ln(p/(1-p))`) and fed to the mixer together
   with a confidence input.
3. **Match models.** Two match models (minimum lengths 6 and 16) predict the
   next bit from the longest earlier occurrence of the current context.
4. **Mixing.** A two-layer network: four first-layer mixers (two for `Fast`),
   each selecting a weight set by a different small context (partial byte,
   match length, previous byte, number of context hits), and a final mixer.
5. **Refinement.** Two adaptive probability maps (APM/SSE) refine the mixer
   output, and the result is averaged with it and clamped.

Everything is integer arithmetic. The lookup tables are embedded constants or
generated by integer-only code at start-up, so the model, and therefore the
payload, is identical on every platform.

### 4.1 Block-dependent parameters

The model's size is a pure function of the block length `n` and the level, so a
decoder derives it from the headers: the context hash table has `2^h` slots with
`h` the smallest integer in `12 .. 22` such that `2^h >= 4n`, and the match-model
tables have `2^m` entries with `m` the smallest integer in `16 .. 22` such that
`2^m >= n`. A block therefore needs at most about 100 MiB of model memory (see
`MemoryUsage`), however large the block size is.

## 5. Compatibility policy

* A reader MUST reject a version byte it does not support.
* Within a version, the bytes a conforming writer produces for given input,
  level and block size MUST NOT change. The test vectors (section 7) enforce
  this.
* The writer's concurrency setting MUST NOT affect the output.

## 6. Limits and resource use

| Quantity | Limit |
|---|---|
| Block size | 1 KiB .. 64 MiB (default 16 MiB) |
| Stream size | unbounded (`total` is a 64-bit uvarint) |
| Model memory per block | about 5 MiB (1 KiB blocks) up to about 100 MiB (4 MiB blocks and larger) |
| Overhead on incompressible data | at most 13 bytes per block (kind, two lengths of up to 4 bytes, CRC-32), plus the stream header (6 to 9 bytes) and the end marker (2 to 5 bytes) |

## 7. Test vectors

`ctxmix_test.go` and `strict_test.go` pin the compressed bytes of fixed inputs
(SHA-256 of the output of `EncodeBlock`), and `testdata/*.cmx` are complete
streams that must decode to known data and be reproduced byte for byte by the
encoder. The tests run on amd64, arm64, 32-bit x86 and big-endian s390x in CI.
Any change that alters these digests is a format change.

`strict_test.go` also contains an independent, minimal parser for section 1 that
is checked against the library on every run, so that this document and the
implementation cannot drift apart unnoticed.
