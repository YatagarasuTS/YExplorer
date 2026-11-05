package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (ix *Index) Stage() string { return ix.stage.Load().(string) }

// Reindex просит перечитать файлы (текущий проход отменяется).
func (ix *Index) Reindex(wipe bool) {
	if wipe {
		ix.cmu.Lock()
		ix.store = newStore()
		ix.docEntry = nil
		ix.vocabDirty = true
		ix.cmu.Unlock()
	}
	ix.runMu.Lock()
	if ix.cancel != nil {
		ix.cancel()
	}
	ix.runMu.Unlock()
	select {
	case ix.kickCh <- struct{}{}:
	default:
	}
}

func (ix *Index) Run() {
	for {
		ctx, cancel := context.WithCancel(context.Background())
		ix.runMu.Lock()
		ix.cancel = cancel
		ix.runMu.Unlock()
		ix.runOnce(ctx)
		cancel()
		select {
		case <-ix.kickCh:
		case <-time.After(10 * time.Minute):
		}
	}
}

func (ix *Index) config() *Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return ix.cfg.clone()
}

func (ix *Index) runOnce(ctx context.Context) {
	cfg := ix.config()
	t0 := time.Now()
	ix.stage.Store("scanning")
	ix.scanned.Store(0)
	snap, roots := ix.scan(ctx, cfg)
	if ctx.Err() != nil {
		return
	}
	ix.scanMs.Store(time.Since(t0).Milliseconds())
	ix.mu.Lock()
	ix.snap = snap
	ix.mu.Unlock()
	ix.linkDocs(snap)
	ix.prune(snap, roots)
	if cfg.Content {
		ix.stage.Store("content")
		ix.contentStage(ctx, snap)
		if ctx.Err() != nil {
			return
		}
	}
	if cfg.OCR && ocrSupported() {
		ix.stage.Store("ocr")
		ix.ocrStage(ctx, snap)
		if ctx.Err() != nil {
			return
		}
	}
	ix.lastRun.Store(time.Now().Unix())
	ix.stage.Store("ready")
	ix.saveStore()
}

func (ix *Index) saveStore() {
	ix.cmu.RLock()
	_ = ix.store.save()
	ix.cmu.RUnlock()
}

// ---------- сканирование файловой системы ----------

func dedupeRoots(roots []string) []string {
	var out []string
	for _, r := range roots {
		r = filepath.Clean(r)
		skip := false
		for _, o := range roots {
			o = filepath.Clean(o)
			if o != r && hasPathPrefix(r, o) {
				skip = true
			}
		}
		dup := false
		for _, x := range out {
			if strings.EqualFold(x, r) {
				dup = true
			}
		}
		if !skip && !dup {
			out = append(out, r)
		}
	}
	return out
}

func (ix *Index) scan(ctx context.Context, cfg *Config) (*Snapshot, []string) {
	excl := map[string]bool{}
	var exclPaths []string
	for _, x := range cfg.Exclusions {
		if strings.ContainsAny(x, `\/`) {
			exclPaths = append(exclPaths, strings.ToLower(filepath.Clean(x)))
		} else {
			excl[strings.ToLower(x)] = true
		}
	}
	var local []Entry
	var okRoots []string
	for _, root := range dedupeRoots(cfg.Folders) {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}
		okRoots = append(okRoots, root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			isDir := d.IsDir()
			if p == root {
				return nil
			}
			if isDir {
				ln := strings.ToLower(name)
				if excl[ln] {
					return fs.SkipDir
				}
				if len(exclPaths) > 0 {
					lp := strings.ToLower(p)
					for _, x := range exclPaths {
						if lp == x || strings.HasPrefix(lp, x+string(filepath.Separator)) {
							return fs.SkipDir
						}
					}
				}
			} else {
				if strings.HasPrefix(name, "~$") || name == "desktop.ini" || name == "Thumbs.db" || name == ".DS_Store" {
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			low := norm(name)
			ext := ""
			if !isDir {
				if k := strings.LastIndexByte(low, '.'); k >= 0 && k < len(low)-1 {
					ext = low[k+1:]
				}
			}
			e := Entry{Path: p, Name: name, Low: low, Ext: ext, Size: info.Size(), Mod: info.ModTime().Unix(), Dir: isDir}
			e.Cat = catFor(ext, isDir)
			local = append(local, e)
			c := ix.scanned.Add(1)
			if c%5000 == 0 {
				part := &Snapshot{entries: local[:len(local):len(local)]}
				ix.mu.Lock()
				if len(ix.snap.entries) < len(part.entries) || ix.snap.byPath == nil {
					ix.snap = part
				}
				ix.mu.Unlock()
			}
			return nil
		})
		if ctx.Err() != nil {
			return nil, nil
		}
	}
	return buildSnapshot(local), okRoots
}

