package ctxmix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// Level selects the speed / ratio trade-off.
type Level int

const (
	// Best uses the full model: smallest output, slowest (about 0.2-0.5 MB/s per core).
	Best Level = 0
	// Fast uses fewer contexts and mixers: about twice as fast, output roughly 8% larger.
	Fast Level = 1
)

func (l Level) valid() bool { return l == Best || l == Fast }

const (
	// DefaultBlockSize is the default amount of input compressed as one independent block.
	DefaultBlockSize = 16 << 20
	// MinBlockSize and MaxBlockSize bound the block size accepted by WithBlockSize and by readers.
	MinBlockSize = 1 << 10
	MaxBlockSize = 64 << 20
)

type config struct {
	level     Level
	blockSize int
	workers   int
	maxBlock  int
	maxOutput int64 // 0 = unlimited
	ctx       context.Context
	progress  func(int)
}

func defaultConfig() config {
	return config{level: Best, blockSize: DefaultBlockSize, workers: 1, maxBlock: MaxBlockSize, ctx: context.Background()}
}

// An Option configures a Writer, a Reader or one of the *All functions.
type Option func(*config) error

// WithLevel sets the compression level (writers only). The default is Best.
func WithLevel(l Level) Option {
	return func(c *config) error {
		if !l.valid() {
			return fmt.Errorf("ctxmix: invalid level %d", int(l))
		}
		c.level = l
		return nil
	}
}

// WithBlockSize sets how much input is compressed as one independent block
// (writers only). Larger blocks compress slightly better but use more memory
// and offer less parallelism. The default is DefaultBlockSize.
func WithBlockSize(n int) Option {
	return func(c *config) error {
		if n < MinBlockSize || n > MaxBlockSize {
			return fmt.Errorf("ctxmix: block size %d out of range [%d, %d]", n, MinBlockSize, MaxBlockSize)
		}
		c.blockSize = n
		return nil
	}
}

// WithConcurrency sets how many blocks are processed in parallel (writers and
// readers). The default is 1. Memory use grows linearly with it: each worker
// needs MemoryUsage(blockSize, level), about 100 MiB at the default block size,
// plus buffers for the data of up to two blocks per worker. The compressed
// output does not depend on this setting.
func WithConcurrency(n int) Option {
	return func(c *config) error {
		if n < 1 || n > 256 {
			return fmt.Errorf("ctxmix: invalid concurrency %d", n)
		}
		c.workers = n
		return nil
	}
}

// WithMaxBlockSize makes a Reader reject streams whose block size exceeds n,
// bounding the memory a malicious stream can make the decoder allocate. The
// default is MaxBlockSize.
func WithMaxBlockSize(n int) Option {
	return func(c *config) error {
		if n < 1 || n > MaxBlockSize {
			return fmt.Errorf("ctxmix: invalid max block size %d", n)
		}
		c.maxBlock = n
		return nil
	}
}

// WithMaxOutputSize makes a Reader (or DecodeAll) fail with ErrTooLarge as soon
// as the stream declares more than n bytes of decompressed data, before any of
// the offending block is decoded. It bounds the work and memory a decompression
// bomb can cause. The default is no limit. Data of earlier blocks that is
// already within the limit is still delivered first.
func WithMaxOutputSize(n int64) Option {
	return func(c *config) error {
		if n < 1 {
			return fmt.Errorf("ctxmix: invalid max output size %d", n)
		}
		c.maxOutput = n
		return nil
	}
}

// WithContext makes long operations abort with ctx.Err() soon after ctx is
// canceled (checked about every 64 KiB of data inside each block).
func WithContext(ctx context.Context) Option {
	return func(c *config) error {
		if ctx == nil {
			return errors.New("ctxmix: nil context")
		}
		c.ctx = ctx
		return nil
	}
}

