package main

// Session logs and support files, so a problem at Dad's end can be
// diagnosed from home:
//   - logs\session_<date>_<time>.txt : everything shown in the serial console
//     (node output, commands sent, connects/disconnects, update checks,
//     firmware installs with full esptool output), timestamped
//   - Save support bundle : one zip with recent session logs, recent tests,
//     node settings and versions
//   - Copy diagnostics    : a short plain-text summary for pasting into a message

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sessionMaxBytes = 10 << 20 // start a new part after 10 MB
	sessionKeep     = 30       // session files kept
)

type SessionLog struct {
	mu      sync.Mutex
	dir     string
	f       *os.File
	path    string
	size    int64
	part    int
	started time.Time
	header  string
}

func newSessionLog(dir, header string) *SessionLog {
	s := &SessionLog{dir: dir, started: time.Now(), header: header}
	s.mu.Lock()
	s.openLocked()
	s.mu.Unlock()
	go pruneFiles(dir, "session_", sessionKeep)
	return s
}

func (s *SessionLog) openLocked() {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	name := "session_" + s.started.Format("20060102_150405")
	if s.part > 0 {
		name += fmt.Sprintf("_part%d", s.part+1)
	}
	p := filepath.Join(s.dir, name+".txt")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	s.f, s.path, s.size = f, p, 0
	s.writeLocked(s.header)
}

func (s *SessionLog) writeLocked(text string) {
	if s.f == nil {
		return
	}
	n, _ := s.f.WriteString(text)
	s.size += int64(n)
}

// Line records one console line with a timestamp.
func (s *SessionLog) Line(line string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	if s.size > sessionMaxBytes {
		s.f.Close()
		s.part++
		s.openLocked()
	}
	s.writeLocked(time.Now().Format("2006-01-02 15:04:05.000  ") + line + "\r\n")
}

func (s *SessionLog) Path() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

func (s *SessionLog) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		s.f.WriteString(time.Now().Format("2006-01-02 15:04:05.000  ") + "# tool closed\r\n")
		s.f.Close()
		s.f = nil
	}
}

// keep the newest `keep` files starting with prefix
func pruneFiles(dir, prefix string, keep int) {
	files := recentFiles(dir, prefix, 0)
	for i, f := range files {
		if i >= keep {
			os.Remove(f)
		}
	}
}

