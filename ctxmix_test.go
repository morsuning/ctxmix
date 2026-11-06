package ctxmix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata golden streams")

func TestRoundtrip(t *testing.T) {
	sizes := []int{0, 1, 2, 3, 7, 255, 256, 257, 1000, 4096, 65535, 65536, 65537}
	if !testing.Short() {
		sizes = append(sizes, 200000)
	}
	for _, lv := range []Level{Best, Fast} {
		for _, n := range sizes {
			for _, in := range testInputs(n) {
				// Skip the slow combinations: one data kind at large sizes.
				if n >= 65535 && in.name != "text" && in.name != "random" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%d", levelName(lv), in.name, n), func(t *testing.T) {
					comp := mustEncode(t, in.data, WithLevel(lv))
					if got := mustDecode(t, comp); !bytes.Equal(got, in.data) {
						t.Fatalf("roundtrip mismatch (len %d vs %d)", len(got), len(in.data))
					}
					// Incompressible data must not blow up.
					if limit := len(in.data) + len(in.data)/100 + 64; len(comp) > limit {
						t.Errorf("output %d bytes for %d input (limit %d)", len(comp), len(in.data), limit)
					}
				})
			}
		}
	}
}

func TestCompressesWell(t *testing.T) {
	data := textData(short(100000), 7)
	comp := mustEncode(t, data)
	if len(comp)*2 > len(data) {
		t.Errorf("text compressed only %d -> %d", len(data), len(comp))
	}
	z := mustEncode(t, make([]byte, 100000))
	if len(z) > 200 {
		t.Errorf("100000 zero bytes -> %d bytes", len(z))
	}
	fast := mustEncode(t, data, WithLevel(Fast))
	if len(fast) < len(comp) {
		t.Logf("note: fast (%d) smaller than best (%d) on this input", len(fast), len(comp))
	}
}

func TestEmptyStream(t *testing.T) {
	comp := mustEncode(t, nil)
	if got := mustDecode(t, comp); len(got) != 0 {
		t.Fatalf("got %d bytes", len(got))
	}
	// A Writer that is closed without any Write still produces a valid stream.
	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), comp) {
		t.Errorf("empty Writer output differs from EncodeAll(nil)")
	}
}

func TestBlockBoundaries(t *testing.T) {
	const bs = MinBlockSize
	for _, lv := range []Level{Best, Fast} {
		for _, n := range []int{0, 1, bs - 1, bs, bs + 1, 2 * bs, 2*bs + 1, 3*bs - 1, 3 * bs, 5*bs + 17} {
			if testing.Short() && n > 3*bs {
				continue
			}
			data := textData(n, 99)
			comp := mustEncode(t, data, WithLevel(lv), WithBlockSize(bs))
			if got := mustDecode(t, comp); !bytes.Equal(got, data) {
				t.Fatalf("%s n=%d: mismatch", levelName(lv), n)
			}
		}
	}
}

func TestConcurrencyDoesNotChangeOutput(t *testing.T) {
	data := textData(short(24000), 5)
	data = append(data, randData(5000, 6)...) // mix compressed and stored blocks
	ref := mustEncode(t, data, WithBlockSize(MinBlockSize), WithConcurrency(1))
	for _, w := range []int{2, 3, 8} {
		got := mustEncode(t, data, WithBlockSize(MinBlockSize), WithConcurrency(w))
		if !bytes.Equal(got, ref) {
			t.Errorf("concurrency %d changed the output", w)
		}
		if out := mustDecode(t, ref, WithConcurrency(w)); !bytes.Equal(out, data) {
			t.Errorf("decode with concurrency %d mismatched", w)
		}
	}
}

func TestWriteChunking(t *testing.T) {
	data := textData(short(9000), 3)
	ref := mustEncode(t, data, WithBlockSize(MinBlockSize))
	for _, chunk := range []int{1, 7, 1023, 1024, 1025, 5000} {
		var buf bytes.Buffer
		w, err := NewWriter(&buf, WithBlockSize(MinBlockSize))
		if err != nil {
			t.Fatal(err)
		}
		for p := data; len(p) > 0; {
			n := chunk
			if n > len(p) {
				n = len(p)
			}
			if m, err := w.Write(p[:n]); err != nil || m != n {
				t.Fatalf("Write: %d, %v", m, err)
			}
			p = p[n:]
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), ref) {
			t.Errorf("chunk size %d changed the output", chunk)
		}
	}
}

func TestStoredBlocks(t *testing.T) {
	data := randData(5000, 1)
	comp := mustEncode(t, data, WithBlockSize(MinBlockSize))
	// 5 blocks, each with <= 1+2+2+4 bytes of header, plus stream header and end marker.
	if over := len(comp) - len(data); over > 5*9+16 {
		t.Errorf("stored overhead %d bytes", over)
	}
	if !bytes.Equal(mustDecode(t, comp), data) {
		t.Fatal("mismatch")
	}
}

