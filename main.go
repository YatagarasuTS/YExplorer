package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	version       = "1.0.0"
	preferredPort = 38457
)

type session struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func sessionFile() string { return filepath.Join(dataDir(), "session.json") }

func readSession() *session {
	b, err := os.ReadFile(sessionFile())
	if err != nil {
		return nil
	}
	var s session
	if json.Unmarshal(b, &s) != nil || s.Port == 0 {
		return nil
	}
	return &s
}

func pingServer(port int) bool {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32))
	return string(b) == "yexplorer"
}

func (a *App) url() string {
	return fmt.Sprintf("http://127.0.0.1:%d/?k=%s", a.port, a.token)
}

func (a *App) openWindow() {
	if os.Getenv("YX_NOLAUNCH") != "" {
		fmt.Println("YExplorer:", a.url())
		return
	}
	_ = launchWindow(a.url())
}

func main() {
	background := flag.Bool("background", false, "запуск без окна (автозапуск)")
	flag.Parse()
	exe, _ := os.Executable()

	if s := readSession(); s != nil && pingServer(s.Port) {
		// уже запущено — просто открываем окно
		if !*background && os.Getenv("YX_NOLAUNCH") == "" {
			_ = launchWindow(fmt.Sprintf("http://127.0.0.1:%d/?k=%s", s.Port, s.Token))
		}
		return
	}

	cfg := loadConfig()
	tb := make([]byte, 16)
	rand.Read(tb)
	a := &App{exe: exe, token: hex.EncodeToString(tb), started: time.Now(), snips: map[string]string{}, quit: make(chan struct{})}
	a.resident.Store(*background || cfg.StartAtLogin)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferredPort))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, "не удалось запустить сервер:", err)
			os.Exit(1)
		}
	}
	a.port = ln.Addr().(*net.TCPAddr).Port

	a.ix = newIndex(cfg)
	sub, _ := fs.Sub(webFS, "web")
	srv := &http.Server{Handler: a.routes(sub)}
	go srv.Serve(ln)
	sb, _ := json.Marshal(session{Port: a.port, Token: a.token})
	_ = os.WriteFile(sessionFile(), sb, 0o600)

	go a.ix.Run()
	if cfg.StartAtLogin {
		setAutostart(true, exe)
	}
	registerHotkey(func() {
		if a.clients.Load() == 0 {
			a.openWindow()
		}
	})
	if !*background {
		a.openWindow()
	}

	// ждём закрытия окна
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.quit:
			a.shutdown()
			return
		case <-tick.C:
			if a.resident.Load() {
				continue
			}
			if a.clients.Load() == 0 {
				if a.seen.Load() {
					if time.Since(time.UnixMilli(a.lastGone.Load())) > 6*time.Second {
						a.shutdown()
						return
					}
				} else if time.Since(a.started) > 3*time.Minute {
					a.shutdown()
					return
				}
			}
		}
	}
}

func (a *App) shutdown() {
	if a.ix.Stage() == "ready" || a.ix.Stage() == "ocr" || a.ix.Stage() == "content" {
		a.ix.saveStore()
	}
	if s := readSession(); s != nil && s.Token == a.token {
		os.Remove(sessionFile())
	}
}
