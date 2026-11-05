package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type CustomTheme struct {
	Bg     string `json:"bg"`
	Panel  string `json:"panel"`
	Text   string `json:"text"`
	Accent string `json:"accent"`
}

type Config struct {
	Theme        string      `json:"theme"` // dark | light | custom
	Custom       CustomTheme `json:"custom"`
	Folders      []string    `json:"folders"`
	Exclusions   []string    `json:"exclusions"`
	MaxResults   int         `json:"maxResults"`
	StartAtLogin bool        `json:"startAtLogin"`
	Content      bool        `json:"content"`
	OCR          bool        `json:"ocr"`
	Typos        bool        `json:"typos"`
	View         string      `json:"view"` // grid | list
}

var defaultExclusions = []string{
	"node_modules", ".git", "$Recycle.Bin", "System Volume Information", "AppData",
	"Windows", "Program Files", "Program Files (x86)", "ProgramData", "Recovery",
	"$WinREAgent", "Windows.old", "__pycache__", ".cache", ".gradle", ".npm", ".vs", "$SysReset",
}

func defaultConfig() *Config {
	return &Config{
		Theme: "dark",
		Custom: CustomTheme{
			Bg: "#12101a", Panel: "#1c1830", Text: "#efeaff", Accent: "#9b6dff",
		},
		Folders:    defaultFolders(),
		Exclusions: append([]string(nil), defaultExclusions...),
		MaxResults: 60,
		Content:    true,
		OCR:        true,
		Typos:      true,
		View:       "grid",
	}
}

func dataDir() string {
	if d := os.Getenv("YX_DATA"); d != "" {
		os.MkdirAll(d, 0o755)
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	d := filepath.Join(base, "YExplorer")
	os.MkdirAll(d, 0o755)
	return d
}

var cfgMu sync.RWMutex

func loadConfig() *Config {
	c := defaultConfig()
	b, err := os.ReadFile(filepath.Join(dataDir(), "settings.json"))
	if err == nil {
		_ = json.Unmarshal(b, c)
	}
	if env := os.Getenv("YX_ROOTS"); env != "" {
		c.Folders = filepath.SplitList(env)
	}
	c.sanitize()
	return c
}

func (c *Config) sanitize() {
	if c.Theme != "dark" && c.Theme != "light" && c.Theme != "custom" {
		c.Theme = "dark"
	}
	if c.MaxResults < 10 {
		c.MaxResults = 10
	}
	if c.MaxResults > 500 {
		c.MaxResults = 500
	}
	if c.View != "list" {
		c.View = "grid"
	}
	var f []string
	seen := map[string]bool{}
	for _, p := range c.Folders {
		p = strings.TrimSpace(p)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		f = append(f, p)
	}
	c.Folders = f
	var ex []string
	for _, p := range c.Exclusions {
		if p = strings.TrimSpace(p); p != "" {
			ex = append(ex, p)
		}
	}
	c.Exclusions = ex
	fix := func(s *string, def string) {
		if len(*s) != 7 || (*s)[0] != '#' {
			*s = def
		}
	}
	fix(&c.Custom.Bg, "#12101a")
	fix(&c.Custom.Panel, "#1c1830")
	fix(&c.Custom.Text, "#efeaff")
	fix(&c.Custom.Accent, "#9b6dff")
}

func (c *Config) save() {
	b, _ := json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(filepath.Join(dataDir(), "settings.json"), b, 0o644)
}

func (c *Config) clone() *Config {
	n := *c
	n.Folders = append([]string(nil), c.Folders...)
	n.Exclusions = append([]string(nil), c.Exclusions...)
	return &n
}

func isWindows() bool { return runtime.GOOS == "windows" }
