package ctxmix_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/morsuning/ctxmix"
)

func ExampleEncodeAll() {
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 200))

	comp, err := ctxmix.EncodeAll(data)
	if err != nil {
		log.Fatal(err)
	}
	back, err := ctxmix.DecodeAll(comp)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(data), "bytes, smaller than 1/50:", len(comp)*50 < len(data), "roundtrip ok:", bytes.Equal(back, data))
	// Output: 8800 bytes, smaller than 1/50: true roundtrip ok: true
}

func ExampleNewWriter() {
	var compressed bytes.Buffer

	w, err := ctxmix.NewWriter(&compressed, ctxmix.WithLevel(ctxmix.Fast), ctxmix.WithConcurrency(2))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := io.Copy(w, strings.NewReader(strings.Repeat("hello, world\n", 1000))); err != nil {
		log.Fatal(err)
	}
	// Close flushes the last block and writes the end marker; it is required.
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Println(compressed.Len() < 200)
	// Output: true
}

func ExampleNewReader() {
	comp, err := ctxmix.EncodeAll([]byte("some data to compress, some data to compress"))
	if err != nil {
		log.Fatal(err)
	}

	r, err := ctxmix.NewReader(bytes.NewReader(comp))
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
	// Output: some data to compress, some data to compress
}

// Decoding untrusted input: cap the block size the stream may demand (and with
// it the memory the decoder allocates) ...
func ExampleWithMaxBlockSize() {
	comp, _ := ctxmix.EncodeAll([]byte("small"), ctxmix.WithBlockSize(1<<20))

	_, err := ctxmix.NewReader(bytes.NewReader(comp), ctxmix.WithMaxBlockSize(1<<16))
	fmt.Println(err != nil)
	// Output: true
}

// ... and the amount of data you are willing to produce. The limit is checked
// against the sizes announced in the stream, before any block is decoded.
func ExampleWithMaxOutputSize() {
	comp, _ := ctxmix.EncodeAll(bytes.Repeat([]byte{'x'}, 1<<17))

	_, err := ctxmix.DecodeAll(comp, ctxmix.WithMaxOutputSize(1<<16))
	fmt.Println(errors.Is(err, ctxmix.ErrTooLarge))
	// Output: true
}

// Inspect reports the structure of a stream without decompressing it.
func ExampleInspect() {
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 200))
	comp, _ := ctxmix.EncodeAll(data, ctxmix.WithBlockSize(4096))

	info, err := ctxmix.Inspect(bytes.NewReader(comp))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d bytes in %d blocks of up to %d bytes\n", info.RawSize, info.Blocks, info.BlockSize)
	// Output: 8800 bytes in 3 blocks of up to 4096 bytes
}

// MemoryUsage helps to budget memory: each block being processed needs this
// much, plus its data.
func ExampleMemoryUsage() {
	mib := ctxmix.MemoryUsage(ctxmix.DefaultBlockSize, ctxmix.Best) >> 20
	fmt.Println("about", mib, "MiB per worker")
	// Output: about 100 MiB per worker
}

func ExampleEncodeBlock() {
	data := []byte(strings.Repeat("abc", 1000))

	// Headerless: the caller stores the length and level next to the data.
	comp, err := ctxmix.EncodeBlock(nil, data, ctxmix.Best)
	if err != nil {
		log.Fatal(err)
	}
	back, err := ctxmix.DecodeBlock(nil, comp, len(data), ctxmix.Best)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(bytes.Equal(back, data))
	// Output: true
}
