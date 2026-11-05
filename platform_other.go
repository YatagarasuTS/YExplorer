//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func defaultFolders() []string {
	h, _ := os.UserHomeDir()
	return []string{h}
}

func openPath(p string) error {
	if os.Getenv("YX_NOOPEN") != "" {
		fmt.Println("open:", p)
		return nil
	}
	return exec.Command("xdg-open", p).Start()
}

func revealPath(p string) error {
	if os.Getenv("YX_NOOPEN") != "" {
		fmt.Println("reveal:", p)
		return nil
	}
	return exec.Command("xdg-open", filepath.Dir(p)).Start()
}

func pickFolder() (string, error) {
	return "", errors.New("не поддерживается на этой системе")
}

func setAutostart(on bool, exe string) {}

func launchWindow(url string) error {
	fmt.Println("YExplorer:", url)
	return nil
}

func registerHotkey(onPress func()) {}

func ocrSupported() bool { return false }

type ocrProc struct{}

func newOCR() (*ocrProc, error)                     { return nil, errors.New("ocr unsupported") }
func (p *ocrProc) Recognize(string) (string, error) { return "", errors.New("ocr unsupported") }
func (p *ocrProc) Close()                           {}
