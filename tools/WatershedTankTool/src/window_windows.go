//go:build windows

package main

// The tool's own window: the UI page hosted in WebView2, the web engine
// built into Windows 10/11, inside a normal app window with its own title,
// icon and taskbar button. No browser is opened. If WebView2 isn't
// available, the caller falls back to an Edge/Chrome app window.

import (
	_ "embed"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed assets/watershed.ico
var appIcon []byte

func init() {
	// the window and WebView2 must stay on the program's main OS thread
	runtime.LockOSThread()
}

var (
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procCreateIconFromResourceEx      = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW                  = user32.NewProc("SendMessageW")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
)

func appDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "WatershedTankTool")
}

// runAppWindow shows the UI in the tool's own window and returns when the
// window is closed. It returns false straight away if WebView2 can't start.
func runAppWindow(url string, a *App) bool {
	// sharp text on high-DPI screens (per-monitor v2)
	if procSetProcessDpiAwarenessContext.Find() == nil {
		procSetProcessDpiAwarenessContext.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(appDataDir(), "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  "Watershed Tank Tool",
			Width:  1320,
			Height: 900,
			Center: true,
		},
	})
	if w == nil {
		a.sysLine("WebView2 isn't available - using a browser window instead")
		return false
	}
	hwnd := uintptr(w.Window())
	setWindowIcon(hwnd)
	a.mu.Lock()
	a.showWindow = func() {
		w.Dispatch(func() {
			procShowWindow.Call(hwnd, 9) // SW_RESTORE
			procSetForegroundWindow.Call(hwnd)
		})
	}
	a.mu.Unlock()
	w.Navigate(url)
	w.Run()
	w.Destroy()
	return true
}

// setWindowIcon gives the window (and its taskbar button) the Watershed
// icon, picking the best sizes from the embedded .ico
func setWindowIcon(hwnd uintptr) {
	big, _, _ := procGetSystemMetrics.Call(11)   // SM_CXICON
	small, _, _ := procGetSystemMetrics.Call(49) // SM_CXSMICON
	for _, s := range []struct {
		which uintptr
		size  int
	}{{1, int(big)}, {0, int(small)}} { // ICON_BIG, ICON_SMALL
		img := icoImage(appIcon, s.size)
		if img == nil {
			continue
		}
		h, _, _ := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&img[0])), uintptr(len(img)),
			1, 0x00030000, uintptr(s.size), uintptr(s.size), 0)
		if h != 0 {
			procSendMessageW.Call(hwnd, 0x0080, s.which, h) // WM_SETICON
		}
	}
}

// icoImage returns the image data of the entry closest to (and not smaller
// than, if possible) the wanted size
func icoImage(ico []byte, want int) []byte {
	if len(ico) < 6 {
		return nil
	}
	n := int(binary.LittleEndian.Uint16(ico[4:6]))
	best, bestSize := -1, 0
	for i := 0; i < n; i++ {
		e := 6 + 16*i
		if e+16 > len(ico) {
			break
		}
		sz := int(ico[e])
		if sz == 0 {
			sz = 256
		}
		better := best < 0 ||
			(sz >= want && (bestSize < want || sz < bestSize)) ||
			(sz < want && bestSize < want && sz > bestSize)
		if better {
			best, bestSize = i, sz
		}
	}
	if best < 0 {
		return nil
	}
	e := 6 + 16*best
	size := int(binary.LittleEndian.Uint32(ico[e+8:]))
	off := int(binary.LittleEndian.Uint32(ico[e+12:]))
	if off+size > len(ico) {
		return nil
	}
	return ico[off : off+size]
}

// iconFile writes the icon next to the tool's data so shortcuts can use it
func iconFile() string {
	p := filepath.Join(appDataDir(), "watershed.ico")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if b, err := os.ReadFile(p); err != nil || len(b) != len(appIcon) {
		os.WriteFile(p, appIcon, 0o644)
	}
	return p
}
