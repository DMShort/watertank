package main

// Firmware flashing with Espressif's esptool.
//
// Release builds flash four parts at their own addresses:
//   0x1000 bootloader  0x8000 partition table  0xe000 boot_app0  0x10000 app
// Only those sectors are written, so the NVS area (0x9000-0xDFFF) that
// holds node ID, tank shape and calibration is kept across updates.
// A single "merged" image is never used because it would wipe that area.
//
// A test build (.bin from Arduino "Export Compiled Binary") is flashed
// to 0x10000 only.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type flashPart struct {
	Offset uint32
	Path   string
	Size   int64
	Label  string
}

type FlashJob struct {
	Running bool     `json:"running"`
	Port    string   `json:"port"`
	Target  string   `json:"target"`
	Stage   string   `json:"stage"`
	Pct     float64  `json:"pct"`
	Done    bool     `json:"done"`
	OK      bool     `json:"ok"`
	Err     string   `json:"error,omitempty"`
	Hint    string   `json:"hint,omitempty"`
	Tail    []string `json:"tail"`
	mu      sync.Mutex
}

func (j *FlashJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{"running": j.Running, "port": j.Port, "target": j.Target, "stage": j.Stage,
		"pct": j.Pct, "done": j.Done, "ok": j.OK, "error": j.Err, "hint": j.Hint, "tail": append([]string(nil), j.Tail...)}
}

const appOffset = 0x10000
const maxAppSize = 0x330000 // app0 in the Heltec V2 default_8MB partition table

// esptool: next to the exe if present, else from the latest release
func (a *App) findEsptool() (string, error) {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, n := range []string{"esptool.exe", "esptool"} {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	a.upd.mu.Lock()
	m := a.upd.latest
	a.upd.mu.Unlock()
	if m == nil && a.upd.enabled() {
		m, _ = a.upd.Check()
	}
	if m == nil || m.Esptool.File == "" {
		return "", fmt.Errorf("the flashing tool (esptool) isn't downloaded yet - connect to the internet and press 'Check for updates', or put esptool.exe next to WatershedTankTool.exe")
	}
	a.setBusy("Downloading esptool…")
	defer a.setBusy("")
	return a.upd.Fetch(m.Esptool.File, m.Esptool.SHA256)
}

// flashPort picks the port to flash: the connected one, else the given one
func (a *App) flashPort(requested string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.portName != "" {
		return a.portName, nil
	}
	if requested != "" {
		return requested, nil
	}
	return "", fmt.Errorf("plug the node in first (or pick its COM port at the top)")
}

func (a *App) beginFlash(port, target string) (*FlashJob, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.flash != nil && a.flash.Running {
		return nil, fmt.Errorf("already flashing")
	}
	if a.test != nil && !a.test.Done {
		return nil, fmt.Errorf("stop the running test first")
	}
	j := &FlashJob{Running: true, Port: port, Target: target, Stage: "Preparing…"}
	a.flash = j
	a.auto = false // keep auto-detect off the port while esptool has it
	return j, nil
}

// flash the latest release's firmware
func (a *App) flashRelease(requestedPort string, allowOtherBoard bool) error {
	a.upd.mu.Lock()
	m := a.upd.latest
	a.upd.mu.Unlock()
	if m == nil {
		var err error
		if m, err = a.upd.Check(); err != nil {
			return err
		}
	}
	if len(m.Firmware.Parts) == 0 {
		return fmt.Errorf("release %s has no firmware", m.Tag)
	}
	a.mu.Lock()
	var board string
	if a.ident != nil {
		board, _ = a.ident["board"].(string)
	}
	a.mu.Unlock()
	if board != "" && m.Firmware.Board != "" && board != m.Firmware.Board && !allowOtherBoard {
		return fmt.Errorf("this node is a %s but the firmware is for %s", board, m.Firmware.Board)
	}
	port, err := a.flashPort(requestedPort)
	if err != nil {
		return err
	}
	j, err := a.beginFlash(port, "firmware "+m.Firmware.Version)
	if err != nil {
		return err
	}
	go func() {
		fail := func(err error) { a.endFlash(j, err) }
		j.setStage("Downloading firmware " + m.Firmware.Version + "…")
		esptool, err := a.findEsptool()
		if err != nil {
			fail(err)
			return
		}
		var parts []flashPart
		for _, p := range m.Firmware.Parts {
			path, err := a.upd.Fetch(p.File, p.SHA256)
			if err != nil {
				fail(err)
				return
			}
			off, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(p.Offset), "0x"), 16, 32)
			if err != nil {
				fail(fmt.Errorf("bad offset %q in manifest", p.Offset))
				return
			}
			st, _ := os.Stat(path)
			parts = append(parts, flashPart{uint32(off), path, st.Size(), p.File})
		}
		if bad := checkParts(parts); bad != "" {
			fail(fmt.Errorf("%s", bad))
			return
		}
		a.runFlash(j, esptool, parts)
	}()
	return nil
}

