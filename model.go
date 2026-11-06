package ctxmix

// model.go - the context-mixing model, in the PAQ / lpaq / ZPAQ family:
//
//   - bit-history state machine (state table from libzpaq, public domain)
//     plus adaptive StateMaps that turn a state into a probability
//   - several order-N / word / line-aware contexts hashed into 16-byte buckets
//     (1 check byte + 15 bit-history states per nibble)
//   - two match models (minimum lengths 6 and 16) and the predicted byte of
//     the longer-matching one used as one more context
//   - a two-layer mixing network (up to 4 gated mixers + 1 final mixer)
//   - two APM/SSE refinement stages and a 32-bit arithmetic coder with
//     16-bit probabilities
//
// IMPORTANT: encoder and decoder must stay bit-identical on every CPU
// architecture, so the model uses integer arithmetic only. All tables are
// either embedded constants (tables.go) or generated with integer code in
// init. Any change to this file that alters the model's output changes the
// compressed format and must bump the frame version.

const (
	cmMaxNC  = 16 // maximum number of hashed contexts
	cmMaxIn  = 2*cmMaxNC + 12
	cmNMix   = 4 // number of first-layer mixers
	cmApmLen = 25
)

const cmSkipErr int32 = 48

var (
	stretchTab [32768]int16
	dtTab      [1024]int32
)

func init() {
	// stretch is the monotone inverse of squash (integer-only, deterministic)
	pi := 0
	for x := -700; x <= 700; x++ {
		v := int(squashTab[x+2048])
		for i := pi; i <= v; i++ {
			stretchTab[i] = int16(x)
		}
		pi = v + 1
	}
	for i := pi; i < 32768; i++ {
		stretchTab[i] = 700
	}
	for i := range dtTab {
		dtTab[i] = int32(16384 / (2*i + 3))
	}
}

func squash(x int32) int32 {
	if x > 2047 {
		x = 2047
	} else if x < -2047 {
		x = -2047
	}
	return int32(squashTab[x+2048])
}

func hash32(a, b uint32) uint32 {
	h := a*0x9E3779B1 + b*0x85EBCA6B + 0x165667B1
	h ^= h >> 15
	h *= 0x2C1B3C6D
	h ^= h >> 12
	h *= 0x297A2D39
	h ^= h >> 15
	return h
}

// ---------------------------------------------------------------- StateMap

// An entry is p(22 bits)<<10 | count(10 bits).
func smUpdate(t *uint32, y int, limit uint32) {
	v := *t
	n := v & 1023
	p := int64(v >> 10)
	d := ((int64(y) << 22) - p) * int64(dtTab[n]) >> 13
	p += d
	if n < limit {
		n++
	}
	*t = uint32(p)<<10 | n
}

func smInitStates(t *[256]uint32) {
	for s := 0; s < 256; s++ {
		n0 := uint64(stateTab[s*4+2])
		n1 := uint64(stateTab[s*4+3])
		p := ((2*n1 + 1) << 31) / (n0 + n1 + 1)
		if p > 0xfffffc00 {
			p = 0xfffffc00
		}
		t[s] = uint32(p) &^ 1023
	}
}

// ---------------------------------------------------------------- APM

type apm struct {
	t   []uint16
	idx int
}

func newAPM(n int) *apm {
	a := &apm{t: make([]uint16, n*cmApmLen)}
	for i := 0; i < n; i++ {
		for j := 0; j < cmApmLen; j++ {
			v := squash(int32((j-12)*64)) * 2
			if v > 65535 {
				v = 65535
			}
			a.t[i*cmApmLen+j] = uint16(v)
		}
	}
	return a
}

// pp refines a prediction: st is in the stretch domain, the result is a 16-bit probability.
func (a *apm) pp(st int32, cx int) int32 {
	if st > 767 {
		st = 767
	} else if st < -767 {
		st = -767
	}
	pos := st + 768
	lo := int(pos >> 6)
	w := pos & 63
	base := cx*cmApmLen + lo
	a.idx = base + int(w>>5)
	return (int32(a.t[base])*(64-w) + int32(a.t[base+1])*w) >> 6
}

