// Command ctxmix compresses and decompresses data with the ctxmix
// context-mixing codec.
//
//	ctxmix c [-level best|fast] [-block MiB] [-j N] [-o OUT] [FILE]
//	ctxmix d [-max-output SIZE] [-j N] [-o OUT] [FILE]
//	ctxmix l [FILE]
//	ctxmix t [-j N] [FILE]
//
// With no FILE (or "-") input is read from standard input. Output is written
// to standard output unless -o is given; a named output file is replaced
// atomically, so a failed run never leaves a truncated file behind.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/morsuning/ctxmix"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage: ctxmix <command> [flags] [FILE]

commands:
  c   compress FILE (or stdin)
  d   decompress FILE (or stdin)
  l   show the structure of a compressed stream (does not decompress)
  t   test a compressed stream: decompress it and verify every checksum

flags (c): -level best|fast  -block MiB  -j N  -o OUT
flags (d): -max-output SIZE  -j N  -o OUT
flags (t): -j N

SIZE accepts a plain number of bytes or a K, M or G suffix (powers of 1024).
-j is the number of blocks processed in parallel; it defaults to the number of
CPUs (at most 4), and each one needs about 100 MiB at the default block size.
`

type usageError string

func (e usageError) Error() string { return string(e) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "c", "compress":
		err = cmdCompress(rest, stdin, stdout, stderr)
	case "d", "decompress":
		err = cmdDecompress(rest, stdin, stdout, stderr)
	case "l", "list":
		err = cmdList(rest, stdin, stdout, stderr)
	case "t", "test":
		err = cmdTest(rest, stdin, stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "ctxmix: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "ctxmix: %v\n", err)
		var ue usageError
		if errors.As(err, &ue) {
			return 2
		}
		return 1
	}
	return 0
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("ctxmix "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse handles flags that may appear before or after the single FILE
// argument and returns that argument ("" for stdin).
func parse(fs *flag.FlagSet, args []string) (string, error) {
	var file string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "", err
			}
			return "", usageError(err.Error())
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if file != "" {
			return "", usageError("too many arguments")
		}
		file, args = args[0], args[1:]
	}
	if file == "-" {
		file = ""
	}
	return file, nil
}

func openIn(file string, stdin io.Reader) (io.Reader, func(), error) {
	if file == "" {
		return bufio.NewReaderSize(stdin, 1<<16), func() {}, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, err
	}
	return bufio.NewReaderSize(f, 1<<16), func() { f.Close() }, nil
}

// openOut returns the destination writer. A named output is written to a
// temporary file in the same directory and renamed on commit.
func openOut(path string, stdout io.Writer) (w io.Writer, commit func() error, abort func(), err error) {
	if path == "" || path == "-" {
		bw := bufio.NewWriterSize(stdout, 1<<16)
		return bw, bw.Flush, func() {}, nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ctxmix-*")
	if err != nil {
		return nil, nil, nil, err
	}
	bw := bufio.NewWriterSize(f, 1<<16)
	commit = func() error {
		if err := bw.Flush(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Chmod(f.Name(), 0o644); err != nil {
			return err
		}
		return os.Rename(f.Name(), path)
	}
	abort = func() { f.Close(); os.Remove(f.Name()) }
	return bw, commit, abort, nil
}

func parseLevel(s string) (ctxmix.Level, error) {
	switch strings.ToLower(s) {
	case "best", "0", "":
		return ctxmix.Best, nil
	case "fast", "1":
		return ctxmix.Fast, nil
	}
	return 0, usageError(fmt.Sprintf("-level must be best or fast, not %q", s))
}

// parseSize parses a byte count with an optional K, M or G suffix.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k', 'K':
			mult, s = 1<<10, s[:n-1]
		case 'm', 'M':
			mult, s = 1<<20, s[:n-1]
		case 'g', 'G':
			mult, s = 1<<30, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 1 || v > (1<<62)/mult {
		return 0, usageError("invalid size")
	}
	return v * mult, nil
}

func cmdCompress(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlags("c", stderr)
	level := fs.String("level", "best", "compression level: best or fast")
	block := fs.Int("block", 0, "block size in MiB (default 16)")
	jobs := fs.Int("j", 0, "number of blocks compressed in parallel (default: number of CPUs, at most 4)")
	out := fs.String("o", "", "output file (default: stdout)")
	file, err := parse(fs, args)
	if err != nil {
		return err
	}
	lv, err := parseLevel(*level)
	if err != nil {
		return err
	}
	if *block < 0 || *jobs < 0 {
		return usageError("-block and -j must not be negative")
	}
	in, closeIn, err := openIn(file, stdin)
	if err != nil {
		return err
	}
	defer closeIn()
	w, commit, abort, err := openOut(*out, stdout)
	if err != nil {
		return err
	}
	opts := []ctxmix.Option{ctxmix.WithLevel(lv), ctxmix.WithConcurrency(workers(*jobs))}
	if *block > 0 {
		opts = append(opts, ctxmix.WithBlockSize(*block<<20))
	}
	cw, err := ctxmix.NewWriter(w, opts...)
	if err != nil {
		abort()
		return usageError(err.Error())
	}
	if _, err := io.Copy(cw, in); err != nil {
		cw.Close()
		abort()
		return err
	}
	if err := cw.Close(); err != nil {
		abort()
		return err
	}
	if err := commit(); err != nil {
		abort()
		return err
	}
	return nil
}

func cmdDecompress(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlags("d", stderr)
	out := fs.String("o", "", "output file (default: stdout)")
	maxOut := fs.String("max-output", "", "refuse streams that decompress to more than this many bytes")
	jobs := fs.Int("j", 0, "number of blocks decompressed in parallel (default: number of CPUs, at most 4)")
	file, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *jobs < 0 {
		return usageError("-j must not be negative")
	}
	opts := []ctxmix.Option{ctxmix.WithConcurrency(workers(*jobs))}
	if *maxOut != "" {
		n, err := parseSize(*maxOut)
		if err != nil {
			return err
		}
		opts = append(opts, ctxmix.WithMaxOutputSize(n))
	}
	in, closeIn, err := openIn(file, stdin)
	if err != nil {
		return err
	}
	defer closeIn()
	w, commit, abort, err := openOut(*out, stdout)
	if err != nil {
		return err
	}
	cr, err := ctxmix.NewReader(in, opts...)
	if err != nil {
		abort()
		return err
	}
	defer cr.Close()
	if _, err := io.Copy(w, cr); err != nil {
		abort()
		return err
	}
	if err := commit(); err != nil {
		abort()
		return err
	}
	return nil
}

func cmdList(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlags("l", stderr)
	file, err := parse(fs, args)
	if err != nil {
		return err
	}
	in, closeIn, err := openIn(file, stdin)
	if err != nil {
		return err
	}
	defer closeIn()
	info, err := ctxmix.Inspect(in)
	if err != nil {
		return err
	}
	level := "best"
	if info.Level == ctxmix.Fast {
		level = "fast"
	}
	ratio := "-"
	if info.RawSize > 0 {
		ratio = fmt.Sprintf("%.2f%%", 100*float64(info.CompressedSize)/float64(info.RawSize))
	}
	fmt.Fprintf(stdout, "level:       %s\n", level)
	fmt.Fprintf(stdout, "block size:  %d bytes\n", info.BlockSize)
	fmt.Fprintf(stdout, "blocks:      %d (%d stored uncompressed)\n", info.Blocks, info.StoredBlocks)
	fmt.Fprintf(stdout, "raw size:    %d bytes\n", info.RawSize)
	fmt.Fprintf(stdout, "stream size: %d bytes (%s)\n", info.CompressedSize, ratio)
	return nil
}

func cmdTest(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlags("t", stderr)
	jobs := fs.Int("j", 0, "number of blocks decompressed in parallel (default: number of CPUs, at most 4)")
	file, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *jobs < 0 {
		return usageError("-j must not be negative")
	}
	in, closeIn, err := openIn(file, stdin)
	if err != nil {
		return err
	}
	defer closeIn()
	cr, err := ctxmix.NewReader(in, ctxmix.WithConcurrency(workers(*jobs)))
	if err != nil {
		return err
	}
	defer cr.Close()
	n, err := io.Copy(io.Discard, cr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OK: %d bytes\n", n)
	return nil
}