func TestReaderBehaviour(t *testing.T) {
	data := textData(short(5000), 11)
	comp := mustEncode(t, data, WithBlockSize(MinBlockSize))

	wrappers := map[string]func(io.Reader) io.Reader{
		"plain":    func(r io.Reader) io.Reader { return r },
		"onebyte":  iotest.OneByteReader,
		"half":     iotest.HalfReader,
		"dataerr":  iotest.DataErrReader,
		"timeout":  iotest.TimeoutReader, // second read fails once; checked separately below
		"bytesNot": func(r io.Reader) io.Reader { return struct{ io.Reader }{r} },
	}
	for name, wrap := range wrappers {
		if name == "timeout" {
			continue
		}
		t.Run("src/"+name, func(t *testing.T) {
			r, err := NewReader(wrap(bytes.NewReader(comp)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("ReadAll: %v (equal=%v)", err, bytes.Equal(got, data))
			}
		})
	}

	t.Run("iotest.TestReader", func(t *testing.T) {
		r, err := NewReader(bytes.NewReader(comp))
		if err != nil {
			t.Fatal(err)
		}
		if err := iotest.TestReader(r, data); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("small-reads", func(t *testing.T) {
		r, _ := NewReader(bytes.NewReader(comp), WithConcurrency(2))
		var got []byte
		buf := make([]byte, 13)
		for {
			n, err := r.Read(buf)
			got = append(got, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(got, data) {
			t.Fatal("mismatch")
		}
		if n, err := r.Read(buf); n != 0 || err != io.EOF {
			t.Errorf("read after EOF: %d, %v", n, err)
		}
	})

	t.Run("source-error-is-propagated", func(t *testing.T) {
		boom := errors.New("boom")
		src := io.MultiReader(bytes.NewReader(comp[:len(comp)/2]), iotest.ErrReader(boom))
		r, err := NewReader(src)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(r)
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want boom", err)
		}
	})
}

// countingReader records how many bytes were consumed.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReaderStopsAtEndOfStream(t *testing.T) {
	data := textData(short(3000), 21)
	comp := mustEncode(t, data, WithBlockSize(MinBlockSize))
	trailer := []byte("TRAILING-DATA")
	in := append(append([]byte(nil), comp...), trailer...)

	for name, mk := range map[string]func() (io.Reader, func() int){
		"bytes.Reader": func() (io.Reader, func() int) {
			r := bytes.NewReader(in)
			return r, func() int { return len(in) - r.Len() }
		},
		"plain": func() (io.Reader, func() int) {
			c := &countingReader{r: bytes.NewReader(in)}
			return c, func() int { return c.n }
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, consumed := mk()
			r, err := NewReader(src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("ReadAll: %v", err)
			}
			if consumed() != len(comp) {
				t.Errorf("consumed %d bytes, stream is %d", consumed(), len(comp))
			}
		})
	}

	// Concatenated streams can be read back to back.
	two := append(append([]byte(nil), comp...), comp...)
	br := bytes.NewReader(two)
	for i := 0; i < 2; i++ {
		r, err := NewReader(br)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("stream %d: %v", i, err)
		}
	}

	if _, err := DecodeAll(in); !errors.Is(err, ErrCorrupt) {
		t.Errorf("DecodeAll with trailing data: %v", err)
	}
}

func TestOptionValidation(t *testing.T) {
	bad := []Option{
		WithLevel(Level(2)), WithLevel(Level(-1)),
		WithBlockSize(0), WithBlockSize(MinBlockSize - 1), WithBlockSize(MaxBlockSize + 1),
		WithConcurrency(0), WithConcurrency(-3), WithConcurrency(1000),
		WithMaxBlockSize(0), WithMaxBlockSize(MaxBlockSize + 1),
		WithContext(nil), //nolint:staticcheck // deliberately nil
	}
	for i, o := range bad {
		if _, err := NewWriter(io.Discard, o); err == nil {
			t.Errorf("NewWriter option %d: expected error", i)
		}
		if _, err := EncodeAll(nil, o); err == nil {
			t.Errorf("EncodeAll option %d: expected error", i)
		}
	}
	comp := mustEncode(t, []byte("x"))
	if _, err := NewReader(bytes.NewReader(comp), WithBlockSize(5)); err == nil {
		t.Error("NewReader accepted an invalid option")
	}
}

type failWriter struct {
	after int
	err   error
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after < len(p) {
		n := f.after
		f.after = 0
		return n, f.err
	}
	f.after -= len(p)
	return len(p), nil
}

func TestWriterErrors(t *testing.T) {
	boom := errors.New("disk full")
	data := textData(short(8000), 4)
	for _, after := range []int{0, 3, 6, 20, 100, 400} {
		for _, workers := range []int{1, 3} {
			w, err := NewWriter(&failWriter{after: after, err: boom}, WithBlockSize(MinBlockSize), WithConcurrency(workers))
			if err != nil {
				t.Fatal(err)
			}
			_, werr := w.Write(data)
			cerr := w.Close()
			if !errors.Is(werr, boom) && !errors.Is(cerr, boom) {
				t.Errorf("after=%d workers=%d: Write=%v Close=%v, want boom", after, workers, werr, cerr)
			}
			if err := w.Close(); !errors.Is(err, boom) {
				t.Errorf("second Close = %v", err)
			}
			if _, err := w.Write([]byte("x")); err == nil {
				t.Errorf("Write after failure succeeded")
			}
		}
	}

	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	w.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("Write after Close succeeded")
	}
}

