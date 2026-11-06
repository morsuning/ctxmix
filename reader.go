package ctxmix

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync/atomic"
)

type decResult struct {
	data []byte
	err  error
}

type byteReader interface {
	io.Reader
	io.ByteReader
}

// oneByteReader adapts a plain io.Reader so uvarints can be read without
// consuming bytes past the end of the stream.
type oneByteReader struct {
	r   io.Reader
	buf [1]byte
}

func (o *oneByteReader) Read(p []byte) (int, error) { return o.r.Read(p) }
func (o *oneByteReader) ReadByte() (byte, error) {
	if _, err := io.ReadFull(o.r, o.buf[:]); err != nil {
		return 0, err
	}
	return o.buf[0], nil
}

func asByteReader(r io.Reader) byteReader {
	if br, ok := r.(byteReader); ok {
		return br
	}
	return &oneByteReader{r: r}
}

// readUvarint is binary.ReadUvarint, except that a malformed varint is
// reported as ErrCorrupt instead of an error from another package, and that
// the encoding must be canonical (no redundant trailing zero groups), so that
// every stream has exactly one valid byte representation of its framing.
func readUvarint(r io.ByteReader) (uint64, error) {
	var x uint64
	var shift uint
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				break
			}
			if i > 0 && b == 0 {
				return 0, fmt.Errorf("%w: non-canonical varint", ErrCorrupt)
			}
			return x | uint64(b)<<shift, nil
		}
		x |= uint64(b&0x7f) << shift
		shift += 7
	}
	return 0, fmt.Errorf("%w: varint overflow", ErrCorrupt)
}

func headerErr(err error) error {
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// streamHeader is the fixed part at the start of every stream.
type streamHeader struct {
	level     Level
	blockSize int
}

// readStreamHeader parses and validates the stream header. maxBlock is the
// largest block size the caller is willing to accept.
func readStreamHeader(br byteReader, maxBlock int) (streamHeader, error) {
	var h streamHeader
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return h, headerErr(err)
	}
	if string(magic[:3]) != frameMagic[:3] {
		return h, fmt.Errorf("%w: not a ctxmix stream", ErrCorrupt)
	}
	if magic[3] != frameMagic[3] {
		return h, fmt.Errorf("ctxmix: unsupported format version %d", magic[3])
	}
	lv, err := br.ReadByte()
	if err != nil {
		return h, headerErr(err)
	}
	h.level = Level(lv)
	if !h.level.valid() {
		return h, fmt.Errorf("%w: invalid level %d", ErrCorrupt, lv)
	}
	bs, err := readUvarint(br)
	if err != nil {
		return h, headerErr(err)
	}
	if bs < MinBlockSize || bs > uint64(MaxBlockSize) {
		return h, fmt.Errorf("%w: invalid block size %d", ErrCorrupt, bs)
	}
	if bs > uint64(maxBlock) {
		return h, fmt.Errorf("ctxmix: stream block size %d exceeds the configured limit %d", bs, maxBlock)
	}
	h.blockSize = int(bs)
	return h, nil
}

// blockHeader is the header of one block, or of the end marker.
type blockHeader struct {
	kind    byte
	rawLen  uint64
	compLen uint64
	crc     uint32
}

// readBlockHeader parses the next block header (or the end marker) and checks
// it against the stream header. scheduled is the number of raw bytes announced
// by the blocks so far, used to validate the end marker.
func readBlockHeader(br byteReader, sh streamHeader, scheduled int64) (blockHeader, error) {
	var h blockHeader
	kind, err := br.ReadByte()
	if err != nil {
		return h, headerErr(err)
	}
	h.kind = kind
	if kind == kindEnd {
		total, err := readUvarint(br)
		if err != nil {
			return h, headerErr(err)
		}
		if total != uint64(scheduled) {
			return h, fmt.Errorf("%w: length mismatch (stream says %d bytes, blocks hold %d)", ErrCorrupt, total, scheduled)
		}
		return h, nil
	}
	if kind != kindStored && kind != kindComp {
		return h, fmt.Errorf("%w: unknown block kind %d", ErrCorrupt, kind)
	}
	if h.rawLen, err = readUvarint(br); err != nil {
		return h, headerErr(err)
	}
	if h.compLen, err = readUvarint(br); err != nil {
		return h, headerErr(err)
	}
	if h.rawLen == 0 || h.rawLen > uint64(sh.blockSize) || h.compLen == 0 || h.compLen > h.rawLen ||
		(kind == kindStored && h.compLen != h.rawLen) || (kind == kindComp && h.compLen == h.rawLen) {
		return h, fmt.Errorf("%w: invalid block lengths", ErrCorrupt)
	}
	var crcb [4]byte
	if _, err := io.ReadFull(br, crcb[:]); err != nil {
		return h, headerErr(err)
	}
	h.crc = binary.LittleEndian.Uint32(crcb[:])
	return h, nil
}

