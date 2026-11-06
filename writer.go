package ctxmix

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Frame layout (all integers little-endian unless noted):
//
//	magic      "CMX\x01"              (the 4th byte is the format version)
//	level      1 byte
//	blockSize  uvarint
//	blocks...  kind(1) rawLen(uvarint) compLen(uvarint) crc32(4) payload
//	           kind 0 = stored (compLen == rawLen), kind 1 = compressed
//	end        kind 2, total raw length (uvarint)
const (
	frameMagic = "CMX\x01"

	kindStored = 0
	kindComp   = 1
	kindEnd    = 2
)

type encResult struct {
	kind byte
	raw  int
	crc  uint32
	data []byte
	err  error
}

// A Writer compresses data written to it and writes the compressed stream to
// the underlying io.Writer. Close must be called to flush the last block and
// write the end marker; it does not close the underlying writer.
//
// A Writer is not safe for concurrent use.
type Writer struct {
	w       io.Writer
	cfg     config
	buf     []byte
	total   int64
	err     error
	started bool
	closed  bool
	sem     chan struct{}
	queue   []chan encResult
}

// NewWriter returns a Writer that writes a compressed stream to w.
func NewWriter(w io.Writer, opts ...Option) (*Writer, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	return &Writer{w: w, cfg: cfg, sem: make(chan struct{}, cfg.workers)}, nil
}

func (w *Writer) writeHeader() error {
	var hdr [4 + 1 + binary.MaxVarintLen64]byte
	n := copy(hdr[:], frameMagic)
	hdr[n] = byte(w.cfg.level)
	n++
	n += binary.PutUvarint(hdr[n:], uint64(w.cfg.blockSize))
	_, err := w.w.Write(hdr[:n])
	w.started = true
	return err
}

func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return w.err
}

// Write compresses p. Data is buffered into blocks; it may not reach the
// underlying writer until a block fills or Close is called.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("ctxmix: write to closed Writer")
	}
	if err := w.cfg.ctx.Err(); err != nil {
		return 0, w.fail(err)
	}
	if !w.started {
		if err := w.writeHeader(); err != nil {
			return 0, w.fail(err)
		}
	}
	written := 0
	for len(p) > 0 {
		room := w.cfg.blockSize - len(w.buf)
		n := len(p)
		if n > room {
			n = room
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		written += n
		w.total += int64(n)
		if len(w.buf) == w.cfg.blockSize {
			if err := w.flushBlock(); err != nil {
				return written, w.fail(err)
			}
		}
	}
	return written, nil
}

func (w *Writer) flushBlock() error {
	if len(w.buf) == 0 {
		return nil
	}
	data := w.buf
	w.buf = nil
	ch := make(chan encResult, 1)
	w.queue = append(w.queue, ch)
	w.sem <- struct{}{}
	level, cancel, tick := w.cfg.level, w.cfg.canceled, w.cfg.progress
	go func() {
		defer func() { <-w.sem }()
		ch <- compressBlock(data, level, cancel, tick)
	}()
	for len(w.queue) > 2*w.cfg.workers {
		if err := w.emit(true); err != nil {
			return err
		}
	}
	return nil
}

func compressBlock(data []byte, level Level, cancel func() bool, tick func(int)) (r encResult) {
	r = encResult{raw: len(data), crc: crc32.ChecksumIEEE(data)}
	defer func() {
		// The encoder cannot panic on any input; this keeps a bug (or an
		// out-of-memory condition surfacing as a panic) from taking down the
		// whole process from a background goroutine.
		if p := recover(); p != nil {
			r.err = fmt.Errorf("ctxmix: internal error: %v", p)
		}
	}()
	comp, err := encodeBlock(data, level, cancel, tick)
	if err != nil {
		r.err = err
		return r
	}
	if len(comp) >= len(data) {
		r.kind, r.data = kindStored, data
	} else {
		r.kind, r.data = kindComp, comp
	}
	return r
}

// emit waits for the oldest pending block and, if write is true, writes it out.
func (w *Writer) emit(write bool) error {
	ch := w.queue[0]
	w.queue = w.queue[1:]
	r := <-ch
	if r.err != nil {
		if r.err == errCanceled {
			if cerr := w.cfg.ctx.Err(); cerr != nil {
				return cerr
			}
		}
		return r.err
	}
	if !write {
		return nil
	}
	var hdr [1 + 2*binary.MaxVarintLen64 + 4]byte
	hdr[0] = r.kind
	n := 1
	n += binary.PutUvarint(hdr[n:], uint64(r.raw))
	n += binary.PutUvarint(hdr[n:], uint64(len(r.data)))
	binary.LittleEndian.PutUint32(hdr[n:], r.crc)
	n += 4
	if _, err := w.w.Write(hdr[:n]); err != nil {
		return err
	}
	_, err := w.w.Write(r.data)
	return err
}

// Close flushes buffered data and writes the end marker. It returns the first
// error encountered by the Writer, if any. Closing twice is a no-op.
func (w *Writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err == nil && !w.started {
		if err := w.writeHeader(); err != nil {
			w.fail(err)
		}
	}
	if w.err == nil {
		if err := w.flushBlock(); err != nil {
			w.fail(err)
		}
	}
	for len(w.queue) > 0 {
		if err := w.emit(w.err == nil); err != nil {
			w.fail(err)
		}
	}
	if w.err != nil {
		return w.err
	}
	var end [1 + binary.MaxVarintLen64]byte
	end[0] = kindEnd
	n := 1 + binary.PutUvarint(end[1:], uint64(w.total))
	if _, err := w.w.Write(end[:n]); err != nil {
		return w.fail(err)
	}
	return nil
}