// WithProgress registers fn to be called as data is processed. n is the number of
// uncompressed bytes handled since the previous call, reported in steps of
// 64 KiB and a final remainder per block, so the values sum to the amount of
// data processed. With WithConcurrency above 1, fn is called from several
// goroutines at once and must be safe for concurrent use.
func WithProgress(fn func(n int)) Option {
	return func(c *config) error {
		c.progress = fn
		return nil
	}
}

func newConfig(opts []Option) (config, error) {
	c := defaultConfig()
	for _, o := range opts {
		if err := o(&c); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (c *config) canceled() bool { return c.ctx.Err() != nil }

// cancelErr turns the internal cancellation marker into the context's error.
func (c *config) cancelErr(err error) error {
	if err == errCanceled {
		if cerr := c.ctx.Err(); cerr != nil {
			return cerr
		}
	}
	return err
}

// EncodeAll compresses src into a complete stream.
func EncodeAll(src []byte, opts ...Option) ([]byte, error) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, opts...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeAll decompresses a complete stream produced by EncodeAll or a Writer.
// Data after the end of the stream is an error. Use WithMaxOutputSize when the
// input is untrusted.
func DecodeAll(comp []byte, opts ...Option) ([]byte, error) {
	br := bytes.NewReader(comp)
	r, err := NewReader(br, opts...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if br.Len() != 0 {
		return nil, fmt.Errorf("%w: %d bytes of trailing data", ErrCorrupt, br.Len())
	}
	return out, nil
}

// MemoryUsage returns the heap memory, in bytes, that one encoder or decoder
// needs for a block of blockSize bytes at the given level: the model tables,
// which dominate (about 100 MiB at the default block size). It
// excludes the block's own input and output buffers. A Writer or Reader with
// WithConcurrency(n) runs up to n of them at once and additionally buffers up to
// 2n blocks of data. blockSize is clamped to [MinBlockSize, MaxBlockSize].
func MemoryUsage(blockSize int, level Level) int64 {
	if blockSize < MinBlockSize {
		blockSize = MinBlockSize
	}
	if blockSize > MaxBlockSize {
		blockSize = MaxBlockSize
	}
	if !level.valid() {
		level = Best
	}
	return modelBytes(blockSize, level)
}

// EncodeBlock compresses src as a single headerless block and appends the result
// to dst. The caller must remember len(src) and level to decode it, and gets no
// integrity check: use a Writer unless you are building your own container.
// src may be at most MaxBlockSize bytes. Only WithContext and WithProgress are
// meaningful here; the other options are ignored.
func EncodeBlock(dst, src []byte, level Level, opts ...Option) ([]byte, error) {
	if !level.valid() {
		return dst, fmt.Errorf("ctxmix: invalid level %d", int(level))
	}
	if len(src) > MaxBlockSize {
		return dst, fmt.Errorf("ctxmix: block of %d bytes exceeds MaxBlockSize", len(src))
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return dst, err
	}
	out, err := encodeBlock(src, level, cfg.canceled, cfg.progress)
	if err != nil {
		return dst, cfg.cancelErr(err)
	}
	return append(dst, out...), nil
}

// DecodeBlock reverses EncodeBlock. It appends exactly rawLen bytes to dst.
// Corrupt input yields ErrCorrupt (a headerless block has no checksum, so some
// corruption can go undetected). Only WithContext and WithProgress are meaningful here.
func DecodeBlock(dst, comp []byte, rawLen int, level Level, opts ...Option) ([]byte, error) {
	if !level.valid() {
		return dst, fmt.Errorf("ctxmix: invalid level %d", int(level))
	}
	if rawLen < 0 || rawLen > MaxBlockSize {
		return dst, fmt.Errorf("ctxmix: invalid length %d", rawLen)
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return dst, err
	}
	out, err := decodeBlock(comp, rawLen, level, cfg.canceled, cfg.progress)
	if err != nil {
		return dst, cfg.cancelErr(err)
	}
	return append(dst, out...), nil
}
