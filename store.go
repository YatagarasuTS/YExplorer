package main

import (
	"compress/gzip"
	"encoding/gob"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store — сохраняемый на диск индекс содержимого (слово → документы).
type DocRec struct {
	ID   uint32
	Mod  int64
	Size int64
}

type Store struct {
	Version int
	Next    uint32
	Docs    map[string]DocRec
	Post    map[string][]uint32
	OCR     map[string]string
}

const storeVersion = 3

func newStore() *Store {
	return &Store{Version: storeVersion, Docs: map[string]DocRec{}, Post: map[string][]uint32{}, OCR: map[string]string{}}
}

func storePath() string { return filepath.Join(dataDir(), "index.gz") }

func loadStore() *Store {
	f, err := os.Open(storePath())
	if err != nil {
		return newStore()
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return newStore()
	}
	defer zr.Close()
	s := newStore()
	if err := gob.NewDecoder(zr).Decode(s); err != nil || s.Version != storeVersion {
		return newStore()
	}
	if s.Docs == nil || s.Post == nil {
		return newStore()
	}
	if s.OCR == nil {
		s.OCR = map[string]string{}
	}
	return s
}

// save вызывается под RLock индекса.
func (s *Store) save() error {
	tmp := storePath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	err = gob.NewEncoder(zw).Encode(s)
	zw.Close()
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, storePath())
}

func (s *Store) addDoc(path string, mod, size int64, words []string) uint32 {
	s.Next++
	id := s.Next
	s.Docs[path] = DocRec{ID: id, Mod: mod, Size: size}
	for _, w := range words {
		s.Post[w] = append(s.Post[w], id)
	}
	return id
}

func sortedVocab(post map[string][]uint32) []string {
	v := make([]string, 0, len(post))
	for w := range post {
		v = append(v, w)
	}
	sort.Strings(v)
	return v
}

func hasPathPrefix(p, root string) bool {
	if isWindows() {
		p, root = strings.ToLower(p), strings.ToLower(root)
	}
	root = strings.TrimRight(root, `\/`)
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator)) || strings.HasPrefix(p, root+"/")
}
