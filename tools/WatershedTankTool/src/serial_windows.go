//go:build windows

package main

// Windows serial port + USB enumeration using only the standard library
// (syscall into kernel32/advapi32), so the tool ships as one .exe with no
// drivers or runtimes beyond the board's own USB-serial driver.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetCommState       = kernel32.NewProc("GetCommState")
	procSetCommState       = kernel32.NewProc("SetCommState")
	procSetCommTimeouts    = kernel32.NewProc("SetCommTimeouts")
	procEscapeCommFunction = kernel32.NewProc("EscapeCommFunction")
	procPurgeComm          = kernel32.NewProc("PurgeComm")
	procSetupComm          = kernel32.NewProc("SetupComm")
	advapi32               = syscall.NewLazyDLL("advapi32.dll")
	procRegEnumValueW      = advapi32.NewProc("RegEnumValueW")
)

// DCB from winbase.h (28 bytes)
type dcb struct {
	DCBlength  uint32
	BaudRate   uint32
	Flags      uint32 // bitfield: fBinary=bit0, fDtrControl=bits4-5, fRtsControl=bits12-13
	wReserved  uint16
	XonLim     uint16
	XoffLim    uint16
	ByteSize   byte
	Parity     byte
	StopBits   byte
	XonChar    byte
	XoffChar   byte
	ErrorChar  byte
	EofChar    byte
	EvtChar    byte
	wReserved1 uint16
}

type commTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

const (
	clrRTS     = 4
	clrDTR     = 6
	purgeTXClr = 0x0004
	purgeRXClr = 0x0008
)

type winPort struct{ h syscall.Handle }

func openPort(name string) (Port, error) {
	path, err := syscall.UTF16PtrFromString(`\\.\` + name)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == 5 { // ERROR_ACCESS_DENIED
			return nil, fmt.Errorf("%s is in use by another program (close the Arduino IDE Serial Monitor or any other terminal)", name)
		}
		if errno, ok := err.(syscall.Errno); ok && errno == 2 {
			return nil, fmt.Errorf("%s not found (unplugged?)", name)
		}
		return nil, fmt.Errorf("open %s: %v", name, err)
	}
	procSetupComm.Call(uintptr(h), 4096, 4096)

	var d dcb
	d.DCBlength = uint32(unsafe.Sizeof(d))
	if r, _, e := procGetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("GetCommState %s: %v", name, e)
	}
	d.BaudRate = 115200
	d.ByteSize = 8
	d.Parity = 0   // NOPARITY
	d.StopBits = 0 // ONESTOPBIT
	// binary mode, no flow control, DTR and RTS held de-asserted so the
	// ESP32 auto-reset/boot circuit is left alone (both lines idle = run)
	d.Flags = 0x0001
	if r, _, e := procSetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("SetCommState %s: %v", name, e)
	}
	// return as soon as anything arrives, otherwise after 200 ms
	t := commTimeouts{0xFFFFFFFF, 0xFFFFFFFF, 200, 0, 2000}
	procSetCommTimeouts.Call(uintptr(h), uintptr(unsafe.Pointer(&t)))
	procEscapeCommFunction.Call(uintptr(h), clrDTR)
	procEscapeCommFunction.Call(uintptr(h), clrRTS)
	procPurgeComm.Call(uintptr(h), purgeRXClr|purgeTXClr)
	return &winPort{h: h}, nil
}

func (p *winPort) Read(b []byte) (int, error) {
	var n uint32
	if err := syscall.ReadFile(p.h, b, &n, nil); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (p *winPort) Write(b []byte) (int, error) {
	var n uint32
	if err := syscall.WriteFile(p.h, b, &n, nil); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (p *winPort) Close() error { return syscall.CloseHandle(p.h) }

// ---------------- registry helpers ----------------

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func regOpen(root syscall.Handle, path string) (syscall.Handle, bool) {
	var k syscall.Handle
	if err := syscall.RegOpenKeyEx(root, u16(path), 0, syscall.KEY_READ, &k); err != nil {
		return 0, false
	}
	return k, true
}

func regSubkeys(k syscall.Handle) []string {
	var out []string
	for i := uint32(0); ; i++ {
		buf := make([]uint16, 256)
		n := uint32(len(buf))
		if err := syscall.RegEnumKeyEx(k, i, &buf[0], &n, nil, nil, nil, nil); err != nil {
			break
		}
		out = append(out, syscall.UTF16ToString(buf[:n]))
	}
	return out
}

func regString(k syscall.Handle, name string) string {
	var typ uint32
	buf := make([]uint16, 512)
	n := uint32(len(buf) * 2)
	if err := syscall.RegQueryValueEx(k, u16(name), nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n); err != nil {
		return ""
	}
	if typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ {
		return ""
	}
	return syscall.UTF16ToString(buf[:n/2])
}

type usbInfo struct{ vid, pid, friendly string }

// COMx -> USB VID/PID, read from the device tree under Enum\USB
func usbPortMap() map[string]usbInfo {
	out := map[string]usbInfo{}
	root, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB`)
	if !ok {
		return out
	}
	defer syscall.RegCloseKey(root)
	for _, devKey := range regSubkeys(root) {
		up := strings.ToUpper(devKey)
		if !strings.HasPrefix(up, "VID_") {
			continue
		}
		vid, pid := "", ""
		for _, part := range strings.Split(up, "&") {
			if strings.HasPrefix(part, "VID_") {
				vid = strings.TrimPrefix(part, "VID_")
			} else if strings.HasPrefix(part, "PID_") {
				pid = strings.TrimPrefix(part, "PID_")
			}
		}
		dk, ok := regOpen(root, devKey)
		if !ok {
			continue
		}
		for _, inst := range regSubkeys(dk) {
			ik, ok := regOpen(dk, inst)
			if !ok {
				continue
			}
			friendly := regString(ik, "FriendlyName")
			if pk, ok := regOpen(ik, `Device Parameters`); ok {
				if pn := regString(pk, "PortName"); strings.HasPrefix(strings.ToUpper(pn), "COM") {
					out[strings.ToUpper(pn)] = usbInfo{vid, pid, friendly}
				}
				syscall.RegCloseKey(pk)
			}
			syscall.RegCloseKey(ik)
		}
		syscall.RegCloseKey(dk)
	}
	return out
}

