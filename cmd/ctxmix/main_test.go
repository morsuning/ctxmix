package main

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func exec(t *testing.T, stdin []byte, args ...string) (stdout, stderr []byte, code int) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, bytes.NewReader(stdin), &o, &e)
	return o.Bytes(), e.Bytes(), code
}

func sample() []byte {
	r := rand.New(rand.NewSource(7))
	var b bytes.Buffer
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	for b.Len() < 40000 {
		b.WriteString(words[r.Intn(len(words))])
		b.WriteByte(' ')
	}
	return b.Bytes()
}

func TestStdinStdoutRoundTrip(t *testing.T) {
	data := sample()
	for _, level := range []string{"best", "fast", "0", "1"} {
		comp, errOut, code := exec(t, data, "c", "-level", level, "-j", "2")
		if code != 0 {
			t.Fatalf("level %s: code %d: %s", level, code, errOut)
		}
		if len(comp) >= len(data)/3 {
			t.Errorf("level %s: %d bytes compressed to %d", level, len(data), len(comp))
		}
		got, errOut, code := exec(t, comp, "d")
		if code != 0 || !bytes.Equal(got, data) {
			t.Fatalf("level %s: decompress code %d: %s", level, code, errOut)
		}
	}
}

func TestEmptyInput(t *testing.T) {
	comp, e, code := exec(t, nil, "c")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	got, e, code := exec(t, comp, "d")
	if code != 0 || len(got) != 0 {
		t.Fatalf("%d %s %q", code, e, got)
	}
}

func TestFilesListAndTest(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "notes.txt")
	data := sample()
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	arc := filepath.Join(dir, "notes.cmx")
	if _, e, code := exec(t, nil, "c", in, "-o", arc, "-block", "1", "-level", "fast"); code != 0 {
		t.Fatalf("compress: %d %s", code, e)
	}
	if st, err := os.Stat(arc); err != nil {
		t.Fatalf("archive stat: %v", err)
	} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 {
		t.Fatalf("archive mode %v, want 0644", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".ctxmix-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	out, _, code := exec(t, nil, "l", arc)
	for _, want := range []string{"level:       fast", "block size:  1048576 bytes", "blocks:      1 ", "raw size:    40"} {
		if code != 0 || !strings.Contains(string(out), want) {
			t.Fatalf("list output lacks %q (code %d):\n%s", want, code, out)
		}
	}
	out, _, code = exec(t, nil, "t", arc)
	if code != 0 || !strings.HasPrefix(string(out), "OK: 40") {
		t.Fatalf("test output (code %d): %s", code, out)
	}
	back := filepath.Join(dir, "back.txt")
	if _, e, code := exec(t, nil, "d", "-o", back, arc); code != 0 {
		t.Fatalf("decompress: %d %s", code, e)
	}
	got, _ := os.ReadFile(back)
	if !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}
}

func TestFlagsAfterFile(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "x")
	os.WriteFile(in, sample(), 0o600)
	if _, e, code := exec(t, nil, "c", in, "-level", "fast", "-o", filepath.Join(dir, "x.cmx")); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
}

func TestMaxOutput(t *testing.T) {
	data := sample()
	comp, _, _ := exec(t, data, "c", "-level", "fast")
	if _, e, code := exec(t, comp, "d", "-max-output", "1K"); code != 1 || !strings.Contains(string(e), "exceeds") {
		t.Fatalf("code %d: %s", code, e)
	}
	if got, e, code := exec(t, comp, "d", "-max-output", "1M"); code != 0 || !bytes.Equal(got, data) {
		t.Fatalf("code %d: %s", code, e)
	}
	for _, bad := range []string{"0", "-5", "abc", "12X", "9999999999999G"} {
		if _, _, code := exec(t, comp, "d", "-max-output", bad); code != 2 {
			t.Errorf("-max-output %q: code %d, want 2", bad, code)
		}
	}
}

func TestFailuresLeaveNoOutput(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.cmx")
	os.WriteFile(bad, []byte("this is not a ctxmix stream"), 0o600)
	dst := filepath.Join(dir, "out.bin")
	_, e, code := exec(t, nil, "d", "-o", dst, bad)
	if code != 1 || len(e) == 0 {
		t.Fatalf("code %d stderr %q", code, e)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("output should not exist: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".ctxmix-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	for _, cmd := range []string{"t", "l"} {
		if _, _, code := exec(t, nil, cmd, bad); code != 1 {
			t.Fatalf("%s of a bad stream: code %d", cmd, code)
		}
	}

	// A corrupted payload is detected and does not replace an existing file.
	comp, _, _ := exec(t, sample(), "c", "-level", "fast")
	comp[len(comp)/2] ^= 0x40
	broken := filepath.Join(dir, "broken.cmx")
	os.WriteFile(broken, comp, 0o600)
	os.WriteFile(dst, []byte("precious"), 0o600)
	if _, _, code := exec(t, nil, "d", "-o", dst, broken); code != 1 {
		t.Fatalf("corrupt stream: code %d", code)
	}
	if got, _ := os.ReadFile(dst); string(got) != "precious" {
		t.Fatalf("existing output was modified: %q", got)
	}
	if _, _, code := exec(t, nil, "t", broken); code != 1 {
		t.Fatalf("test of a corrupt stream: code %d", code)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"c", "-level", "turbo"},
		{"c", "-block", "-1"},
		{"c", "-block", "9999"},
		{"c", "-j", "-2"},
		{"d", "-j", "-2"},
		{"c", "a", "b"},
		{"c", "-nonsense"},
	} {
		if _, _, code := exec(t, sample(), args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	if out, _, code := exec(t, nil, "help"); code != 0 || !strings.Contains(string(out), "usage:") {
		t.Errorf("help: code %d", code)
	}
	if _, _, code := exec(t, nil, "c", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Errorf("missing input: code %d, want 1", code)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "10": 10, "2k": 2048, "2K": 2048, "3m": 3 << 20, "1G": 1 << 30} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%q: %d, %v", in, got, err)
		}
	}
}
