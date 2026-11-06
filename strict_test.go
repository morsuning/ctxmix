package ctxmix

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"runtime"
	"testing"
	"time"
)

// ---- hand-built frames ------------------------------------------------------

func uv(v uint64) []byte { return binary.AppendUvarint(nil, v) }

type fblock struct {
	kind    byte
	rawLen  []byte // raw uvarint bytes, so tests can make them non-canonical
	compLen []byte
	crc     uint32
	payload []byte
}

func storedBlock(data []byte) fblock {
	return fblock{kindStored, uv(uint64(len(data))), uv(uint64(len(data))), crc32.ChecksumIEEE(data), data}
}

// frame assembles a stream from raw parts.
func frame(level byte, blockSize []byte, blocks []fblock, end []byte) []byte {
	out := append([]byte(frameMagic), level)
	out = append(out, blockSize...)
	for _, b := range blocks {
		out = append(out, b.kind)
		out = append(out, b.rawLen...)
		out = append(out, b.compLen...)
		out = binary.LittleEndian.AppendUint32(out, b.crc)
		out = append(out, b.payload...)
	}
	return append(out, end...)
}

func endMarker(total uint64) []byte { return append([]byte{kindEnd}, uv(total)...) }

func TestHandBuiltFrameIsValid(t *testing.T) {
	// If this fails, the other tests in this file prove nothing.
	hello := []byte("hello")
	in := frame(0, uv(1024), []fblock{storedBlock(hello)}, endMarker(5))
	if got, err := DecodeAll(in); err != nil || !bytes.Equal(got, hello) {
		t.Fatalf("DecodeAll: %q, %v", got, err)
	}
	if info, err := Inspect(bytes.NewReader(in)); err != nil || info.RawSize != 5 || info.Blocks != 1 || info.StoredBlocks != 1 || info.CompressedSize != int64(len(in)) {
		t.Fatalf("Inspect: %+v, %v", info, err)
	}
}

// Every stream has exactly one valid encoding of its framing: lengths must be
// canonical varints and consistent with the block kind.
func TestFramingIsStrict(t *testing.T) {
	hello := []byte("hello")
	good := storedBlock(hello)
	nonCanonical := func(v byte) []byte { return []byte{v | 0x80, 0x00} } // v encoded in two bytes

	badRaw := good
	badRaw.rawLen = nonCanonical(5)
	badComp := good
	badComp.compLen = nonCanonical(5)
	compEqualsRaw := good // a compressed block must be strictly smaller than its input
	compEqualsRaw.kind = kindComp
	storedMismatch := good
	storedMismatch.compLen = uv(4)
	zeroLen := fblock{kindStored, uv(0), uv(0), 0, nil}
	rawTooBig := good
	rawTooBig.rawLen, rawTooBig.compLen = uv(2000), uv(2000)

	cases := []struct {
		name string
		in   []byte
	}{
		{"noncanonical-blocksize", frame(0, []byte{0x80 | 0x00, 0x88, 0x00}, []fblock{good}, endMarker(5))},
		{"blocksize-below-minimum", frame(0, uv(1023), []fblock{good}, endMarker(5))},
		{"noncanonical-rawlen", frame(0, uv(1024), []fblock{badRaw}, endMarker(5))},
		{"noncanonical-complen", frame(0, uv(1024), []fblock{badComp}, endMarker(5))},
		{"compressed-not-smaller", frame(0, uv(1024), []fblock{compEqualsRaw}, endMarker(5))},
		{"stored-length-mismatch", frame(0, uv(1024), []fblock{storedMismatch}, endMarker(5))},
		{"zero-length-block", frame(0, uv(1024), []fblock{zeroLen}, endMarker(0))},
		{"block-larger-than-blocksize", frame(0, uv(1024), []fblock{rawTooBig}, endMarker(2000))},
		{"noncanonical-total", frame(0, uv(1024), []fblock{good}, []byte{kindEnd, 0x85, 0x00})},
		{"wrong-total", frame(0, uv(1024), []fblock{good}, endMarker(6))},
		{"bad-crc", frame(0, uv(1024), []fblock{{kindStored, uv(5), uv(5), 0xdeadbeef, hello}}, endMarker(5))},
		{"unknown-kind", frame(0, uv(1024), []fblock{{3, uv(5), uv(5), 0, hello}}, endMarker(5))},
		{"missing-end-marker", frame(0, uv(1024), []fblock{good}, nil)},
	}
	for _, c := range cases {
		if _, err := DecodeAll(c.in); err == nil || !acceptableDecodeError(err) {
			t.Errorf("%s: DecodeAll error %v", c.name, err)
		}
		if _, err := Inspect(bytes.NewReader(c.in)); err == nil && c.name != "bad-crc" {
			t.Errorf("%s: Inspect accepted the stream", c.name)
		}
	}
}