// Ports that exist right now (HARDWARE\DEVICEMAP\SERIALCOMM only lists
// connected devices), annotated with USB details where available.
func listPorts() []PortInfo {
	present := map[string]string{}
	if k, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`); ok {
		for i := uint32(0); ; i++ {
			name := make([]uint16, 256)
			nameLen := uint32(len(name))
			data := make([]uint16, 128)
			dataLen := uint32(len(data) * 2)
			var typ uint32
			r, _, _ := procRegEnumValueW.Call(uintptr(k), uintptr(i),
				uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)), 0,
				uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&dataLen)))
			if r != 0 {
				break
			}
			com := strings.ToUpper(syscall.UTF16ToString(data[:dataLen/2]))
			present[com] = syscall.UTF16ToString(name[:nameLen])
		}
		syscall.RegCloseKey(k)
	}
	usb := usbPortMap()
	var out []PortInfo
	for com, dev := range present {
		pi := PortInfo{Name: com, Device: dev}
		if u, ok := usb[com]; ok {
			pi.VID, pi.PID = u.vid, u.pid
			pi.Desc = u.friendly
		}
		classify(&pi)
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Likely != out[j].Likely {
			return out[i].Likely
		}
		return comNum(out[i].Name) < comNum(out[j].Name)
	})
	return out
}

func comNum(s string) int {
	n := 0
	fmt.Sscanf(strings.TrimPrefix(strings.ToUpper(s), "COM"), "%d", &n)
	return n
}

// ---------------- app window ----------------

func openWindow(url string) {
	profile := filepath.Join(os.Getenv("LOCALAPPDATA"), "WatershedTankTool", "browser")
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			cmd := exec.Command(c, "--app="+url, "--window-size=1320,900",
				"--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check")
			if cmd.Start() == nil {
				return
			}
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func openFolder(path string) { exec.Command("explorer", path).Start() }

// open Explorer with the file selected
func revealFile(path string) {
	cmd := exec.Command("explorer")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer /select,"` + path + `"`}
	cmd.Start()
}

// run esptool without flashing up its own console window
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

// showMessage pops up a standard Windows message box (release builds have
// no console window to print to)
func showMessage(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x40) // MB_ICONINFORMATION
}

// ensureShortcuts adds Desktop and Start Menu shortcuts to this exe, once.
// Updates replace the exe in place, so the shortcuts stay valid.
func ensureShortcuts(a *App) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	base, err := os.UserConfigDir() // %AppData%
	if err != nil {
		return
	}
	dir := filepath.Join(base, "WatershedTankTool")
	flag := filepath.Join(dir, "shortcuts.txt")
	if b, err := os.ReadFile(flag); err == nil && strings.TrimSpace(string(b)) == exe {
		return // already done for this location (deleted ones stay deleted)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := `$ErrorActionPreference='Stop'; $w=New-Object -ComObject WScript.Shell; ` +
		`foreach($d in @([Environment]::GetFolderPath('Desktop'),[Environment]::GetFolderPath('Programs'))){ ` +
		`$s=$w.CreateShortcut((Join-Path $d 'Watershed Tank Tool.lnk')); $s.TargetPath=` + q(exe) + `; ` +
		`$s.WorkingDirectory=` + q(filepath.Dir(exe)) + `; $s.Description='Set up, test and update the Watershed tank node'; $s.Save() }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		a.sysLine("couldn't create shortcuts: %v %s", err, strings.TrimSpace(string(out)))
		return
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(flag, []byte(exe), 0o644)
	a.sysLine("created Desktop and Start Menu shortcuts to %s", exe)
}
