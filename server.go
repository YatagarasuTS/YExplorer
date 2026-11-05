package main

import (
	"bytes"
	"container/heap"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type App struct {
	exe      string
	ix       *Index
	token    string
	port     int
	resident atomic.Bool
	started  time.Time
	clients  atomic.Int64
	seen     atomic.Bool
	lastGone atomic.Int64
	snipMu   sync.Mutex
	snips    map[string]string
	quit     chan struct{}
}

func (a *App) config() *Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return a.ix.cfg.clone()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (a *App) hostOK(r *http.Request) bool {
	return r.Host == fmt.Sprintf("127.0.0.1:%d", a.port) || r.Host == fmt.Sprintf("localhost:%d", a.port)
}

func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r) {
			httpErr(w, 403, "bad host")
			return
		}
		c, err := r.Cookie("yx")
		if err != nil || c.Value != a.token {
			httpErr(w, 403, "нет доступа: откройте YExplorer через ярлык")
			return
		}
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				httpErr(w, 403, "bad origin")
				return
			}
		}
		h(w, r)
	}
}

func (a *App) routes(web fs.FS) http.Handler {
	mux := http.NewServeMux()
	static := http.FileServer(http.FS(web))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r) {
			httpErr(w, 403, "bad host")
			return
		}
		if k := r.URL.Query().Get("k"); k != "" && r.URL.Path == "/" {
			if k == a.token {
				http.SetCookie(w, &http.Cookie{Name: "yx", Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "yexplorer")
	})
	mux.HandleFunc("/api/live", a.guard(a.handleLive))
	mux.HandleFunc("/api/status", a.guard(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.ix.Status()) }))
	mux.HandleFunc("/api/search", a.guard(a.handleSearch))
	mux.HandleFunc("/api/preview", a.guard(a.handlePreview))
	mux.HandleFunc("/api/thumb", a.guard(a.handleThumb))
	mux.HandleFunc("/api/open", a.guard(a.handleOpen(false)))
	mux.HandleFunc("/api/reveal", a.guard(a.handleOpen(true)))
	mux.HandleFunc("/api/settings", a.guard(a.handleSettings))
	mux.HandleFunc("/api/pickfolder", a.guard(func(w http.ResponseWriter, r *http.Request) {
		p, err := pickFolder()
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]string{"path": p})
	}))
	mux.HandleFunc("/api/reindex", a.guard(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Wipe bool `json:"wipe"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.ix.Reindex(body.Wipe)
		writeJSON(w, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("/api/quit", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			close(a.quit)
		}()
	}))
	return mux
}

// SSE-соединение: пока окно открыто, оно висит; закрытие окна = разрыв.
func (a *App) handleLive(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	a.clients.Add(1)
	a.seen.Store(true)
	defer func() {
		a.clients.Add(-1)
		a.lastGone.Store(time.Now().UnixMilli())
	}()
	fmt.Fprint(w, "retry: 1000\n\n")
	fl.Flush()
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.quit:
			return
		case <-t.C:
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---------- поиск ----------

type Item struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Ext   string `json:"ext"`
	Cat   string `json:"cat"`
	Size  int64  `json:"size"`
	Mod   int64  `json:"mod"`
	Snip  string `json:"snip,omitempty"`
	Match string `json:"match"`
	IsDir bool   `json:"isDir"`
}

var catNames = [...]string{"other", "image", "doc", "code", "media", "folder"}

type hitHeap struct {
	h    []Hit
	less func(a, b Hit) bool // a лучше b
}

func (x *hitHeap) Len() int           { return len(x.h) }
func (x *hitHeap) Less(i, j int) bool { return x.less(x.h[j], x.h[i]) } // корень — худший
func (x *hitHeap) Swap(i, j int)      { x.h[i], x.h[j] = x.h[j], x.h[i] }
func (x *hitHeap) Push(v any)         { x.h = append(x.h, v.(Hit)) }
func (x *hitHeap) Pop() any {
	n := len(x.h)
	v := x.h[n-1]
	x.h = x.h[:n-1]
	return v
}

func topK(hits []Hit, k int, better func(a, b Hit) bool) []Hit {
	if len(hits) <= k*3 || len(hits) < 2000 {
		out := append([]Hit(nil), hits...)
		sort.SliceStable(out, func(i, j int) bool { return better(out[i], out[j]) })
		if len(out) > k {
			out = out[:k]
		}
		return out
	}
	hp := &hitHeap{less: better, h: make([]Hit, 0, k)}
	for _, h := range hits {
		if len(hp.h) < k {
			heap.Push(hp, h)
		} else if better(h, hp.h[0]) {
			hp.h[0] = h
			heap.Fix(hp, 0)
		}
	}
	out := hp.h
	sort.SliceStable(out, func(i, j int) bool { return better(out[i], out[j]) })
	return out
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := strings.TrimSpace(qv.Get("q"))
	cat := qv.Get("cat")
	sortBy := qv.Get("sort")
	cfg := a.config()
	limit := cfg.MaxResults
	if l, err := strconv.Atoi(qv.Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	t0 := time.Now()
	res := a.ix.Search(q, cfg.Content, cfg.Typos)
	fixed := ""
	if len(res.Hits) == 0 && q != "" {
		if f := fixLayout(q); f != "" {
			if r2 := a.ix.Search(f, cfg.Content, cfg.Typos); len(r2.Hits) > 0 {
				res, fixed = r2, f
			}
		}
	}
	entries := res.Snap.entries
	counts := map[string]int{}
	recentCut := time.Now().Add(-14 * 24 * time.Hour).Unix()
	filtered := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		e := &entries[h.Idx]
		counts["all"]++
		if e.Dir {
			counts["folders"]++
		} else {
			counts["files"]++
		}
		switch e.Cat {
		case CatImage:
			counts["images"]++
		case CatDoc:
			counts["docs"]++
		case CatCode:
			counts["code"]++
		case CatMedia:
			counts["media"]++
		}
		recent := !e.Dir && e.Mod >= recentCut
		if recent {
			counts["recent"]++
		}
		ok := true
		switch cat {
		case "files":
			ok = !e.Dir
		case "folders":
			ok = e.Dir
		case "images":
			ok = e.Cat == CatImage
		case "docs":
			ok = e.Cat == CatDoc
		case "code":
			ok = e.Cat == CatCode
		case "media":
			ok = e.Cat == CatMedia
		case "recent":
			ok = recent
		}
		if ok {
			filtered = append(filtered, h)
		}
	}
	var better func(a, b Hit) bool
	switch sortBy {
	case "name":
		better = func(x, y Hit) bool { return entries[x.Idx].Low < entries[y.Idx].Low }
	case "date":
		better = func(x, y Hit) bool { return entries[x.Idx].Mod > entries[y.Idx].Mod }
	case "size":
		better = func(x, y Hit) bool { return entries[x.Idx].Size > entries[y.Idx].Size }
	default:
		better = func(x, y Hit) bool {
			if x.Score != y.Score {
				return x.Score > y.Score
			}
			return entries[x.Idx].Mod > entries[y.Idx].Mod
		}
	}
	total := len(filtered)
	top := topK(filtered, limit, better)
	elapsed := time.Since(t0)

	toks := res.Toks
	items := make([]Item, len(top))
	for i, h := range top {
		e := &entries[h.Idx]
		m := "name"
		switch {
		case h.Flags&FFuzzy != 0:
			m = "fuzzy"
		case h.Flags&FName == 0 && h.Flags&FContent != 0:
			m = "content"
		case h.Flags&FName != 0 && h.Flags&FContent != 0:
			m = "both"
		}
		items[i] = Item{Path: e.Path, Name: e.Name, Dir: parentOf(e.Path), Ext: e.Ext, Cat: catNames[e.Cat], Size: e.Size, Mod: e.Mod, Match: m, IsDir: e.Dir}
	}
	a.fillSnippets(items, top, toks, cfg.Content)
	writeJSON(w, map[string]any{
		"items": items, "counts": counts, "total": total,
		"ms": float64(elapsed.Microseconds()) / 1000, "fixed": fixed, "toks": toks,
		"indexed": len(entries),
	})
}

func (a *App) fillSnippets(items []Item, top []Hit, toks []string, content bool) {
	var mu sync.Mutex
	closed := false
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	key := strings.Join(toks, " ")
	for i := range items {
		it := items[i]
		if it.IsDir || it.Cat == "media" || it.Cat == "other" {
			continue
		}
		hasContent := top[i].Flags&FContent != 0
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s := a.snippet(it, toks, key, hasContent)
			mu.Lock()
			if !closed {
				items[i].Snip = s
			}
			mu.Unlock()
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(900 * time.Millisecond):
	}
	mu.Lock()
	closed = true
	mu.Unlock()
}

func (a *App) textOf(it Item, limit int) string {
	if it.Cat == "image" {
		a.ix.cmu.RLock()
		t := a.ix.store.OCR[it.Path]
		a.ix.cmu.RUnlock()
		return t
	}
	if !textExts[it.Ext] && !zipDocExts[it.Ext] && it.Ext != "pdf" {
		return ""
	}
	return extractText(it.Path, it.Ext, limit)
}

func (a *App) snippet(it Item, toks []string, key string, hasContent bool) string {
	ck := fmt.Sprintf("%s|%d|%s|%v", it.Path, it.Mod, key, hasContent)
	a.snipMu.Lock()
	if s, ok := a.snips[ck]; ok {
		a.snipMu.Unlock()
		return s
	}
	a.snipMu.Unlock()
	var s string
	if hasContent {
		text := a.textOf(it, 600000)
		s = snippetAround(text, toks, 170)
		if s == "" {
			var stems []string
			for _, t := range toks {
				if r := []rune(t); len(r) > 4 {
					stems = append(stems, string(r[:len(r)-2]))
				}
			}
			s = snippetAround(text, stems, 170)
		}
		if s == "" {
			s = headSnippet(text, 6, 220)
		}
	} else if it.Cat == "code" || it.Cat == "doc" || it.Cat == "image" {
		s = headSnippet(a.textOf(it, 3000), 8, 260)
	}
	a.snipMu.Lock()
	if len(a.snips) > 3000 {
		a.snips = map[string]string{}
	}
	a.snips[ck] = s
	a.snipMu.Unlock()
	return s
}

// ---------- проверка путей ----------

func (a *App) validPath(p string) bool {
	if p == "" {
		return false
	}
	snap := a.ix.snapshot()
	if snap.byPath != nil {
		_, ok := snap.byPath[p]
		return ok
	}
	for _, r := range a.config().Folders {
		if hasPathPrefix(filepath.Clean(p), r) {
			return true
		}
	}
	return false
}

func (a *App) handleOpen(reveal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b); err != nil || !a.validPath(b.Path) {
			httpErr(w, 400, "файл не найден в индексе")
			return
		}
		var err error
		if reveal {
			err = revealPath(b.Path)
		} else {
			err = openPath(b.Path)
		}
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (a *App) handlePreview(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	if !a.validPath(p) {
		httpErr(w, 404, "нет файла")
		return
	}
	st, err := os.Stat(p)
	if err != nil {
		httpErr(w, 404, "нет файла")
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	it := Item{Path: p, Ext: ext, Cat: catNames[catFor(ext, st.IsDir())]}
	out := map[string]any{"size": st.Size(), "mod": st.ModTime().Unix(), "isDir": st.IsDir(), "cat": it.Cat}
	if !st.IsDir() {
		text := a.textOf(it, 24000)
		out["text"] = text
		if ext == "pdf" || zipDocExts[ext] {
			out["kind"] = "doc"
		}
	} else {
		if ents, err := os.ReadDir(p); err == nil {
			var names []string
			for i, e := range ents {
				if i >= 40 {
					names = append(names, "…")
					break
				}
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			out["list"] = names
		}
	}
	writeJSON(w, out)
}

// ---------- миниатюры ----------

func (a *App) handleThumb(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	if !a.validPath(p) {
		http.NotFound(w, r)
		return
	}
	width := 360
	if v, err := strconv.Atoi(r.URL.Query().Get("w")); err == nil && v >= 64 && v <= 2000 {
		width = v
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	switch ext {
	case "png", "jpg", "jpeg", "gif":
		sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d|%d", p, st.ModTime().UnixNano(), st.Size(), width)))
		cdir := filepath.Join(dataDir(), "thumbs")
		os.MkdirAll(cdir, 0o755)
		cf := filepath.Join(cdir, hex.EncodeToString(sum[:])+".jpg")
		if b, err := os.ReadFile(cf); err == nil {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(b)
			return
		}
		if st.Size() > 40<<20 {
			http.NotFound(w, r)
			return
		}
		b, err := makeThumb(p, width)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_ = os.WriteFile(cf, b, 0o644)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(b)
	case "svg", "webp", "bmp", "ico", "avif":
		if st.Size() > 12<<20 {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
	default:
		http.NotFound(w, r)
	}
}

func makeThumb(path string, width int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > 60_000_000 {
		return nil, fmt.Errorf("слишком большое")
	}
	f.Seek(0, 0)
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	sb := img.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw := width
	if sw < dw {
		dw = sw
	}
	dh := sh * dw / sw
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// усреднение блока пикселей
	for y := 0; y < dh; y++ {
		y0 := sb.Min.Y + y*sh/dh
		y1 := sb.Min.Y + (y+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := sb.Min.X + x*sw/dw
			x1 := sb.Min.X + (x+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as, n uint32
			stepY := (y1 - y0 + 2) / 3
			stepX := (x1 - x0 + 2) / 3
			if stepY < 1 {
				stepY = 1
			}
			if stepX < 1 {
				stepX = 1
			}
			for yy := y0; yy < y1; yy += stepY {
				for xx := x0; xx < x1; xx += stepX {
					r, g, b, a := img.At(xx, yy).RGBA()
					rs += r >> 8
					gs += g >> 8
					bs += b >> 8
					as += a >> 8
					n++
				}
			}
			if n == 0 {
				continue
			}
			r, g, b, a := rs/n, gs/n, bs/n, as/n
			// премультиплицированные значения → поверх белого
			r = r + (255 - a)
			g = g + (255 - a)
			b = b + (255 - a)
			if r > 255 {
				r = 255
			}
			if g > 255 {
				g = 255
			}
			if b > 255 {
				b = 255
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r), uint8(g), uint8(b), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 78}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var _ = png.Decode
var _ = gif.Decode

// ---------- настройки ----------

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"config": a.config(), "exe": a.exe, "windows": isWindows(), "version": version, "dataDir": dataDir()})
		return
	}
	old := a.config()
	n := old.clone()
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(n); err != nil {
		httpErr(w, 400, "некорректные настройки")
		return
	}
	n.sanitize()
	needReindex := !sameList(old.Folders, n.Folders) || !sameList(old.Exclusions, n.Exclusions) ||
		(n.Content && !old.Content) || (n.OCR && !old.OCR)
	cfgMu.Lock()
	*a.ix.cfg = *n
	cfgMu.Unlock()
	n.save()
	if old.StartAtLogin != n.StartAtLogin {
		setAutostart(n.StartAtLogin, a.exe)
		a.resident.Store(n.StartAtLogin)
	}
	if needReindex {
		a.ix.Reindex(false)
	}
	writeJSON(w, map[string]any{"config": n, "reindex": needReindex})
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