// flash a test build app image
func (a *App) flashUpload(requestedPort, name string, data []byte) error {
	if len(data) < 256 || data[0] != 0xE9 {
		return fmt.Errorf("%s isn't an ESP32 app image. Use the tank_node.ino.bin from Arduino 'Export Compiled Binary', not the bootloader, partitions or merged file", name)
	}
	if len(data) > maxAppSize {
		return fmt.Errorf("%s is %d KB, too big for the app partition", name, len(data)/1024)
	}
	if bytes.HasSuffix([]byte(strings.ToLower(name)), []byte(".merged.bin")) {
		return fmt.Errorf("merged images would erase the node's settings - use tank_node.ino.bin instead")
	}
	port, err := a.flashPort(requestedPort)
	if err != nil {
		return err
	}
	dir := filepath.Join(a.upd.cacheDir, "uploads")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "test_build_app.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	j, err := a.beginFlash(port, "test build "+name)
	if err != nil {
		return err
	}
	go func() {
		esptool, err := a.findEsptool()
		if err != nil {
			a.endFlash(j, err)
			return
		}
		a.runFlash(j, esptool, []flashPart{{appOffset, path, int64(len(data)), name}})
	}()
	return nil
}

func checkParts(parts []flashPart) string {
	for _, p := range parts {
		end := int64(p.Offset) + p.Size
		// never touch the settings (NVS) area
		if int64(p.Offset) < 0xE000 && end > 0x9000 {
			return fmt.Sprintf("refusing to flash %s: it would overwrite the node's saved settings (0x9000-0xE000)", p.Label)
		}
	}
	return ""
}

func (j *FlashJob) setStage(s string) {
	j.mu.Lock()
	j.Stage = s
	j.mu.Unlock()
}

var (
	reWriting = regexp.MustCompile(`Writing at 0x([0-9a-fA-F]+)\.*\s*\((\d+)\s*%\)`)
)

func (a *App) runFlash(j *FlashJob, esptool string, parts []flashPart) {
	port := j.Port
	a.disconnect()
	time.Sleep(400 * time.Millisecond)
	var total int64
	for _, p := range parts {
		total += p.Size
	}
	var lastErr error
	for attempt, baud := range []int{921600, 460800, 115200} {
		args := []string{"--chip", "esp32", "--port", port, "--baud", strconv.Itoa(baud),
			"--before", "default_reset", "--after", "hard_reset", "write_flash", "-z"}
		for _, p := range parts {
			args = append(args, fmt.Sprintf("0x%x", p.Offset), p.Path)
		}
		if attempt > 0 {
			a.sysLine("retrying at %d baud", baud)
		}
		out, err := a.runEsptool(j, esptool, args, parts, total)
		if err == nil {
			a.endFlash(j, nil)
			return
		}
		lastErr = err
		low := strings.ToLower(out)
		// these won't be fixed by a slower speed
		if strings.Contains(low, "could not open port") || strings.Contains(low, "failed to connect") ||
			strings.Contains(low, "no serial data received") || strings.Contains(low, "permissionerror") {
			j.mu.Lock()
			j.Hint = flashHint(low)
			j.mu.Unlock()
			break
		}
	}
	a.endFlash(j, lastErr)
}

