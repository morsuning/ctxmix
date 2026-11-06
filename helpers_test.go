package ctxmix

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"strconv"
	"testing"
)

// textData returns deterministic, compressible, source-code-like data.
func textData(n int, seed uint32) []byte {
	var b bytes.Buffer
	x := seed
	words := []string{"alpha", "beta", "gamma", "delta", "func", "return", "if", "else", "for", "range", "x", "y", "(", ")", "{", "}", "\n", "\t", " ", "=", "+"}
	for b.Len() < n {
		x = x*1664525 + 1013904223
		b.WriteString(words[(x>>16)%uint32(len(words))])
		if (x>>8)%7 == 0 {
			b.WriteByte(byte(x >> 24))
		}
	}
	return b.Bytes()[:n]
}

// randData returns deterministic incompressible data.
func randData(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// vectorData is the fixed input of the golden vectors. Do not change it.
func vectorData() []byte { return textData(300000, 12345) }

type testInput struct {
	name string
	data []byte
}

func testInputs(sizes ...int) []testInput {
	var out []testInput
	for _, n := range sizes {
		out = append(out,
			testInput{"text", textData(n, uint32(n)+1)},
			testInput{"random", randData(n, int64(n)+1)},
			testInput{"zeros", make([]byte, n)},
			testInput{"ones", bytes.Repeat([]byte{0xff}, n)},
			testInput{"period", bytes.Repeat([]byte("abcdefghij0123456789"), n/20+1)[:n]},
		)
	}
	return out
}

func mustEncode(t testing.TB, data []byte, opts ...Option) []byte {
	t.Helper()
	comp, err := EncodeAll(data, opts...)
	if err != nil {
		t.Fatalf("EncodeAll: %v", err)
	}
	return comp
}

func mustDecode(t testing.TB, comp []byte, opts ...Option) []byte {
	t.Helper()
	out, err := DecodeAll(comp, opts...)
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	return out
}

func levelName(l Level) string {
	if l == Fast {
		return "fast"
	}
	return "best"
}

// short scales a size down in -short mode.
func short(n int) int {
	if testing.Short() {
		return n / 4
	}
	return n
}

// mixedVectorData is the fixed input of the second set of golden vectors. It
// exercises parts of the model that vectorData does not: binary records, long
// repeats (the match models), zero runs, indentation and incompressible bytes.
// Do not change it.
func mixedVectorData() []byte {
	var b bytes.Buffer
	text := textData(40000, 77)
	b.Write(text)
	for i := uint32(0); i < 4096; i++ {
		var rec [8]byte
		binary.LittleEndian.PutUint32(rec[0:], i*7)
		binary.LittleEndian.PutUint32(rec[4:], i*i)
		b.Write(rec[:])
	}
	b.Write(text[5000:13000]) // a long repeat
	b.Write(make([]byte, 6000))
	for i := 0; i < 400; i++ {
		b.WriteString("\t\tif x" + strconv.Itoa(i%17) + " == " + strconv.Itoa(i*i%101) + " {\n\t\t\treturn\n\t\t}\n")
	}
	b.Write(randData(3000, 3))
	return b.Bytes()
}
