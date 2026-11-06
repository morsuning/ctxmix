package ctxmix

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// acceptableDecodeError reports whether err is one of the documented ways
// decoding bad input may fail.
func acceptableDecodeError(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.HasPrefix(err.Error(), "ctxmix:")
}

// smallStream returns a two-block stream (one compressed, one stored block)
// together with its plain data.
func smallStream(t testing.TB) (stream, data []byte) {
	data = append(textData(1024, 8), randData(300, 9)...)
	return mustEncode(t, data, WithBlockSize(MinBlockSize)), data
}

// Whatever happens to the stream, DecodeAll must either fail cleanly or
// return the original data; it must never return different data or panic.
func TestBitFlips(t *testing.T) {
	stream, data := smallStream(t)
	check := func(i int, bit uint) {
		bad := append([]byte(nil), stream...)
		bad[i] ^= 1 << bit
		out, err := DecodeAll(bad)
		if err != nil {
			if !acceptableDecodeError(err) {
				t.Errorf("byte %d bit %d: unexpected error type: %v", i, bit, err)
			}
			return
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("byte %d bit %d: corrupt stream decoded to wrong data without an error", i, bit)
		}
	}
	// Every bit of the headers and the first block's lengths...
	for i := 0; i < 24 && i < len(stream); i++ {
		for bit := uint(0); bit < 8; bit++ {
			check(i, bit)
		}
	}
	// ...and a rotating bit everywhere else.
	step := 1
	if testing.Short() {
		step = 5
	}
	for i := 24; i < len(stream); i += step {
		check(i, uint(i)%8)
	}
}

func TestTruncation(t *testing.T) {
	stream, _ := smallStream(t)
	for n := 0; n < len(stream); n++ {
		out, err := DecodeAll(stream[:n])
		if err == nil {
			t.Fatalf("prefix of %d/%d bytes decoded without error (%d bytes of output)", n, len(stream), len(out))
		}
		if !acceptableDecodeError(err) {
			t.Errorf("prefix %d: unexpected error: %v", n, err)
		}
	}
}

// A truncated stream still yields every block that arrived intact, in order,
// before the error.
func TestTruncatedStreamYieldsPrefix(t *testing.T) {
	data := textData(3000, 77)
	stream := mustEncode(t, data, WithBlockSize(MinBlockSize), WithLevel(Fast))
	r, err := NewReader(bytes.NewReader(stream[:len(stream)-9]), WithConcurrency(2))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !bytes.HasPrefix(data, got) || len(got) == 0 {
		t.Errorf("got %d bytes that are not a prefix of the data", len(got))
	}
}

func TestGarbageNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	iters := 300
	if testing.Short() {
		iters = 60
	}
	hdr := []byte("CMX\x01\x00\x80\x08") // valid header: best, block size 1024
	for i := 0; i < iters; i++ {
		body := make([]byte, rng.Intn(200))
		rng.Read(body)
		in := append(append([]byte(nil), hdr...), body...)
		if i%3 == 0 { // plausible block header with random payload
			in = append(in[:len(hdr)], kindComp, 0x80, 0x08, 0x40, 0, 0, 0, 0)
			in = append(in, body...)
		}
		if i%3 == 1 && len(in) > 4 {
			in[4] = byte(i % 2) // level
		}
		out, err := DecodeAll(in, WithMaxBlockSize(1<<12))
		if err == nil && len(out) > 1<<12*8 {
			t.Fatalf("iter %d: implausible output", i)
		}
		if err != nil && !acceptableDecodeError(err) {
			t.Fatalf("iter %d: unexpected error type: %v", i, err)
		}
	}
	// Pure garbage, including the right magic only.
	for i := 0; i < 100; i++ {
		b := make([]byte, rng.Intn(64))
		rng.Read(b)
		DecodeAll(b)
	}
}

// DecodeBlock has no checksum but must still never panic or run away on bad input.
func TestDecodeBlockGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		comp := make([]byte, rng.Intn(300))
		rng.Read(comp)
		n := rng.Intn(3000)
		for _, lv := range []Level{Best, Fast} {
			out, err := DecodeBlock(nil, comp, n, lv)
			if err == nil && len(out) != n {
				t.Fatalf("returned %d bytes, want %d", len(out), n)
			}
		}
	}
	// A block cut short must be reported, not zero-filled silently.
	data := textData(2000, 5)
	comp, _ := EncodeBlock(nil, data, Best)
	if out, err := DecodeBlock(nil, comp[:len(comp)/2], len(data), Best); err == nil && bytes.Equal(out, data) {
		t.Error("truncated block decoded to the original data")
	}
}

func TestDeclaredLengthMismatch(t *testing.T) {
	stream, _ := smallStream(t)
	// The end marker is the last bytes: kind(2) + uvarint(total). Alter the total.
	bad := append([]byte(nil), stream...)
	bad[len(bad)-1] ^= 0x01
	if _, err := DecodeAll(bad); !errors.Is(err, ErrCorrupt) {
		t.Errorf("wrong total length: %v", err)
	}
}

// Found by fuzzing: an over-long varint used to surface encoding/binary's error
// instead of ErrCorrupt.
func TestVarintOverflow(t *testing.T) {
	over := bytes.Repeat([]byte{0xff}, 10)
	for name, in := range map[string][]byte{
		"blocksize": append([]byte("CMX\x01\x00"), over...),
		"rawlen":    append([]byte("CMX\x01\x00\x80\x08\x01"), over...),
		"complen":   append([]byte("CMX\x01\x00\x80\x08\x01\x05"), over...),
		"total":     append([]byte("CMX\x01\x00\x80\x08\x02"), over...),
		"too-long":  append([]byte("CMX\x01\x00"), bytes.Repeat([]byte{0x80}, 11)...),
	} {
		if _, err := DecodeAll(in); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
	}
}