// readPayload reads exactly n bytes. The buffer grows as data actually arrives,
// so a header that announces a huge block followed by a truncated stream cannot
// force a large allocation up front.
func readPayload(r io.Reader, n int) ([]byte, error) {
	const chunk = 1 << 20
	if n <= chunk {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		return b, headerErr(err)
	}
	b := make([]byte, 0, chunk)
	for len(b) < n {
		m := n - len(b)
		if m > chunk {
			m = chunk
		}
		if cap(b)-len(b) < m {
			nc := 2 * cap(b)
			if nc > n {
				nc = n
			}
			nb := make([]byte, len(b), nc)
			copy(nb, b)
			b = nb
		}
		if _, err := io.ReadFull(r, b[len(b):len(b)+m]); err != nil {
			return nil, headerErr(err)
		}
		b = b[:len(b)+m]
	}
	return b, nil
}

// A Reader decompresses a stream produced by a Writer.
//
// A Reader reads from the underlying reader only as much as the stream needs
// and never past the end marker, so concatenated streams or trailing data can
// be handled by the caller. It is not safe for concurrent use, except that
// Close may be called from another goroutine to abort a Read in progress.
type Reader struct {
	src       byteReader
	cfg       config
	hdr       streamHeader
	cur       []byte
	queue     []chan decResult
	sem       chan struct{}
	endSeen   bool
	scheduled int64
	schedErr  error // error hit while reading the stream; reported after queued blocks drain
	err       error
	stop      atomic.Bool
}

// NewReader returns a Reader for the compressed stream in r. It reads and
// validates the stream header immediately. If r is not already an
// io.ByteReader it is not buffered; wrap it in a bufio.Reader for speed.
func NewReader(r io.Reader, opts ...Option) (*Reader, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	br := asByteReader(r)
	hdr, err := readStreamHeader(br, cfg.maxBlock)
	if err != nil {
		return nil, err
	}
	return &Reader{src: br, cfg: cfg, hdr: hdr, sem: make(chan struct{}, cfg.workers)}, nil
}

// Close stops background decoding; later calls to Read fail with
// io.ErrClosedPipe. It does not close the underlying reader. It is safe to call
// Close more than once.
func (r *Reader) Close() error {
	r.stop.Store(true)
	return nil
}

func (r *Reader) cancelled() bool { return r.stop.Load() || r.cfg.canceled() }

// schedule reads the next block from the stream and starts decoding it.
func (r *Reader) schedule() error {
	bh, err := readBlockHeader(r.src, r.hdr, r.scheduled)
	if err != nil {
		return err
	}
	if bh.kind == kindEnd {
		r.endSeen = true
		return nil
	}
	if r.cfg.maxOutput > 0 && r.scheduled+int64(bh.rawLen) > r.cfg.maxOutput {
		return ErrTooLarge
	}
	payload, err := readPayload(r.src, int(bh.compLen))
	if err != nil {
		return err
	}
	r.scheduled += int64(bh.rawLen)
	ch := make(chan decResult, 1)
	r.queue = append(r.queue, ch)
	r.sem <- struct{}{}
	level, tick := r.hdr.level, r.cfg.progress
	go func() {
		defer func() { <-r.sem }()
		var data []byte
		var err error
		if bh.kind == kindStored {
			data = payload
		} else {
			data, err = decodeBlock(payload, int(bh.rawLen), level, r.cancelled, tick)
			if err == errCanceled {
				if cerr := r.cfg.ctx.Err(); cerr != nil {
					err = cerr
				} else {
					err = io.ErrClosedPipe
				}
			}
		}
		if err == nil && crc32.ChecksumIEEE(data) != bh.crc {
			err = fmt.Errorf("%w: block checksum mismatch", ErrCorrupt)
		}
		ch <- decResult{data: data, err: err}
	}()
	return nil
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.stop.Load() {
			r.err = io.ErrClosedPipe
			return 0, r.err
		}
		if err := r.cfg.ctx.Err(); err != nil {
			r.err = err
			return 0, err
		}
		for r.schedErr == nil && !r.endSeen && len(r.queue) <= r.cfg.workers {
			r.schedErr = r.schedule()
		}
		if len(r.queue) == 0 {
			if r.schedErr != nil {
				r.err = r.schedErr
			} else {
				r.err = io.EOF
			}
			return 0, r.err
		}
		ch := r.queue[0]
		r.queue = r.queue[1:]
		res := <-ch
		if res.err != nil {
			r.err = res.err
			return 0, res.err
		}
		r.cur = res.data
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

var _ io.Reader = (*Reader)(nil)
