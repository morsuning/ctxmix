package ctxmix

import "errors"

// configFor derives the model configuration for a block of n bytes. It must be a
// pure function of (n, level): the decoder recomputes it from the frame header.
func configFor(n int, level Level) cmConfig {
	cfg := cmConfig{ids: cmDefaultIDs, nmix: cmNMix, htLog: 12, r1: 7, r2: 3, lim: 1023}
	if level == Fast {
		cfg.ids = cmFastIDs
		cfg.nmix = 2
	}
	for (1<<cfg.htLog) < 4*n && cfg.htLog < 22 {
		cfg.htLog++
	}
	return cfg
}

// matchBits is the log2 size of the match models' hash tables for a block of n bytes.
func matchBits(n int) uint {
	mb := uint(16)
	for (1<<mb) < n && mb < 22 {
		mb++
	}
	return mb
}

// modelFixedBytes covers the model's fixed-size structures (state maps, headers).
const modelFixedBytes = 21 << 10

// modelBytes returns the heap memory of one model instance for a block of n
// bytes (n > 0): the hashed context table, the two match-model tables, the two
// APM stages, and the mixers.
func modelBytes(n int, level Level) int64 {
	cfg := configFor(n, level)
	nin := int64(2*len(cfg.ids) + 1 + 4 + 1)
	total := int64(16) << cfg.htLog                       // context hash table
	total += 2 * (int64(4) << matchBits(n))               // match-model tables
	total += (int64(1<<16) + int64(1<<14)) * cmApmLen * 2 // APM tables
	mixSets := int64(256 + 64*8 + 256 + 17*8)             // first-layer mixer weight sets (all four are allocated)
	total += mixSets*nin*4 + 256*int64(cfg.nmix+1)*4      // mixer weights
	return total + modelFixedBytes
}

var (
	errCanceled = errors.New("ctxmix: canceled")
	// ErrCorrupt is returned when compressed data is malformed, truncated or
	// fails its integrity check.
	ErrCorrupt = errors.New("ctxmix: corrupt data")
	// ErrTooLarge is returned when a stream declares more decompressed data than
	// the limit set with WithMaxOutputSize.
	ErrTooLarge = errors.New("ctxmix: decompressed size exceeds the configured limit")
)

// encodeBlock compresses src as one independent block. The output carries no
// header: the caller must remember len(src) and level. cancel (optional) is polled
// every 64 KiB, and tick (optional) is called with the number of bytes processed
// since its previous call.
func encodeBlock(src []byte, level Level, cancel func() bool, tick func(int)) ([]byte, error) {
	n := len(src)
	if n == 0 {
		return nil, nil
	}
	cfg := configFor(n, level)
	cfg.cancel = cancel
	m := newCM(src, n, false, cfg)
	e := &cmEnc{x1: 0, x2: 0xffffffff, out: make([]byte, 0, n/3+16)}
	for i := 0; i < n; i++ {
		if i&0xffff == 0xffff {
			if cancel != nil && cancel() {
				return nil, errCanceled
			}
			if tick != nil {
				tick(0x10000)
			}
		}
		c := uint32(src[i])
		for b := 7; b >= 0; b-- {
			y := int(c>>uint(b)) & 1
			e.encode(y, m.predict())
			m.update(y)
		}
	}
	e.flush()
	if tick != nil && n&0xffff != 0 {
		tick(n & 0xffff)
	}
	return e.out, nil
}

// decodeBlock reverses encodeBlock and returns exactly n bytes. The decoder is
// strict about the payload length: it must be exactly the length encodeBlock
// produces, so truncated, zero-padded or over-long payloads are rejected. (The
// last four bytes are the arithmetic coder's final interval bound; any value
// inside the interval decodes identically, so equal output does not imply an
// equal payload. Data integrity is the job of the block CRC-32.)
func decodeBlock(comp []byte, n int, level Level, cancel func() bool, tick func(int)) (out []byte, err error) {
	if n == 0 {
		if len(comp) != 0 {
			return nil, ErrCorrupt
		}
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, ErrCorrupt
		}
	}()
	cfg := configFor(n, level)
	buf := make([]byte, n)
	m := newCM(buf, n, true, cfg)
	var x1, x2, x uint32 = 0, 0xffffffff, 0
	ip := 0
	next := func() uint32 {
		ip++
		if ip <= len(comp) {
			return uint32(comp[ip-1])
		}
		return 0
	}
	for i := 0; i < 4; i++ {
		x = x<<8 | next()
	}
	for i := 0; i < n; i++ {
		if i&0xffff == 0xffff {
			if cancel != nil && cancel() {
				return nil, errCanceled
			}
			if ip > len(comp) { // a canonical payload is never read past its end
				return nil, ErrCorrupt
			}
			if tick != nil {
				tick(0x10000)
			}
		}
		for b := 0; b < 8; b++ {
			p := m.predict()
			r := x2 - x1
			xmid := x1 + (r>>16)*p + ((r&0xffff)*p)>>16
			var y int
			if x <= xmid {
				y = 1
				x2 = xmid
			} else {
				x1 = xmid + 1
			}
			for (x1^x2)&0xff000000 == 0 {
				x1 <<= 8
				x2 = x2<<8 | 255
				x = x<<8 | next()
			}
			m.update(y)
		}
	}
	// The decoder consumes exactly as many bytes as the encoder produced: one
	// per renormalization shift plus the four initial/flush bytes.
	if ip != len(comp) {
		return nil, ErrCorrupt
	}
	if tick != nil && n&0xffff != 0 {
		tick(n & 0xffff)
	}
	return buf, nil
}