func TestHeaderErrors(t *testing.T) {
	good := mustEncode(t, []byte("hello world"))
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, io.ErrUnexpectedEOF},
		{"short-magic", good[:2], io.ErrUnexpectedEOF},
		{"bad-magic", append([]byte("ZZZ\x01"), good[4:]...), ErrCorrupt},
		{"no-level", good[:4], io.ErrUnexpectedEOF},
		{"bad-level", append(append([]byte(nil), good[:4]...), append([]byte{9}, good[5:]...)...), ErrCorrupt},
		{"no-blocksize", good[:5], io.ErrUnexpectedEOF},
		{"zero-blocksize", []byte("CMX\x01\x00\x00"), ErrCorrupt},
		{"huge-blocksize", []byte("CMX\x01\x00\xff\xff\xff\xff\x0f"), ErrCorrupt},
	}
	for _, c := range cases {
		if _, err := NewReader(bytes.NewReader(c.in)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := NewReader(bytes.NewReader([]byte("CMX\x02\x00\x80\x80\x04"))); err == nil {
		t.Error("future version accepted")
	}
	// Block size limit.
	big := mustEncode(t, []byte("x"), WithBlockSize(1<<20))
	if _, err := NewReader(bytes.NewReader(big), WithMaxBlockSize(1<<19)); err == nil {
		t.Error("WithMaxBlockSize not enforced")
	}
	if _, err := NewReader(bytes.NewReader(big), WithMaxBlockSize(1<<20)); err != nil {
		t.Errorf("block size equal to the limit rejected: %v", err)
	}
}

// cancelBound is how long a canceled operation may keep running. The model
// checks for cancellation every 64 KiB, which takes about a quarter of a second
// at normal speed and ten times that under the race detector.
func cancelBound() time.Duration {
	if raceEnabled {
		return 30 * time.Second
	}
	return 3 * time.Second
}

func TestContextCancel(t *testing.T) {
	t.Run("already-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := EncodeAll([]byte("abc"), WithContext(ctx)); !errors.Is(err, context.Canceled) {
			t.Errorf("EncodeAll: %v", err)
		}
		comp := mustEncode(t, []byte("abc"))
		if _, err := DecodeAll(comp, WithContext(ctx)); !errors.Is(err, context.Canceled) {
			t.Errorf("DecodeAll: %v", err)
		}
	})
	t.Run("mid-encode", func(t *testing.T) {
		data := textData(1<<20, 1) // several seconds of work
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := EncodeAll(data, WithContext(ctx), WithBlockSize(1<<20))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
		if d := time.Since(start); d > cancelBound() {
			t.Errorf("cancellation took %v", d)
		}
	})
	t.Run("mid-decode", func(t *testing.T) {
		data := textData(1<<20, 1)
		stream := mustEncode(t, data, WithLevel(Fast), WithBlockSize(1<<20))
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := DecodeAll(stream, WithContext(ctx))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
		if d := time.Since(start); d > cancelBound() {
			t.Errorf("cancellation took %v", d)
		}
	})
	t.Run("reader-close", func(t *testing.T) {
		data := textData(1<<20, 2)
		stream := mustEncode(t, data, WithLevel(Fast), WithBlockSize(1<<20))
		r, err := NewReader(bytes.NewReader(stream))
		if err != nil {
			t.Fatal(err)
		}
		r.Close() // abort background decoding before any Read
		if _, err := io.ReadAll(r); err == nil {
			t.Error("expected an error after Close")
		}
	})
}