func flashHint(out string) string {
	switch {
	case strings.Contains(out, "could not open port") || strings.Contains(out, "permissionerror"):
		return "The COM port is busy. Close the Arduino IDE / Serial Monitor and try again."
	case strings.Contains(out, "failed to connect") || strings.Contains(out, "no serial data received"):
		return "The board didn't enter download mode. Hold the PRG button on the Heltec, click Install again, and let go when 'Connecting' appears. Also try another USB cable."
	}
	return ""
}

func (a *App) runEsptool(j *FlashJob, exe string, args []string, parts []flashPart, total int64) (string, error) {
	a.sysLine("esptool %s", strings.Join(args, " "))
	cmd := exec.Command(exe, args...)
	hideWindow(cmd)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	j.setStage("Connecting to the board…")
	a.hub.send("flash", j.snapshot())
	if err := cmd.Start(); err != nil {
		pw.Close()
		return "", fmt.Errorf("can't run esptool: %v", err)
	}
	var all strings.Builder
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(scanCRLF)
		lastSend := time.Time{}
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			all.WriteString(line + "\n")
			a.mu.Lock()
			a.logLine("  " + line)
			a.mu.Unlock()
			j.mu.Lock()
			j.Tail = append(j.Tail, line)
			if len(j.Tail) > 8 {
				j.Tail = j.Tail[len(j.Tail)-8:]
			}
			low := strings.ToLower(line)
			switch {
			case strings.HasPrefix(low, "connecting"):
				j.Stage = "Connecting to the board…"
			case strings.HasPrefix(low, "chip is"):
				j.Stage = "Connected: " + line
			case strings.Contains(low, "erasing") || strings.HasPrefix(low, "compressed"):
				j.Stage = "Preparing flash…"
			case strings.HasPrefix(low, "hash of data verified"):
				j.Stage = "Verified ✓"
			case strings.HasPrefix(low, "hard resetting"):
				j.Stage = "Restarting the node…"
			}
			if m := reWriting.FindStringSubmatch(line); m != nil {
				addr, _ := strconv.ParseUint(m[1], 16, 32)
				pct, _ := strconv.Atoi(m[2])
				var before int64
				var cur *flashPart
				for i := range parts {
					if uint64(parts[i].Offset) <= addr {
						cur = &parts[i]
					}
				}
				for i := range parts {
					if cur != nil && parts[i].Offset < cur.Offset {
						before += parts[i].Size
					}
				}
				if cur != nil && total > 0 {
					j.Pct = 100 * (float64(before) + float64(cur.Size)*float64(pct)/100) / float64(total)
					j.Stage = fmt.Sprintf("Writing %s… %d%%", cur.Label, pct)
				}
			}
			j.mu.Unlock()
			if time.Since(lastSend) > 150*time.Millisecond {
				a.hub.send("flash", j.snapshot())
				lastSend = time.Now()
			}
		}
		close(done)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil {
		return all.String(), fmt.Errorf("esptool failed: %v", lastLines(all.String(), 2))
	}
	return all.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// split on \n or \r (esptool overwrites progress lines with \r on a terminal)
func scanCRLF(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (a *App) endFlash(j *FlashJob, err error) {
	j.mu.Lock()
	j.Running, j.Done = false, true
	if err != nil {
		j.OK, j.Err = false, err.Error()
		if j.Hint == "" {
			j.Hint = flashHint(strings.ToLower(err.Error()))
		}
		j.Stage = "Failed"
	} else {
		j.OK, j.Pct, j.Stage = true, 100, "Done - waiting for the node to restart…"
	}
	j.mu.Unlock()
	a.hub.send("flash", j.snapshot())
	if err != nil {
		a.sysLine("flash failed: %v", err)
	} else {
		a.sysLine("flash complete")
	}
	time.Sleep(1500 * time.Millisecond)
	a.mu.Lock()
	a.auto = true
	a.failed = map[string]bool{}
	a.mu.Unlock()
	a.pushStatus()
}