func (a *apm) update(y int, rate uint) {
	g := int32(y<<16) + int32(y<<rate) - int32(y) - int32(y)
	v := int32(a.t[a.idx])
	a.t[a.idx] = uint16(v + (g-v)>>rate)
}

// ---------------------------------------------------------------- Mixer

type mixer struct {
	n    int
	w    []int32
	base int
	pr   int32 // output probability, 15 bits
	dot  int32 // output in the stretch domain
	rate int32
}

func newMixer(n, sets int, init int32, rate int32) *mixer {
	m := &mixer{n: n, w: make([]int32, n*sets), rate: rate}
	for i := range m.w {
		m.w[i] = init
	}
	return m
}

func (m *mixer) mix(x []int32, sel int) int32 {
	m.base = sel * m.n
	n := m.n
	w := m.w[m.base : m.base+n : m.base+n]
	x = x[:n:n]
	var s0, s1 int64
	i := 0
	for ; i+4 <= n; i += 4 {
		xs := x[i : i+4 : i+4]
		ws := w[i : i+4 : i+4]
		s0 += int64(xs[0])*int64(ws[0]) + int64(xs[1])*int64(ws[1])
		s1 += int64(xs[2])*int64(ws[2]) + int64(xs[3])*int64(ws[3])
	}
	for ; i < n; i++ {
		s0 += int64(x[i]) * int64(w[i])
	}
	d := int32((s0 + s1) >> 16)
	if d > 2047 {
		d = 2047
	} else if d < -2047 {
		d = -2047
	}
	m.dot = d
	m.pr = squash(d)
	return d
}

func (m *mixer) update(x []int32, y int) {
	e := (int32(y) << 15) - m.pr
	if e > -cmSkipErr && e < cmSkipErr {
		return
	}
	err := int64(e * m.rate >> 4)
	n := m.n
	w := m.w[m.base : m.base+n : m.base+n]
	x = x[:n:n]
	i := 0
	for ; i+4 <= n; i += 4 {
		xs := x[i : i+4 : i+4]
		ws := w[i : i+4 : i+4]
		ws[0] += int32((int64(xs[0])*err + 4096) >> 13)
		ws[1] += int32((int64(xs[1])*err + 4096) >> 13)
		ws[2] += int32((int64(xs[2])*err + 4096) >> 13)
		ws[3] += int32((int64(xs[3])*err + 4096) >> 13)
	}
	for ; i < n; i++ {
		w[i] += int32((int64(x[i])*err + 4096) >> 13)
	}
}

// ---------------------------------------------------------------- match model

type matchModel struct {
	minLen int
	tab    []int32
	mask   uint32
	ptr    int
	length int
	sm     [128]uint32
	smi    int
	active bool
}

func newMatchModel(minLen int, bits uint) *matchModel {
	m := &matchModel{minLen: minLen, tab: make([]int32, 1<<bits), mask: 1<<bits - 1}
	for i := range m.sm {
		m.sm[i] = 1 << 31
	}
	return m
}

func lenQ(l int) int {
	if l < 32 {
		return l
	}
	q := 32 + (l-32)>>4
	if q > 63 {
		q = 63
	}
	return q
}

// byteUpdate is called after each byte; buf[:pos] is known.
func (m *matchModel) byteUpdate(buf []byte, pos int, hv uint32) {
	if m.length > 0 && buf[m.ptr] == buf[pos-1] {
		if m.length < 65535 {
			m.length++
		}
		m.ptr++
	} else {
		m.length = 0
	}
	if pos < m.minLen {
		return
	}
	hi := hv & m.mask
	if m.length == 0 {
		cand := int(m.tab[hi])
		if cand > 0 {
			l := 0
			for l < 4096 && cand-1-l >= 0 && buf[cand-1-l] == buf[pos-1-l] {
				l++
			}
			if l >= m.minLen {
				m.length = l
				m.ptr = cand
			}
		}
	}
	m.tab[hi] = int32(pos)
}

