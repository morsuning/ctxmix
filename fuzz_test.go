package ctxmix

import (
	"bytes"
	"errors"
	"testing"
)

func FuzzRoundtrip(f *testing.F) {
	f.Add([]byte(""), uint8(0))
	f.Add([]byte("a"), uint8(1))
	f.Add([]byte("hello hello hello hello"), uint8(0))
	f.Add(bytes.Repeat([]byte{0}, 3000), uint8(1))
	f.Add(textData(1500, 4), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		if len(data) > 3072 {
			data = data[:3072]
		}
		lv := Level(mode & 1)
		comp, err := EncodeAll(data, WithLevel(lv), WithBlockSize(MinBlockSize), WithConcurrency(1+int(mode>>1)%3))
		if err != nil {
			t.Fatal(err)
		}
		out, err := DecodeAll(comp)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("roundtrip mismatch")
		}
		blk, err := EncodeBlock(nil, data, lv)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := DecodeBlock(nil, blk, len(data), lv); err != nil || !bytes.Equal(out, data) {
			t.Fatalf("block roundtrip: %v", err)
		}
	})
}

func FuzzDecode(f *testing.F) {
	stream, _ := smallStream(f)
	f.Add(stream)
	f.Add([]byte("CMX\x01\x00\x80\x08"))
	f.Add([]byte("CMX\x01\x01\x80\x08\x01\x05\x05\x00\x00\x00\x00hello\x02\x05"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := DecodeAll(in, WithMaxBlockSize(MinBlockSize))
		// Inspect only looks at the framing, so it must accept everything that
		// decodes, and agree on the size.
		info, ierr := Inspect(bytes.NewReader(in))
		if err == nil {
			if ierr != nil || info.RawSize != int64(len(out)) || info.CompressedSize != int64(len(in)) {
				t.Fatalf("Inspect disagrees with DecodeAll: %+v, %v", info, ierr)
			}
		} else if ierr != nil && !acceptableDecodeError(ierr) {
			t.Fatalf("unexpected Inspect error type: %v", ierr)
		}
		if err == nil {
			// Anything accepted must re-encode and decode to the same bytes.
			again, err := DecodeAll(mustEncode(t, out, WithBlockSize(MinBlockSize)))
			if err != nil || !bytes.Equal(again, out) {
				t.Fatalf("accepted stream does not roundtrip: %v", err)
			}
		} else if !acceptableDecodeError(err) {
			t.Fatalf("unexpected error type: %v", err)
		}
	})
}

func FuzzDecodeBlock(f *testing.F) {
	data := textData(700, 9)
	for _, lv := range []Level{Best, Fast} {
		comp, _ := EncodeBlock(nil, data, lv)
		f.Add(comp, uint16(len(data)), uint8(lv))
	}
	f.Add([]byte{}, uint16(0), uint8(0))
	f.Add([]byte{1, 2, 3, 4, 5}, uint16(40), uint8(1))
	f.Fuzz(func(t *testing.T, comp []byte, n uint16, mode uint8) {
		lv := Level(mode & 1)
		size := int(n) % 4096
		out, err := DecodeBlock(nil, comp, size, lv)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("error %v is not ErrCorrupt", err)
			}
			return
		}
		if len(out) != size {
			t.Fatalf("returned %d bytes, want %d", len(out), size)
		}
		// Whatever was accepted must survive a re-encode, and the re-encoded
		// payload has the same length (the length is canonical).
		again, err := EncodeBlock(nil, out, lv)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(comp) {
			t.Fatalf("payload length %d, canonical length %d", len(comp), len(again))
		}
		back, err := DecodeBlock(nil, again, size, lv)
		if err != nil || !bytes.Equal(back, out) {
			t.Fatalf("re-encoded block does not decode: %v", err)
		}
	})
}