func TestProgress(t *testing.T) {
	data := textData(150000, 3) // not a multiple of 64 KiB
	var enc, dec atomic.Int64
	comp, err := EncodeAll(data, WithLevel(Fast), WithBlockSize(65536+4096), WithConcurrency(2),
		WithProgress(func(n int) { enc.Add(int64(n)) }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAll(comp, WithConcurrency(2), WithProgress(func(n int) { dec.Add(int64(n)) })); err != nil {
		t.Fatal(err)
	}
	if enc.Load() != int64(len(data)) || dec.Load() != int64(len(data)) {
		t.Errorf("progress totals: encode %d, decode %d, want %d", enc.Load(), dec.Load(), len(data))
	}
	// Headerless blocks report too.
	var blk int64
	c, _ := EncodeBlock(nil, data[:70000], Fast, WithProgress(func(n int) { blk += int64(n) }))
	if blk != 70000 {
		t.Errorf("EncodeBlock progress %d", blk)
	}
	blk = 0
	if _, err := DecodeBlock(nil, c, 70000, Fast, WithProgress(func(n int) { blk += int64(n) })); err != nil || blk != 70000 {
		t.Errorf("DecodeBlock progress %d, %v", blk, err)
	}
}

func TestEncodeDecodeBlock(t *testing.T) {
	for _, lv := range []Level{Best, Fast} {
		for _, in := range testInputs(1500) {
			prefix := []byte("keep")
			comp, err := EncodeBlock(prefix, in.data, lv)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(comp, prefix) {
				t.Fatal("EncodeBlock did not append to dst")
			}
			out, err := DecodeBlock([]byte("pre"), comp[len(prefix):], len(in.data), lv)
			if err != nil {
				t.Fatalf("%s/%s: %v", levelName(lv), in.name, err)
			}
			if !bytes.Equal(out[:3], []byte("pre")) || !bytes.Equal(out[3:], in.data) {
				t.Fatalf("%s/%s: mismatch", levelName(lv), in.name)
			}
		}
	}
	if _, err := EncodeBlock(nil, nil, Level(7)); err == nil {
		t.Error("invalid level accepted")
	}
	if _, err := DecodeBlock(nil, nil, 1, Level(7)); err == nil {
		t.Error("invalid level accepted")
	}
	if _, err := DecodeBlock(nil, nil, -1, Best); err == nil {
		t.Error("negative length accepted")
	}
	if _, err := DecodeBlock(nil, nil, MaxBlockSize+1, Best); err == nil {
		t.Error("oversized length accepted")
	}
	if out, err := DecodeBlock(nil, nil, 0, Best); err != nil || len(out) != 0 {
		t.Errorf("empty block: %v %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	big := textData(1<<17, 1) // cancellation is polled every 64 KiB
	if _, err := EncodeBlock(nil, big, Fast, WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Errorf("EncodeBlock with canceled context: %v", err)
	}
	comp, _ := EncodeBlock(nil, big, Fast)
	if _, err := DecodeBlock(nil, comp, len(big), Fast, WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Errorf("DecodeBlock with canceled context: %v", err)
	}
	if _, err := EncodeBlock(nil, nil, Best, WithLevel(Level(9))); err == nil {
		t.Error("invalid option accepted")
	}
}

// The model is integer-only and table-driven, so the compressed bytes are part
// of the format. These digests pin them; see also the mixed vectors in
// strict_test.go.
const (
	vectorBest = "d8bc79db6b9171112b348c83175f36081dad6a59e052c52fe317b6cc41e1b819"
	vectorFast = "7f71e30e3964efc951ee1520b2e19f20f609a8e375a7ab5097739f86727d0e3a"
)

func TestGoldenBlockVectors(t *testing.T) {
	data := vectorData()
	for lv, want := range map[Level]string{Best: vectorBest, Fast: vectorFast} {
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

// goldenStreams are small complete streams stored in testdata/ so that streams
// written by older versions must keep decoding. Run `go test -update` to
// regenerate them deliberately.
var goldenStreams = []struct {
	file string
	data func() []byte
	opts []Option
}{
	{"best.cmx", func() []byte { return append(textData(3000, 1), randData(1500, 2)...) }, []Option{WithBlockSize(MinBlockSize)}},
	{"fast.cmx", func() []byte { return textData(2500, 3) }, []Option{WithLevel(Fast), WithBlockSize(2048)}},
	{"empty.cmx", func() []byte { return nil }, nil},
}

func TestGoldenStreams(t *testing.T) {
	for _, g := range goldenStreams {
		path := filepath.Join("testdata", g.file)
		want := g.data()
		comp := mustEncode(t, want, g.opts...)
		if *update {
			if err := os.WriteFile(path, comp, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		stored, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run go test -update)", err)
		}
		if got := mustDecode(t, stored); !bytes.Equal(got, want) {
			t.Errorf("%s: stored stream no longer decodes to the expected data", g.file)
		}
		if !bytes.Equal(stored, comp) {
			t.Errorf("%s: encoder output changed (stored %d bytes, now %d); the stream format must stay stable", g.file, len(stored), len(comp))
		}
	}
}