// predict appends two mixer inputs to x and returns the new k.
func (m *matchModel) predict(x []int32, k int, buf []byte, c0, bpos uint32) int {
	m.active = false
	if m.length > 0 {
		eb := uint32(buf[m.ptr]) | 256
		if eb>>(8-bpos) == c0 {
			bit := int((eb >> (7 - bpos)) & 1)
			m.smi = lenQ(m.length)*2 + bit
			m.active = true
			p := int32(m.sm[m.smi] >> 17)
			x[k] = int32(stretchTab[p])
			l := int32(m.length)
			if l > 32 {
				l = 32
			}
			if bit == 1 {
				x[k+1] = l * 24
			} else {
				x[k+1] = -l * 24
			}
			return k + 2
		}
		m.length = 0
	}
	x[k] = 0
	x[k+1] = 0
	return k + 2
}

func (m *matchModel) update(y int) {
	if m.active {
		smUpdate(&m.sm[m.smi], y, 255)
	}
}

// ---------------------------------------------------------------- main model

type cmConfig struct {
	ids   []uint8 // which contexts to use
	nmix  int     // number of first-layer mixers (1..4)
	htLog uint
	r1    int32  // learning rate of the first-layer mixers
	r2    int32  // learning rate of the final mixer
	lim   uint32 // StateMap count limit

	cancel func() bool // optional; polled every 64 KiB, true aborts
}

var cmFastIDs = []uint8{1, 2, 3, 12, 7}
var cmDefaultIDs = []uint8{1, 2, 3, 12, 7, 0, 4, 6, 8, 11, 13}

type cm struct {
	buf   []byte
	pos   int
	write bool

	c0, bpos, c4, c8 uint32
	word, pword      uint32
	lineStart        int
	prevLine         int
	indent           uint32
	inIndent         bool

	nc    int
	nmix  int
	lim   uint32
	ids   [cmMaxNC]uint8
	ht    []byte
	hmask uint32
	h     [cmMaxNC]uint32
	bk    [cmMaxNC]uint32
	sidx  [cmMaxNC]uint32
	sm    [cmMaxNC][256]uint32
	o0    [256]uint32
	mm    [2]*matchModel
	mh    [2]uint32

	x  [cmMaxIn]int32
	nx int
	l1 [cmNMix]*mixer
	l2 *mixer
	x2 [cmNMix + 1]int32

	a1, a2   *apm
	apmCtx2  int
	prFinal  uint32
	mixSel   [cmNMix]int
	hit      int
	apmC1    int
	sel2     int
	stateMix int
}

func newCM(buf []byte, n int, write bool, cfg cmConfig) *cm {
	m := &cm{buf: buf, write: write, nc: len(cfg.ids), nmix: cfg.nmix, lim: cfg.lim}
	copy(m.ids[:], cfg.ids)
	m.ht = make([]byte, 16<<cfg.htLog)
	m.hmask = 1<<cfg.htLog - 1
	for i := 0; i < m.nc; i++ {
		smInitStates(&m.sm[i])
	}
	for i := range m.o0 {
		m.o0[i] = 1 << 31
	}
	mb := matchBits(n)
	m.mm[0] = newMatchModel(6, mb)
	m.mm[1] = newMatchModel(16, mb)
	nin := 2*m.nc + 1 + 4 + 1
	m.l1[0] = newMixer(nin, 256, 9000, cfg.r1)
	m.l1[1] = newMixer(nin, 64*8, 9000, cfg.r1)
	m.l1[2] = newMixer(nin, 256, 9000, cfg.r1)
	m.l1[3] = newMixer(nin, 17*8, 9000, cfg.r1)
	m.l2 = newMixer(m.nmix+1, 256, int32(65536/m.nmix), cfg.r2)
	// the bias input starts with weight 0 so it does not skew early predictions
	for _, mx := range append(append([]*mixer{}, m.l1[:m.nmix]...), m.l2) {
		for s := 0; s < len(mx.w)/mx.n; s++ {
			mx.w[s*mx.n+mx.n-1] = 0
		}
	}
	m.a1 = newAPM(1 << 16)
	m.a2 = newAPM(1 << 14)
	m.c0 = 1
	m.lineStart = 0
	m.prevLine = 0
	m.inIndent = true
	m.startByte()
	return m
}

func isWordChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c >= 0x80
}

func (m *cm) ctxHash(id uint8) uint32 {
	c4 := m.c4
	switch id {
	case 0:
		return hash32(c4&0xff, 1)
	case 1:
		return hash32(c4&0xffff, 2)
	case 2:
		return hash32(c4&0xffffff, 3)
	case 3:
		return hash32(c4, 4)
	case 4:
		return hash32(c4, 5+(m.c8&0xff)<<8)
	case 6:
		return hash32(c4, hash32(m.c8, 7))
	case 7:
		if m.word != 0 {
			return hash32(m.word, 0x77)
		}
		return hash32(m.pword, c4&0xff|0x100)
	case 8:
		return hash32(m.word, m.pword+0x88)
	case 11:
		col := m.pos - m.lineStart
		var above uint32
		if m.prevLine+col < m.lineStart {
			above = uint32(m.buf[m.prevLine+col])
		}
		return hash32(above|(c4&0xff)<<8, 11)
	case 12:
		mm := m.mm[0]
		if mm.length > 0 {
			return hash32(uint32(m.buf[mm.ptr])|uint32(lenQ(mm.length)>>2)<<8, 12)
		}
		return hash32(0, 12)
	case 13:
		return hash32(m.indent, (c4&0xffff)|13<<16)
	default:
		return hash32(0, 0xdead)
	}
}

func (m *cm) find(h uint32) uint32 {
	chk := byte(h >> 24)
	i := (h & m.hmask) << 4
	ht := m.ht
	if ht[i] == chk {
		return i
	}
	j := i ^ 16
	if ht[j] == chk {
		return j
	}
	k := i ^ 32
	if ht[k] == chk {
		return k
	}
	best, bp := i, ht[i+1]
	if ht[j+1] < bp {
		best, bp = j, ht[j+1]
	}
	if ht[k+1] < bp {
		best = k
	}
	b := ht[best : best+16 : best+16]
	for t := range b {
		b[t] = 0
	}
	b[0] = chk
	return best
}

// startByte computes the context hashes and finds the first-nibble buckets.
func (m *cm) startByte() {
	for i := 0; i < m.nc; i++ {
		m.h[i] = m.ctxHash(m.ids[i])
		m.bk[i] = m.find(m.h[i])
	}
}

func (m *cm) startNibble() {
	for i := 0; i < m.nc; i++ {
		m.bk[i] = m.find(hash32(m.h[i], m.c0+0x3779B1))
	}
}

func (m *cm) byteDone() {
	c := byte(m.c0)
	if m.write {
		m.buf[m.pos] = c
	}
	m.pos++
	m.c8 = m.c8<<8 | m.c4>>24
	m.c4 = m.c4<<8 | uint32(c)
	// word hash
	if isWordChar(c) {
		lc := uint32(c)
		if c >= 'A' && c <= 'Z' {
			lc += 32
		}
		m.word = (m.word + lc + 1) * 0x2F0B4C27
	} else if m.word != 0 {
		m.pword = m.word
		m.word = 0
	}
	// line / indentation tracking
	if c == '\n' {
		m.prevLine = m.lineStart
		m.lineStart = m.pos
		m.indent = 0
		m.inIndent = true
	} else if m.inIndent && (c == ' ' || c == '\t') {
		if m.indent < 63 {
			m.indent++
		}
	} else {
		m.inIndent = false
	}
	// match models
	m.mm[0].byteUpdate(m.buf, m.pos, hash32(m.c4, m.c8&0xffff))
	if m.pos >= 16 {
		var hv uint32
		for _, b := range m.buf[m.pos-16 : m.pos] {
			hv = (hv + uint32(b) + 1) * 0x2F0B4C27
		}
		m.mm[1].byteUpdate(m.buf, m.pos, hv^hv>>15)
	}
	m.c0 = 1
	m.bpos = 0
	if m.pos < len(m.buf) {
		m.startByte()
	}
}