// linkDocs связывает документы из индекса содержимого с записями снимка.
func (ix *Index) linkDocs(snap *Snapshot) {
	ix.cmu.Lock()
	defer ix.cmu.Unlock()
	de := make([]int32, ix.store.Next+1)
	for i := range de {
		de[i] = -1
	}
	for i := range snap.entries {
		e := &snap.entries[i]
		if rec, ok := ix.store.Docs[e.Path]; ok && rec.Mod == e.Mod && rec.Size == e.Size && int(rec.ID) < len(de) {
			de[rec.ID] = int32(i)
		}
	}
	ix.docEntry = de
	ix.vocabDirty = true
}

// prune удаляет из индекса содержимого файлы, которых больше нет.
func (ix *Index) prune(snap *Snapshot, roots []string) {
	ix.cmu.Lock()
	defer ix.cmu.Unlock()
	for p := range ix.store.Docs {
		if _, ok := snap.byPath[p]; ok {
			continue
		}
		under := false
		for _, r := range roots {
			if hasPathPrefix(p, r) {
				under = true
				break
			}
		}
		if under {
			delete(ix.store.Docs, p)
			delete(ix.store.OCR, p)
		}
	}
}

// ---------- чтение содержимого ----------

func (ix *Index) contentStage(ctx context.Context, snap *Snapshot) {
	type job struct {
		idx int32
	}
	var todo []int32
	var total, done int64
	ix.cmu.RLock()
	for i := range snap.entries {
		e := &snap.entries[i]
		if !contentEligible(e) {
			continue
		}
		total++
		if rec, ok := ix.store.Docs[e.Path]; ok && rec.Mod == e.Mod && rec.Size == e.Size {
			done++
		} else {
			todo = append(todo, int32(i))
		}
	}
	ix.cmu.RUnlock()
	sort.Slice(todo, func(a, b int) bool { return snap.entries[todo[a]].Mod > snap.entries[todo[b]].Mod })
	ix.cTotal.Store(total)
	ix.cDone.Store(done)
	ix.cBase.Store(done)
	ix.cStart.Store(time.Now().UnixMilli())
	if len(todo) == 0 {
		return
	}
	workers := runtime.NumCPU() - 1
	if workers < 2 {
		workers = 2
	}
	if workers > 8 {
		workers = 8
	}
	ch := make(chan int32, 256)
	var wg sync.WaitGroup
	var sinceSave atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := map[string]struct{}{}
			for idx := range ch {
				if ctx.Err() != nil {
					continue
				}
				e := &snap.entries[idx]
				text := extractText(e.Path, e.Ext, 0)
				for k := range seen {
					delete(seen, k)
				}
				var words []string
				tokenize(text, func(w string) {
					if len(seen) >= 4000 {
						return
					}
					if _, dup := seen[w]; !dup {
						seen[w] = struct{}{}
						words = append(words, w)
					}
				})
				ix.cmu.Lock()
				id := ix.store.addDoc(e.Path, e.Mod, e.Size, words)
				for int(id) >= len(ix.docEntry) {
					ix.docEntry = append(ix.docEntry, -1)
				}
				ix.docEntry[id] = idx
				ix.vocabDirty = true
				ix.cmu.Unlock()
				ix.cDone.Add(1)
				sinceSave.Add(1)
			}
		}()
	}
	last := time.Now()
feed:
	for _, idx := range todo {
		select {
		case ch <- idx:
		case <-ctx.Done():
			break feed
		}
		if time.Since(last) > 90*time.Second && sinceSave.Load() > 2000 {
			last = time.Now()
			sinceSave.Store(0)
			ix.saveStore()
		}
	}
	close(ch)
	wg.Wait()
}