// A compressed payload must have exactly the length the encoder produces:
// appending, removing or padding bytes is an error even where the model could
// still reproduce the data.
func TestBlockPayloadLengthIsExact(t *testing.T) {
	for _, lv := range []Level{Best, Fast} {
		data := textData(3000, 31)
		comp, err := EncodeBlock(nil, data, lv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeBlock(nil, comp, len(data), lv); err != nil {
			t.Fatalf("%s: %v", levelName(lv), err)
		}
		mutations := map[string][]byte{
			"trailing-zero":  append(append([]byte(nil), comp...), 0),
			"trailing-byte":  append(append([]byte(nil), comp...), 0x5a),
			"dropped-last":   comp[:len(comp)-1],
			"dropped-first":  comp[1:],
			"dropped-4-last": comp[:len(comp)-4],
			"empty":          nil,
		}
		for name, m := range mutations {
			out, err := DecodeBlock(nil, m, len(data), lv)
			if err == nil && bytes.Equal(out, data) {
				t.Errorf("%s/%s: payload of the wrong length accepted", levelName(lv), name)
			}
			if err != nil && !errors.Is(err, ErrCorrupt) {
				t.Errorf("%s/%s: error %v is not ErrCorrupt", levelName(lv), name, err)
			}
		}
	}
	// Zero-length blocks have no payload at all.
	if _, err := DecodeBlock(nil, []byte{1}, 0, Best); !errors.Is(err, ErrCorrupt) {
		t.Errorf("payload for an empty block: %v", err)
	}
}

// ---- limits ---------------------------------------------------------------------

func TestMaxOutputSize(t *testing.T) {
	data := textData(5000, 41)
	comp := mustEncode(t, data, WithBlockSize(MinBlockSize)) // 5 blocks
	if got, err := DecodeAll(comp, WithMaxOutputSize(int64(len(data)))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("limit equal to the size: %v", err)
	}
	if _, err := DecodeAll(comp, WithMaxOutputSize(int64(len(data))-1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit below the size: %v", err)
	}
	// The data before the offending block is still delivered, and the error
	// comes before any work is spent on that block.
	r, err := NewReader(bytes.NewReader(comp), WithMaxOutputSize(2*MinBlockSize))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if !errors.Is(err, ErrTooLarge) || !bytes.Equal(got, data[:2*MinBlockSize]) {
		t.Fatalf("got %d bytes and error %v", len(got), err)
	}
	if _, err := NewReader(bytes.NewReader(comp), WithMaxOutputSize(0)); err == nil {
		t.Error("WithMaxOutputSize(0) accepted")
	}
	if _, err := NewReader(bytes.NewReader(comp), WithMaxOutputSize(-5)); err == nil {
		t.Error("negative WithMaxOutputSize accepted")
	}
}

// A header that announces a big block followed by a truncated stream must not
// make the reader allocate the announced size.
func TestHostileHeaderDoesNotAllocateUpFront(t *testing.T) {
	const big = 32 << 20
	in := append([]byte(frameMagic), 0)
	in = append(in, uv(uint64(MaxBlockSize))...)
	in = append(in, kindComp)
	in = append(in, uv(big)...)
	in = append(in, uv(big-1)...)
	in = binary.LittleEndian.AppendUint32(in, 0)
	in = append(in, make([]byte, 100)...) // far less payload than announced

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := DecodeAll(in)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want unexpected EOF", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("allocated %d bytes for a 100-byte stream", grew)
	}
}

func TestReadPayload(t *testing.T) {
	for _, n := range []int{0, 1, 1<<20 - 1, 1 << 20, 1<<20 + 1, 3<<20 + 5} {
		src := randData(n, int64(n))
		got, err := readPayload(bytes.NewReader(src), n)
		if err != nil || !bytes.Equal(got, src) {
			t.Errorf("n=%d: %v", n, err)
		}
		if n > 0 {
			if _, err := readPayload(bytes.NewReader(src[:n-1]), n); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("n=%d truncated: %v", n, err)
			}
		}
	}
}

