# Third-party material and lineage

`ctxmix` is original Go code, but it builds on published ideas and one small
public-domain data table:

* **Bit-history state table** (`tables.go`): the nonstationary state machine from
  [libzpaq](https://mattmahoney.net/dc/zpaq.html) by Matt Mahoney, which is
  dedicated to the public domain. The table is embedded as constants.
* **Architecture**: the overall design (hashed context bit histories, adaptive
  probability maps, gated mixer network, APM/SSE stages, arithmetic coder) follows
  the PAQ / lpaq / ZPAQ family of compressors by Matt Mahoney and contributors.
  No code from those projects other than the table above is included.

This package is not affiliated with, and is not compatible with, the ZPAQ format
or any PAQ variant. The ZPAQ-compatible sibling project is
[zpaq-go](https://github.com/morsuning/zpaq-go).