// files in dir starting with prefix, newest first (limit 0 = all)
func recentFiles(dir, prefix string, limit int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type fi struct {
		p string
		t time.Time
	}
	var list []fi
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, fi{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.After(list[j].t) })
	var out []string
	for _, f := range list {
		out = append(out, f.p)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func sessionHeader() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("# Watershed Tank Tool v%s session log\r\n# started %s, %s/%s, computer %q\r\n# updates from: %s\r\n",
		appVersion, time.Now().Format("2006-01-02 15:04:05 -0700"), runtime.GOOS, runtime.GOARCH, host, orNone(updateRepo))
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ---------------- diagnostics text ----------------

func (a *App) diagnostics() string {
	var b strings.Builder
	w := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	w("Watershed diagnostics - %s", time.Now().Format("2006-01-02 15:04:05"))
	w("Tool: v%s (%s/%s)", appVersion, runtime.GOOS, runtime.GOARCH)

	u := a.updateState()
	if lat, ok := u["latest"].(map[string]any); ok {
		w("Latest release: %v (tool %v, firmware %v)", lat["tag"], lat["tool_version"], lat["fw_version"])
	}
	if e, _ := u["error"].(string); e != "" {
		w("Update check error: %s", e)
	}

	a.mu.Lock()
	switch {
	case a.connected && a.ident != nil:
		w("Node: connected on %s, node %v, firmware v%v, board %v", a.portName, a.ident["node_id"], a.ident["ver"], a.ident["board"])
	case a.connected && a.legacy:
		w("Node: connected on %s, OLD firmware v0.2 (no tool support)", a.portName)
	case a.connected:
		w("Node: port %s open, not identified as a Watershed node", a.portName)
	default:
		w("Node: not connected")
	}
	if a.cfg != nil {
		c := a.cfg
		w("Tank: %s  diam %.1f  len %.1f  width %.1f cm | empty %.1f  full %.1f cm | capacity %.0f L | LoRa TX %v, radio ok %v",
			c.Shape, c.DiamCm, c.LenCm, c.WidthCm, c.EmptyCm, c.FullCm, c.CapacityL, c.TxOn, c.RadioOK)
	}
	if a.last != nil {
		age := time.Since(a.lastAt).Round(time.Second)
		w("Last reading (%s ago): %s", age, string(a.last))
	}
	if a.flash != nil {
		f := a.flash.snapshot()
		switch {
		case f["running"] == true:
			w("Firmware install in progress: %v (%v)", f["target"], f["stage"])
		case f["ok"] == true:
			w("Last firmware install: %v - succeeded", f["target"])
		default:
			w("Last firmware install: %v - FAILED at: %v", f["target"], f["stage"])
		}
		if h, _ := f["hint"].(string); h != "" {
			w("  hint: %s", h)
		}
		if e, _ := f["error"].(string); e != "" {
			w("  error: %s", e)
		}
	}
	if a.test != nil && a.test.Result != nil {
		r := a.test.Result
		w("Last test: %s, %d readings, echo %.0f%%", r.Started, r.Samples, r.EchoPct)
		for _, v := range r.Verdicts {
			if v.Level != "ok" {
				w("  [%s] %s", strings.ToUpper(v.Level), v.Text)
			}
		}
	}
	// recent problems from the console
	var probs []string
	for _, l := range a.lines {
		low := strings.ToLower(l)
		if strings.HasPrefix(l, "@ERR") || (strings.HasPrefix(l, "#") && (strings.Contains(low, "fail") || strings.Contains(low, "error") || strings.Contains(low, "lost") || strings.Contains(low, "refus"))) ||
			strings.Contains(low, "fatal error") {
			probs = append(probs, l)
		}
	}
	a.mu.Unlock()

	ports := listPorts()
	var pl []string
	for _, p := range ports {
		pl = append(pl, fmt.Sprintf("%s (%s)", p.Name, p.Desc))
	}
	if len(pl) == 0 {
		pl = []string{"none"}
	}
	w("Serial ports: %s", strings.Join(pl, ", "))
	if len(probs) > 0 {
		if len(probs) > 12 {
			probs = probs[len(probs)-12:]
		}
		w("Recent problems:")
		for _, p := range probs {
			w("  %s", p)
		}
	} else {
		w("Recent problems: none")
	}
	if p := a.slog.Path(); p != "" {
		w("Session log: %s", p)
	}
	return b.String()
}

// ---------------- support bundle ----------------

func (a *App) supportBundle() (string, error) {
	if err := os.MkdirAll(a.logDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(a.logDir, "support_"+time.Now().Format("20060102_150405")+".zip")
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(out)
	add := func(name string, r io.Reader) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()}
		if fw, err := zw.CreateHeader(h); err == nil {
			io.Copy(fw, r)
		}
	}
	addFile := func(dir, p string) {
		f, err := os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		add(dir+filepath.Base(p), f)
	}

	add("diagnostics.txt", strings.NewReader(strings.ReplaceAll(a.diagnostics(), "\n", "\r\n")))

	a.mu.Lock()
	extra := map[string]any{"status": a.statusLocked(), "config": a.cfgRaw, "last_reading": a.last}
	if a.flash != nil {
		extra["last_flash"] = a.flash.snapshot()
	}
	console := strings.Join(a.lines, "\r\n")
	a.mu.Unlock()
	extra["update"] = a.updateState()
	extra["ports"] = listPorts()
	js, _ := json.MarshalIndent(extra, "", "  ")
	add("state.json", strings.NewReader(string(js)))
	add("console_now.txt", strings.NewReader(console))

	for _, p := range recentFiles(a.logDir, "session_", 5) {
		addFile("sessions/", p)
	}
	for _, p := range recentFiles(a.logDir, "tanktest_", 6) { // ~3 tests: csv + summary each
		addFile("tests/", p)
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	a.sysLine("support bundle saved: %s", path)
	return path, nil
}