func TestReaderClose(t *testing.T) {
	data := textData(3000, 5)
	comp := mustEncode(t, data, WithBlockSize(MinBlockSize))
	r, err := NewReader(bytes.NewReader(comp), WithConcurrency(2))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Data already decoded for the current block may still be returned, but
	// the stream must end with ErrClosedPipe, never with a clean EOF.
	_, err = io.ReadAll(r)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Read after Close: %v", err)
	}
}

// Close from another goroutine aborts a Read in progress.
func TestCloseAbortsRead(t *testing.T) {
	data := textData(1<<20, 6)
	comp := mustEncode(t, data, WithLevel(Fast), WithBlockSize(1<<20))
	r, err := NewReader(bytes.NewReader(comp))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, r); done <- err }()
	time.AfterFunc(20*time.Millisecond, func() { r.Close() })
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("unexpected error %v", err)
		}
	case <-time.After(cancelBound() + 30*time.Second):
		t.Fatal("Close did not abort the Read")
	}
}

// ---- MemoryUsage ------------------------------------------------------------------

func TestMemoryUsage(t *testing.T) {
	for _, lv := range []Level{Best, Fast} {
		for _, n := range []int{MinBlockSize, 100000, 1 << 20, 4 << 20, DefaultBlockSize, MaxBlockSize} {
			if n > 4<<20 {
				// Do not allocate 100+ MiB just to measure it: the table sizes
				// stop growing at 4 MiB, so the estimate must equal the 4 MiB one.
				if got, want := MemoryUsage(n, lv), MemoryUsage(4<<20, lv); got != want {
					t.Errorf("%s n=%d: estimate %d, want %d", levelName(lv), n, got, want)
				}
				continue
			}
			buf := make([]byte, n)
			var a, b runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&a)
			m := newCM(buf, n, false, configFor(n, lv))
			runtime.ReadMemStats(&b)
			runtime.KeepAlive(m)
			measured := int64(b.TotalAlloc - a.TotalAlloc)
			est := MemoryUsage(n, lv)
			if d := measured - est; d < -4<<10 || d > 4<<10 {
				t.Errorf("%s n=%d: estimate %d, measured %d", levelName(lv), n, est, measured)
			}
		}
	}
	if MemoryUsage(0, Best) != MemoryUsage(MinBlockSize, Best) || MemoryUsage(int(^uint(0)>>1), Best) != MemoryUsage(MaxBlockSize, Best) {
		t.Error("block size is not clamped")
	}
	if MemoryUsage(MinBlockSize, Level(9)) != MemoryUsage(MinBlockSize, Best) {
		t.Error("invalid level is not treated as Best")
	}
	if mib := MemoryUsage(DefaultBlockSize, Best) >> 20; mib < 90 || mib > 110 {
		t.Errorf("default block needs %d MiB, documentation says about 100", mib)
	}
}

// ---- Inspect ----------------------------------------------------------------------

func TestInspect(t *testing.T) {
	data := append(textData(2500, 51), randData(1500, 52)...) // compressed and stored blocks
	for _, lv := range []Level{Best, Fast} {
		comp := mustEncode(t, data, WithLevel(lv), WithBlockSize(MinBlockSize))
		trailer := []byte("junk after the stream")
		cr := &countingReader{r: bytes.NewReader(append(append([]byte(nil), comp...), trailer...))}
		info, err := Inspect(cr)
		if err != nil {
			t.Fatal(err)
		}
		if cr.n != len(comp) {
			t.Errorf("consumed %d bytes of a %d byte stream", cr.n, len(comp))
		}
		// 4000 bytes in 1 KiB blocks: four blocks, the last one random and therefore stored.
		want := Info{Level: lv, BlockSize: MinBlockSize, Blocks: 4, RawSize: int64(len(data)), CompressedSize: int64(len(comp))}
		got := info
		got.StoredBlocks = 0
		if got != want {
			t.Errorf("%s: %+v, want %+v", levelName(lv), info, want)
		}
		if info.StoredBlocks < 1 || info.StoredBlocks > info.Blocks {
			t.Errorf("%s: StoredBlocks = %d", levelName(lv), info.StoredBlocks)
		}
	}
	// Truncation anywhere is reported.
	comp := mustEncode(t, data, WithBlockSize(MinBlockSize))
	for cut := 0; cut < len(comp); cut += 37 {
		if _, err := Inspect(bytes.NewReader(comp[:cut])); err == nil || !acceptableDecodeError(err) {
			t.Fatalf("cut at %d: %v", cut, err)
		}
	}
	// The empty stream.
	info, err := Inspect(bytes.NewReader(mustEncode(t, nil)))
	if err != nil || info.Blocks != 0 || info.RawSize != 0 || info.CompressedSize != int64(len(mustEncode(t, nil))) {
		t.Errorf("empty stream: %+v, %v", info, err)
	}
	if _, err := Inspect(bytes.NewReader([]byte("not a stream"))); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage: %v", err)
	}
}