func (m *cm) nibIdx() uint32 {
	if m.bpos < 4 {
		return m.c0
	}
	s := m.bpos - 4
	return 1<<s | m.c0&(1<<s-1)
}

// predict returns the 16-bit probability that the next bit is 1.
func (m *cm) predict() uint32 {
	x := m.x[:]
	k := 0
	idx := m.nibIdx()
	hit := 0
	for i := 0; i < m.nc; i++ {
		si := m.bk[i] + idx
		m.sidx[i] = si
		s := m.ht[si]
		if s != 0 && i < 8 {
			hit++
		}
		p := int32(m.sm[i][s] >> 17)
		x[k] = int32(stretchTab[p])
		x[k+1] = (p - 16384) >> 5
		k += 2
	}
	x[k] = int32(stretchTab[m.o0[m.c0]>>17])
	k++
	k = m.mm[0].predict(x, k, m.buf, m.c0, m.bpos)
	k = m.mm[1].predict(x, k, m.buf, m.c0, m.bpos)
	x[k] = 256
	k++
	m.nx = k

	c1 := int(m.c4 & 0xff)
	bp := int(m.bpos)
	ml := 0
	if m.mm[0].active {
		ml = lenQ(m.mm[0].length)
		if ml > 63 {
			ml = 63
		}
	}
	m.mixSel[0] = int(m.c0)
	m.mixSel[1] = ml*8 + bp
	m.mixSel[2] = c1
	m.mixSel[3] = hit*8 + bp
	for j := 0; j < m.nmix; j++ {
		m.x2[j] = m.l1[j].mix(x, m.mixSel[j])
	}
	m.x2[m.nmix] = 256
	d := m.l2.mix(m.x2[:m.nmix+1], int(m.c0))
	pr := m.l2.pr // 15 bits
	pm := int32(pr) * 2

	a1 := m.a1.pp(d, int(m.c0)|c1<<8)
	h2 := hash32(m.c4&0xffffff, 0x51ED27)
	a2 := m.a2.pp(d, int((uint32(m.c0)^h2)&0x3fff))
	p := (pm + a1 + 2*a2 + 2) >> 2
	if p < 8 {
		p = 8
	} else if p > 65528 {
		p = 65528
	}
	return uint32(p)
}

func (m *cm) update(y int) {
	for i := 0; i < m.nc; i++ {
		si := m.sidx[i]
		s := m.ht[si]
		smUpdate(&m.sm[i][s], y, m.lim)
		m.ht[si] = stateTab[int(s)*4+y]
	}
	smUpdate(&m.o0[m.c0], y, 255)
	m.mm[0].update(y)
	m.mm[1].update(y)
	for j := 0; j < m.nmix; j++ {
		m.l1[j].update(m.x[:], y)
	}
	m.l2.update(m.x2[:m.nmix+1], y)
	m.a1.update(y, 7)
	m.a2.update(y, 7)
	m.c0 = m.c0<<1 | uint32(y)
	m.bpos++
	if m.bpos == 8 {
		m.c0 &= 255
		m.byteDone()
	} else if m.bpos == 4 {
		m.startNibble()
	}
}

// ---------------------------------------------------------------- arithmetic coder

type cmEnc struct {
	x1, x2 uint32
	out    []byte
}

func (e *cmEnc) encode(y int, p uint32) {
	r := e.x2 - e.x1
	xmid := e.x1 + (r>>16)*p + ((r&0xffff)*p)>>16
	if y != 0 {
		e.x2 = xmid
	} else {
		e.x1 = xmid + 1
	}
	for (e.x1^e.x2)&0xff000000 == 0 {
		e.out = append(e.out, byte(e.x2>>24))
		e.x1 <<= 8
		e.x2 = e.x2<<8 | 255
	}
}

func (e *cmEnc) flush() {
	e.out = append(e.out, byte(e.x1>>24), byte(e.x1>>16), byte(e.x1>>8), byte(e.x1))
}
