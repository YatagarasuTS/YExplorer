package main

import (
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type Entry struct {
	Path string
	Name string
	Low  string
	Ext  string
	Size int64
	Mod  int64
	Cat  Cat
	Dir  bool
}

type Snapshot struct {
	entries []Entry
	byPath  map[string]int32
	tri     map[uint32][]int32
	counts  [6]int
}

type Index struct {
	mu   sync.RWMutex
	snap *Snapshot

	cmu        sync.RWMutex
	store      *Store
	docEntry   []int32
	vocab      []string
	vocabDirty bool
	vocabAt    time.Time

	cfg *Config

	kickCh   chan struct{}
	runMu    sync.Mutex
	cancel   func()
	stage    atomic.Value
	scanned  atomic.Int64
	cDone    atomic.Int64
	cTotal   atomic.Int64
	cStart   atomic.Int64
	cBase    atomic.Int64
	oDone    atomic.Int64
	oTotal   atomic.Int64
	lastRun  atomic.Int64
	scanMs   atomic.Int64
	firstRun bool
	ocrOn    atomic.Bool
}

func newIndex(cfg *Config) *Index {
	ix := &Index{cfg: cfg, kickCh: make(chan struct{}, 1), store: loadStore()}
	ix.firstRun = len(ix.store.Docs) == 0
	ix.snap = &Snapshot{}
	ix.vocabDirty = true
	ix.stage.Store("scanning")
	return ix
}

func (ix *Index) snapshot() *Snapshot {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.snap
}

// ---------- построение снимка ----------

func gramOf(a, b, c rune) uint32 {
	return (uint32(a)*31+uint32(b))*31 + uint32(c)
}

func gramsOf(s string, buf []rune) ([]uint32, []rune) {
	buf = buf[:0]
	for _, r := range s {
		buf = append(buf, r)
	}
	if len(buf) < 3 {
		return nil, buf
	}
	g := make([]uint32, 0, len(buf)-2)
	for i := 0; i+2 < len(buf); i++ {
		g = append(g, gramOf(buf[i], buf[i+1], buf[i+2]))
	}
	return g, buf
}

func buildSnapshot(entries []Entry) *Snapshot {
	s := &Snapshot{entries: entries, byPath: make(map[string]int32, len(entries)), tri: make(map[uint32][]int32, 1<<16)}
	var buf []rune
	var gs []uint32
	for i := range entries {
		e := &entries[i]
		s.byPath[e.Path] = int32(i)
		s.counts[e.Cat]++
		gs, buf = gramsOf(e.Low, buf)
		for _, g := range gs {
			p := s.tri[g]
			if n := len(p); n == 0 || p[n-1] != int32(i) {
				s.tri[g] = append(p, int32(i))
			}
		}
	}
	return s
}

// ---------- поиск ----------

const (
	FName    = 1
	FContent = 2
	FFuzzy   = 4
)

type Hit struct {
	Idx   int32
	Score float32
	Flags uint8
}

type SearchResult struct {
	Snap *Snapshot
	Hits []Hit
	Toks []string
}

func queryTokens(raw string) []string {
	var out []string
	for _, f := range strings.Fields(norm(raw)) {
		if len(out) >= 6 {
			break
		}
		out = append(out, f)
	}
	return out
}

func isSepByte(c byte) bool {
	switch c {
	case ' ', '.', '_', '-', '(', ')', '[', ']', '+', ',', '\'':
		return true
	}
	return false
}

func tokenScore(low, t string) float32 {
	i := strings.Index(low, t)
	if i < 0 {
		return 0
	}
	var s float32
	switch {
	case len(low) == len(t):
		s = 1000
	case i == 0 && low[len(t)] == '.':
		s = 950
	case i == 0:
		s = 800
	case isSepByte(low[i-1]):
		s = 600
	default:
		s = 300
	}
	return s + 100*float32(len(t))/float32(len(low))
}

func parallelFor(n int, fn func(lo, hi int)) {
	w := runtime.GOMAXPROCS(0)
	if n < 30000 || w < 2 {
		fn(0, n)
		return
	}
	var wg sync.WaitGroup
	step := (n + w - 1) / w
	for lo := 0; lo < n; lo += step {
		hi := lo + step
		if hi > n {
			hi = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			fn(lo, hi)
		}(lo, hi)
	}
	wg.Wait()
}

func (ix *Index) Search(raw string, content, typos bool) SearchResult {
	snap := ix.snapshot()
	entries := snap.entries
	n := len(entries)
	toks := queryTokens(raw)
	res := SearchResult{Snap: snap, Toks: toks}
	if len(toks) == 0 {
		hits := make([]Hit, n)
		for i := range entries {
			hits[i] = Hit{Idx: int32(i), Score: float32(entries[i].Mod), Flags: FName}
		}
		res.Hits = hits
		return res
	}
	nt := len(toks)
	full := uint8(1<<nt - 1)
	nameMask := make([]uint8, n)
	nameScore := make([]float32, n)
	parallelFor(n, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			low := entries[i].Low
			var m uint8
			var sc float32
			for ti, t := range toks {
				if s := tokenScore(low, t); s > 0 {
					m |= 1 << ti
					sc += s
				}
			}
			nameMask[i] = m
			nameScore[i] = sc
		}
	})
	// совпадение с именем родительской папки для токенов, не найденных в имени
	if nt > 1 {
		for i := 0; i < n; i++ {
			m := nameMask[i]
			if m == 0 || m == full {
				continue
			}
			dir := norm(parentOf(entries[i].Path))
			for ti, t := range toks {
				if m&(1<<ti) == 0 && strings.Contains(dir, t) {
					nameMask[i] |= 1 << ti
					nameScore[i] += 150
				}
			}
		}
	}
	var cmask []uint8
	if content {
		cmask = ix.contentMask(toks, n, false, nil)
	}
	fz := make([]uint8, n)
	tokHit := func(ti int) int {
		c := 0
		bit := uint8(1 << ti)
		for i := 0; i < n; i++ {
			if nameMask[i]&bit != 0 || (cmask != nil && cmask[i]&bit != 0) {
				c++
			}
		}
		return c
	}
	if typos {
		var need []int
		for ti, t := range toks {
			if utf8.RuneCountInString(t) >= 3 && tokHit(ti) == 0 {
				need = append(need, ti)
			}
		}
		for _, ti := range need {
			bit := uint8(1 << ti)
			found := ix.fuzzyName(snap, toks[ti])
			for idx, sc := range found {
				fz[idx] |= bit
				nameScore[idx] += sc
			}
			if content {
				if cm := ix.contentMask(toks, n, true, []int{ti}); cm != nil {
					for i := 0; i < n; i++ {
						if cm[i]&bit != 0 {
							if cmask == nil {
								cmask = make([]uint8, n)
							}
							cmask[i] |= bit
						}
					}
				}
			}
		}
	}
	now := time.Now().Unix()
	hits := make([]Hit, 0, 256)
	for i := 0; i < n; i++ {
		nm := nameMask[i]
		var cm uint8
		if cmask != nil {
			cm = cmask[i]
		}
		if nm|cm|fz[i] != full {
			continue
		}
		e := &entries[i]
		var fl uint8
		score := nameScore[i]
		onlyContent := cm &^ nm &^ fz[i]
		for b := onlyContent; b != 0; b &= b - 1 {
			score += 180
		}
		if nm != 0 {
			fl |= FName
		}
		if cm != 0 {
			fl |= FContent
			if nm != 0 {
				score += 60
			}
		}
		if fz[i] != 0 {
			fl |= FFuzzy
		}
		score /= float32(nt)
		if e.Mod > 0 {
			age := float64(now-e.Mod) / 86400
			if age < 0 {
				age = 0
			}
			score += float32(40 * math.Exp(-age/60))
		}
		if e.Dir {
			score += 10
		}
		hits = append(hits, Hit{Idx: int32(i), Score: score, Flags: fl})
	}
	res.Hits = hits
	return res
}

func parentOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '\\' || p[i] == '/' {
			return p[:i]
		}
	}
	return ""
}

// fuzzyName — файлы, в имени которых есть слово, похожее на t (с опечатками).
func (ix *Index) fuzzyName(snap *Snapshot, t string) map[int32]float32 {
	out := map[int32]float32{}
	tr := []rune(t)
	d := maxTypos(len(tr))
	if d == 0 || snap.tri == nil {
		return out
	}
	gs, _ := gramsOf(t, nil)
	if len(gs) == 0 {
		return out
	}
	n := len(snap.entries)
	cnt := make([]uint8, n)
	var touched []int32
	for _, g := range gs {
		for _, idx := range snap.tri[g] {
			if cnt[idx] == 0 {
				touched = append(touched, idx)
			}
			if cnt[idx] < 255 {
				cnt[idx]++
			}
		}
	}
	thr := len(gs) - 3*d
	if thr < 1 {
		thr = 1
	}
	for _, idx := range touched {
		if int(cnt[idx]) < thr {
			continue
		}
		best := d + 1
		for _, w := range splitWords(snap.entries[idx].Low) {
			wr := []rune(w)
			if dd := osa(tr, wr, d); dd < best {
				best = dd
			}
			if len(wr) > len(tr) {
				for _, L := range []int{len(tr) - 1, len(tr), len(tr) + 1} {
					if L > 1 && L <= len(wr) {
						if dd := osa(tr, wr[:L], d); dd < best {
							best = dd
						}
					}
				}
			}
		}
		if best <= d {
			out[idx] = float32(230 - 50*best)
		}
	}
	return out
}