// ---- reference parser -----------------------------------------------------------

// refParse is a deliberately independent, minimal implementation of the layout
// described in FORMAT.md. It checks the specification against the library: if
// the two ever disagree, one of them (or the document) is wrong.
func refParse(t *testing.T, b []byte) (level byte, blockSize int, rawTotal int, nblocks int) {
	t.Helper()
	pos := 0
	get := func() uint64 {
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			t.Fatalf("bad varint at %d", pos)
		}
		pos += n
		return v
	}
	if string(b[:4]) != "CMX\x01" {
		t.Fatalf("magic % x", b[:4])
	}
	level = b[4]
	pos = 5
	blockSize = int(get())
	for {
		kind := b[pos]
		pos++
		if kind == 2 {
			if total := int(get()); total != rawTotal {
				t.Fatalf("end marker says %d, blocks hold %d", total, rawTotal)
			}
			if pos != len(b) {
				t.Fatalf("%d bytes after the end marker", len(b)-pos)
			}
			return
		}
		raw, comp := int(get()), int(get())
		crc := binary.LittleEndian.Uint32(b[pos:])
		pos += 4
		payload := b[pos : pos+comp]
		pos += comp
		if kind == 0 {
			if raw != comp || crc32.ChecksumIEEE(payload) != crc {
				t.Fatalf("stored block %d inconsistent", nblocks)
			}
		} else if comp >= raw {
			t.Fatalf("compressed block %d is not smaller", nblocks)
		}
		if raw > blockSize {
			t.Fatalf("block %d larger than the block size", nblocks)
		}
		rawTotal += raw
		nblocks++
	}
}

func TestSpecificationMatchesImplementation(t *testing.T) {
	data := append(append(textData(2200, 61), randData(1300, 62)...), textData(900, 63)...)
	for _, lv := range []Level{Best, Fast} {
		for _, bs := range []int{MinBlockSize, 3000, DefaultBlockSize} {
			comp := mustEncode(t, data, WithLevel(lv), WithBlockSize(bs))
			level, blockSize, raw, n := refParse(t, comp)
			info, err := Inspect(bytes.NewReader(comp))
			if err != nil {
				t.Fatal(err)
			}
			if Level(level) != lv || blockSize != bs || raw != len(data) || n != info.Blocks || info.RawSize != int64(raw) {
				t.Errorf("%s bs=%d: reference (%d, %d, %d, %d) vs library %+v", levelName(lv), bs, level, blockSize, raw, n, info)
			}
			// All blocks but the last are full.
			if want := (len(data) + bs - 1) / bs; n != want {
				t.Errorf("%s bs=%d: %d blocks, want %d", levelName(lv), bs, n, want)
			}
		}
	}
	// The empty stream is exactly: magic, level, block size, end marker.
	want := append(append([]byte("CMX\x01\x00"), uv(DefaultBlockSize)...), 2, 0)
	if got := mustEncode(t, nil); !bytes.Equal(got, want) {
		t.Errorf("empty stream % x, want % x", got, want)
	}
}

// ---- more golden vectors -------------------------------------------------------

const (
	mixedVectorBest = "f2ee6987f7e07065e03945c9f130ec861065ef5a1c14c19d106eb6116fc32193"
	mixedVectorFast = "ee8eb2dd17eb1d24b6a7db6fecdaa9f64fab039b807f94b59f556b32d0276f7c"
)

func TestGoldenMixedVectors(t *testing.T) {
	data := mixedVectorData()
	for lv, want := range map[Level]string{Best: mixedVectorBest, Fast: mixedVectorFast} {
		comp, err := EncodeBlock(nil, data, lv)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(comp)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: compressed block digest %s, want %s: the format changed", levelName(lv), got, want)
		}
		out, err := DecodeBlock(nil, comp, len(data), lv)
		if err != nil || !bytes.Equal(out, data) {
			t.Errorf("%s: decode failed: %v", levelName(lv), err)
		}
	}
}
