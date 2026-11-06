package ctxmix

import (
	"fmt"
	"testing"
)

func BenchmarkEncode(b *testing.B) {
	data := textData(256<<10, 1)
	for _, lv := range []Level{Best, Fast} {
		b.Run(levelName(lv), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			var comp []byte
			for i := 0; i < b.N; i++ {
				var err error
				if comp, err = EncodeAll(data, WithLevel(lv)); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(comp))*100/float64(len(data)), "%size")
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	data := textData(256<<10, 1)
	for _, lv := range []Level{Best, Fast} {
		comp, _ := EncodeAll(data, WithLevel(lv))
		b.Run(levelName(lv), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				if _, err := DecodeAll(comp); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkEncodeParallel(b *testing.B) {
	data := textData(1<<20, 1)
	for _, w := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("workers=%d", w), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				if _, err := EncodeAll(data, WithLevel(Fast), WithBlockSize(256<<10), WithConcurrency(w)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