// ---------- поиск по содержимому ----------

func (ix *Index) refreshVocab() {
	ix.cmu.RLock()
	need := ix.vocabDirty && (ix.vocab == nil || time.Since(ix.vocabAt) > 2*time.Second)
	ix.cmu.RUnlock()
	if !need {
		return
	}
	ix.cmu.Lock()
	if ix.vocabDirty {
		ix.vocab = sortedVocab(ix.store.Post)
		ix.vocabDirty = false
		ix.vocabAt = time.Now()
	}
	ix.cmu.Unlock()
}

type bitset []uint64

func newBitset(n int) bitset { return make(bitset, (n+63)/64) }
func (b bitset) set(i int)   { b[i>>6] |= 1 << (uint(i) & 63) }

// expandWord возвращает слова словаря, подходящие под sub.
func (ix *Index) expandWord(sub string, fuzzy bool) []string {
	vocab := ix.vocab
	var out []string
	const capWords = 400
	prefix := func(p string) {
		for k := sort.SearchStrings(vocab, p); k < len(vocab) && strings.HasPrefix(vocab[k], p) && len(out) < capWords; k++ {
			out = append(out, vocab[k])
		}
	}
	if !fuzzy {
		prefix(sub)
		if len(out) == 0 {
			r := []rune(sub)
			cut := 0
			switch {
			case len(r) >= 7:
				cut = 2
			case len(r) >= 5:
				cut = 1
			}
			if cut > 0 {
				prefix(string(r[:len(r)-cut]))
			}
		}
		return out
	}
	sr := []rune(sub)
	d := maxTypos(len(sr))
	if d == 0 {
		return nil
	}
	for _, w := range vocab {
		if lw := len(w); lw < len(sub)-2*d || lw > len(sub)+3*d+2 {
			continue
		}
		wr := []rune(w)
		if osa(sr, wr, d) <= d {
			out = append(out, w)
			if len(out) >= 100 {
				break
			}
		}
	}
	return out
}

// contentMask: для каждого файла — маска токенов, найденных в содержимом.
func (ix *Index) contentMask(toks []string, n int, fuzzy bool, only []int) []uint8 {
	ix.refreshVocab()
	ix.cmu.RLock()
	defer ix.cmu.RUnlock()
	if len(ix.store.Post) == 0 {
		return nil
	}
	var mask []uint8
	for ti, t := range toks {
		if only != nil {
			ok := false
			for _, o := range only {
				if o == ti {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		var subs []string
		tokenize(t, func(s string) { subs = append(subs, s) })
		if len(subs) == 0 {
			continue
		}
		var acc bitset
		for si, sub := range subs {
			words := ix.expandWord(sub, fuzzy)
			b := newBitset(n)
			for _, w := range words {
				for _, id := range ix.store.Post[w] {
					if int(id) < len(ix.docEntry) {
						if e := ix.docEntry[id]; e >= 0 && int(e) < n {
							b.set(int(e))
						}
					}
				}
			}
			if si == 0 {
				acc = b
			} else {
				for k := range acc {
					acc[k] &= b[k]
				}
			}
		}
		if mask == nil {
			mask = make([]uint8, n)
		}
		for w, bits := range acc {
			for ; bits != 0; bits &= bits - 1 {
				tz := trailingZeros64(bits)
				mask[w*64+tz] |= 1 << ti
			}
		}
	}
	return mask
}

func trailingZeros64(x uint64) int {
	n := 0
	for x&1 == 0 {
		x >>= 1
		n++
	}
	return n
}
