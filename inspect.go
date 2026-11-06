package ctxmix

import "io"

// Info describes the framing of a stream, as reported by Inspect.
type Info struct {
	// Level is the compression level recorded in the stream header.
	Level Level
	// BlockSize is the block size recorded in the stream header.
	BlockSize int
	// Blocks is the number of data blocks, and StoredBlocks how many of them are
	// stored uncompressed because compression did not make them smaller.
	Blocks       int
	StoredBlocks int
	// RawSize is the total decompressed size and CompressedSize the total size
	// of the stream in bytes, including all headers and the end marker.
	RawSize        int64
	CompressedSize int64
}

// countingByteReader counts the bytes consumed from the underlying reader.
type countingByteReader struct {
	r byteReader
	n int64
}

func (c *countingByteReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingByteReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// Inspect reads the stream in r from its header to its end marker and reports
// its structure, without decompressing any data. Like a Reader it consumes
// exactly the bytes of the stream. Because nothing is decompressed, the block
// checksums are not verified: use a Reader (or DecodeAll) to test integrity.
// Malformed framing yields an error wrapping ErrCorrupt (or
// io.ErrUnexpectedEOF for a truncated stream).
func Inspect(r io.Reader) (Info, error) {
	var info Info
	cr := &countingByteReader{r: asByteReader(r)}
	hdr, err := readStreamHeader(cr, MaxBlockSize)
	if err != nil {
		return info, err
	}
	info.Level, info.BlockSize = hdr.level, hdr.blockSize
	for {
		bh, err := readBlockHeader(cr, hdr, info.RawSize)
		if err != nil {
			return info, err
		}
		if bh.kind == kindEnd {
			info.CompressedSize = cr.n
			return info, nil
		}
		// Skip the payload without buffering it.
		n, err := io.CopyN(io.Discard, cr, int64(bh.compLen))
		if err != nil {
			if err == io.EOF && n < int64(bh.compLen) {
				err = io.ErrUnexpectedEOF
			}
			return info, err
		}
		info.Blocks++
		if bh.kind == kindStored {
			info.StoredBlocks++
		}
		info.RawSize += int64(bh.rawLen)
	}
}
