//go:build windows

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	shell32           = syscall.NewLazyDLL("shell32.dll")
	user32            = syscall.NewLazyDLL("user32.dll")
	pGetLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	pGetDriveType     = kernel32.NewProc("GetDriveTypeW")
	pShellExecute     = shell32.NewProc("ShellExecuteW")
	pRegisterHotKey   = user32.NewProc("RegisterHotKey")
	pGetMessage       = user32.NewProc("GetMessageW")
)

const createNoWindow = 0x08000000

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

func defaultFolders() []string {
	mask, _, _ := pGetLogicalDrives.Call()
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		t, _, _ := pGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		if t == 3 { // DRIVE_FIXED
			out = append(out, root)
		}
	}
	if len(out) == 0 {
		h, _ := os.UserHomeDir()
		out = []string{h}
	}
	return out
}

func shellOpen(target string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r, _, _ := pShellExecute.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, 1)
	if r <= 32 {
		return errors.New("не удалось открыть файл")
	}
	return nil
}

func openPath(p string) error { return shellOpen(p) }

func revealPath(p string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + p + `"`}
	return cmd.Start()
}

func pickFolder() (string, error) {
	script := `[Console]::OutputEncoding=[Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.Form -Property @{TopMost=$true; ShowInTaskbar=$false; Opacity=0; StartPosition='CenterScreen'}
$f.Show()
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = 'Выберите папку для поиска'
if ($d.ShowDialog($f) -eq 'OK') { [Console]::Out.Write($d.SelectedPath) }
$f.Close()`
	cmd := hidden(exec.Command("powershell.exe", "-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-EncodedCommand", psEncode(script)))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func psEncode(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func setAutostart(on bool, exe string) {
	key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	if on {
		_ = hidden(exec.Command("reg.exe", "add", key, "/v", "YExplorer", "/t", "REG_SZ", "/d", `"`+exe+`" --background`, "/f")).Run()
	} else {
		_ = hidden(exec.Command("reg.exe", "delete", key, "/v", "YExplorer", "/f")).Run()
	}
}

func findBrowser() string {
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	la := os.Getenv("LocalAppData")
	cands := []string{
		filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(la, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(pf, `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(la, `Yandex\YandexBrowser\Application\browser.exe`),
	}
	for _, c := range cands {
		if c != "" && filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	return ""
}

func launchWindow(url string) error {
	if b := findBrowser(); b != "" {
		cmd := exec.Command(b, "--app="+url,
			"--user-data-dir="+filepath.Join(dataDir(), "window"),
			"--window-size=1240,800", "--no-first-run", "--no-default-browser-check",
			"--disable-features=Translate,msEdgeSignin", "--disable-sync")
		if err := cmd.Start(); err == nil {
			go cmd.Wait()
			return nil
		}
	}
	return shellOpen(url)
}

func registerHotkey(onPress func()) {
	go func() {
		runtime.LockOSThread()
		const modAlt, modNoRepeat, vkSpace = 0x1, 0x4000, 0x20
		r, _, _ := pRegisterHotKey.Call(0, 1, modAlt|modNoRepeat, vkSpace)
		if r == 0 {
			return
		}
		var msg struct {
			hwnd    uintptr
			message uint32
			wParam  uintptr
			lParam  uintptr
			time    uint32
			x, y    int32
		}
		for {
			r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			if msg.message == 0x0312 { // WM_HOTKEY
				onPress()
			}
		}
	}()
}

// ---------- OCR через встроенный в Windows 10/11 движок ----------

const psOCRScript = `$ErrorActionPreference = 'Stop'
try {
Add-Type -AssemblyName System.Runtime.WindowsRuntime
[void][Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType=WindowsRuntime]
[void][Windows.Media.Ocr.OcrResult, Windows.Foundation, ContentType=WindowsRuntime]
[void][Windows.Graphics.Imaging.BitmapDecoder, Windows.Foundation, ContentType=WindowsRuntime]
[void][Windows.Graphics.Imaging.SoftwareBitmap, Windows.Foundation, ContentType=WindowsRuntime]
[void][Windows.Storage.StorageFile, Windows.Storage, ContentType=WindowsRuntime]
[void][Windows.Storage.FileAccessMode, Windows.Storage, ContentType=WindowsRuntime]
[void][Windows.Storage.Streams.IRandomAccessStream, Windows.Storage, ContentType=WindowsRuntime]
$asTask = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object { $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation` + "`" + `1' })[0]
function Await($op, $type) { $t = $asTask.MakeGenericMethod($type).Invoke($null, @($op)); [void]$t.Wait(-1); $t.Result }
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
if ($null -eq $engine) { [Console]::Out.WriteLine('FAIL'); [Console]::Out.Flush(); exit }
} catch { [Console]::Out.WriteLine('FAIL'); [Console]::Out.Flush(); exit }
[Console]::Out.WriteLine('READY'); [Console]::Out.Flush()
while ($true) {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { break }
  $text = ''
  try {
    $path = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($line))
    $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($path)) ([Windows.Storage.StorageFile])
    $stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
    $dec = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
    $bmp = Await ($dec.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
    $res = Await ($engine.RecognizeAsync($bmp)) ([Windows.Media.Ocr.OcrResult])
    $text = $res.Text
    $stream.Dispose()
  } catch { $text = '' }
  [Console]::Out.WriteLine([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$text)))
  [Console]::Out.Flush()
}
`

func ocrSupported() bool { return true }

type ocrProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	mu  sync.Mutex
}

func readLineTimeout(r *bufio.Reader, d time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := r.ReadString('\n')
		ch <- res{strings.TrimSpace(s), err}
	}()
	select {
	case x := <-ch:
		return x.s, x.err
	case <-time.After(d):
		return "", errors.New("timeout")
	}
}

func newOCR() (*ocrProc, error) {
	cmd := hidden(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", psEncode(psOCRScript)))
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &ocrProc{cmd: cmd, in: in, out: bufio.NewReader(out)}
	line, err := readLineTimeout(p.out, 40*time.Second)
	if err != nil || line != "READY" {
		p.Close()
		return nil, errors.New("OCR недоступен")
	}
	return p, nil
}

func (p *ocrProc) Recognize(path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := io.WriteString(p.in, base64.StdEncoding.EncodeToString([]byte(path))+"\n"); err != nil {
		return "", err
	}
	line, err := readLineTimeout(p.out, 45*time.Second)
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return "", nil
	}
	return string(bytes.TrimSpace(b)), nil
}

func (p *ocrProc) Close() {
	if p == nil || p.cmd == nil {
		return
	}
	p.in.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	go p.cmd.Wait()
}