// ---------- OCR скриншотов ----------

func (ix *Index) ocrStage(ctx context.Context, snap *Snapshot) {
	var todo []int32
	ix.cmu.RLock()
	for i := range snap.entries {
		e := &snap.entries[i]
		if e.Dir || !ocrExts[e.Ext] || e.Size < 6*1024 || e.Size > 30<<20 {
			continue
		}
		if rec, ok := ix.store.Docs[e.Path]; ok && rec.Mod == e.Mod && rec.Size == e.Size {
			continue
		}
		todo = append(todo, int32(i))
	}
	ix.cmu.RUnlock()
	sort.Slice(todo, func(a, b int) bool { return snap.entries[todo[a]].Mod > snap.entries[todo[b]].Mod })
	if len(todo) > 20000 {
		todo = todo[:20000]
	}
	ix.oTotal.Store(int64(len(todo)))
	ix.oDone.Store(0)
	if len(todo) == 0 {
		return
	}
	proc, err := newOCR()
	if err != nil {
		return
	}
	defer func() { proc.Close() }()
	for n, idx := range todo {
		if ctx.Err() != nil {
			return
		}
		e := &snap.entries[idx]
		text, err := proc.Recognize(e.Path)
		if err != nil {
			text = ""
			proc.Close()
			if proc, err = newOCR(); err != nil {
				return
			}
		}
		var words []string
		seen := map[string]struct{}{}
		tokenize(text, func(w string) {
			if _, dup := seen[w]; !dup {
				seen[w] = struct{}{}
				words = append(words, w)
			}
		})
		ix.cmu.Lock()
		id := ix.store.addDoc(e.Path, e.Mod, e.Size, words)
		if text != "" {
			ix.store.OCR[e.Path] = text
		}
		for int(id) >= len(ix.docEntry) {
			ix.docEntry = append(ix.docEntry, -1)
		}
		ix.docEntry[id] = idx
		ix.vocabDirty = true
		ix.cmu.Unlock()
		ix.oDone.Add(1)
		if n%500 == 499 {
			ix.saveStore()
		}
	}
}

// ---------- статус ----------

type Status struct {
	Stage        string `json:"stage"`
	Scanned      int64  `json:"scanned"`
	Files        int    `json:"files"`
	Folders      int    `json:"folders"`
	ContentDone  int64  `json:"contentDone"`
	ContentTotal int64  `json:"contentTotal"`
	OCRDone      int64  `json:"ocrDone"`
	OCRTotal     int64  `json:"ocrTotal"`
	ETA          int64  `json:"eta"`
	FirstRun     bool   `json:"firstRun"`
	ScanMs       int64  `json:"scanMs"`
	Docs         int    `json:"docs"`
	OCR          bool   `json:"ocrAvailable"`
	LastRun      int64  `json:"lastRun"`
}

func (ix *Index) Status() Status {
	snap := ix.snapshot()
	st := Status{
		Stage: ix.Stage(), Scanned: ix.scanned.Load(),
		ContentDone: ix.cDone.Load(), ContentTotal: ix.cTotal.Load(),
		OCRDone: ix.oDone.Load(), OCRTotal: ix.oTotal.Load(),
		FirstRun: ix.firstRun, ScanMs: ix.scanMs.Load(), OCR: ocrSupported(), LastRun: ix.lastRun.Load(),
	}
	st.Folders = snap.counts[CatFolder]
	st.Files = len(snap.entries) - st.Folders
	if snap.byPath == nil {
		st.Files = len(snap.entries)
	}
	ix.cmu.RLock()
	st.Docs = len(ix.store.Docs)
	ix.cmu.RUnlock()
	if st.Stage == "content" {
		el := float64(time.Now().UnixMilli()-ix.cStart.Load()) / 1000
		done := float64(st.ContentDone - ix.cBase.Load())
		if done > 20 && el > 0.5 {
			rate := done / el
			st.ETA = int64(float64(st.ContentTotal-st.ContentDone) / rate)
		}
	}
	return st
}
