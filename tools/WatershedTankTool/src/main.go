// Watershed Tank Tool
// --------------------
// Windows utility for setting up and testing a Watershed tank node over USB:
//   - finds the node automatically when it is plugged in
//   - sets node ID, tank shape and dimensions, calibration, LoRa TX on/off
//   - shows live level / volume / temperature / supply readings
//   - runs a timed local test at the tank and saves a CSV + summary
//
// The UI is a local page (127.0.0.1 only) shown in an Edge/Chrome app
// window. Talks to firmware tank_node.ino v0.3+ using its "@" lines.
package main

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appName     = "Watershed Tank Tool"
	fwID        = "watershed-node"
	minFW       = "0.3"
	defaultPort = 47615
)

//go:embed web/index.html
var indexHTML []byte

type Port interface {
	Read([]byte) (int, error) // returns (0, nil) on timeout
	Write([]byte) (int, error)
	Close() error
}

type PortInfo struct {
	Name   string `json:"name"`
	Device string `json:"device,omitempty"`
	VID    string `json:"vid,omitempty"`
	PID    string `json:"pid,omitempty"`
	Desc   string `json:"desc"`
	Chip   string `json:"chip,omitempty"`
	Likely bool   `json:"likely"`
}

// classify marks ports that look like an ESP32 board's USB-serial chip
func classify(p *PortInfo) {
	dev := strings.ToLower(p.Device)
	switch {
	case p.VID == "10C4" && (p.PID == "EA60" || p.PID == "EA70"):
		p.Chip, p.Likely = "Silicon Labs CP210x (Heltec WiFi LoRa 32)", true
	case p.VID == "1A86":
		p.Chip, p.Likely = "WCH CH340/CH9102 (ESP32 board)", true
	case p.VID == "303A":
		p.Chip, p.Likely = "Espressif native USB (ESP32-S3/C3)", true
	case p.VID == "0403":
		p.Chip, p.Likely = "FTDI USB-serial", true
	case strings.Contains(dev, "silabser"):
		p.Chip, p.Likely = "Silicon Labs CP210x", true
	case strings.Contains(dev, "ch341") || strings.Contains(dev, "wchusb"):
		p.Chip, p.Likely = "WCH CH340", true
	case strings.Contains(dev, "usbser"):
		p.Chip, p.Likely = "USB serial", true
	}
	if p.Desc == "" {
		if p.Chip != "" {
			p.Desc = p.Chip
		} else {
			p.Desc = "serial port"
		}
	}
}

// ---------------- event hub (Server-Sent Events) ----------------

type Hub struct {
	mu         sync.Mutex
	clients    map[chan []byte]struct{}
	lastSeen   time.Time
	everClient bool
}

func (h *Hub) send(event string, v any) {
	b, _ := json.Marshal(v)
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- msg:
		default: // slow client: drop rather than block the serial reader
		}
	}
}

// ---------------- node data ----------------

type Reading struct {
	Seq        int      `json:"seq"`
	DistanceCm *float64 `json:"distance_cm"`
	HeightCm   *float64 `json:"height_cm"`
	LevelPct   *float64 `json:"level_pct"`
	VolumeL    *float64 `json:"volume_l"`
	CapacityL  *float64 `json:"capacity_l"`
	TempC      *float64 `json:"temp_c"`
	BattMv     *float64 `json:"batt_mv"`
	DS18B20    int      `json:"ds18b20_count"`
	TX         string   `json:"tx"`
	UptimeS    int      `json:"uptime_s"`
}

type Config struct {
	NodeID    int     `json:"node_id"`
	Shape     string  `json:"shape"`
	DiamCm    float64 `json:"diam_cm"`
	LenCm     float64 `json:"len_cm"`
	WidthCm   float64 `json:"width_cm"`
	EmptyCm   float64 `json:"empty_cm"`
	FullCm    float64 `json:"full_cm"`
	CapacityL float64 `json:"capacity_l"`
	FreqMhz   float64 `json:"freq_mhz"`
	SF        int     `json:"sf"`
	BW        float64 `json:"bw"`
	CR        int     `json:"cr"`
	TxOn      bool    `json:"tx_on"`
	RadioOK   bool    `json:"radio_ok"`
}

// same maths as the firmware, used for the test's reference comparison
func volumeAt(c Config, h float64) float64 {
	if h <= 0 {
		return 0
	}
	switch c.Shape {
	case "HORIZ":
		r := c.DiamCm / 2
		if h > c.DiamCm {
			h = c.DiamCm
		}
		a := r*r*math.Acos((r-h)/r) - (r-h)*math.Sqrt(2*r*h-h*h)
		return a * c.LenCm / 1000
	case "RECT":
		return c.LenCm * c.WidthCm * h / 1000
	default:
		r := c.DiamCm / 2
		return math.Pi * r * r * h / 1000
	}
}

// ---------------- local test ----------------

type Sample struct {
	T time.Time
	R Reading
}

type Test struct {
	Label     string
	Start     time.Time
	Duration  time.Duration
	RefHeight *float64
	Cfg       *Config
	Samples   []Sample
	Done      bool
	Result    *TestResult
}

type Stat struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	SD   float64 `json:"sd"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

func stat(v []float64) *Stat {
	if len(v) == 0 {
		return nil
	}
	s := &Stat{N: len(v), Min: v[0], Max: v[0]}
	for _, x := range v {
		s.Mean += x
		s.Min = math.Min(s.Min, x)
		s.Max = math.Max(s.Max, x)
	}
	s.Mean /= float64(len(v))
	for _, x := range v {
		s.SD += (x - s.Mean) * (x - s.Mean)
	}
	if len(v) > 1 {
		s.SD = math.Sqrt(s.SD / float64(len(v)-1))
	} else {
		s.SD = 0
	}
	return s
}

type TestResult struct {
	Label      string         `json:"label"`
	Started    string         `json:"started"`
	Seconds    int            `json:"seconds"`
	Samples    int            `json:"samples"`
	EchoOK     int            `json:"echo_ok"`
	EchoPct    float64        `json:"echo_pct"`
	Distance   *Stat          `json:"distance_cm"`
	Height     *Stat          `json:"height_cm"`
	Volume     *Stat          `json:"volume_l"`
	Level      *Stat          `json:"level_pct"`
	Temp       *Stat          `json:"temp_c"`
	Supply     *Stat          `json:"batt_mv"`
	TX         map[string]int `json:"tx"`
	RefHeight  *float64       `json:"ref_height_cm,omitempty"`
	RefErrCm   *float64       `json:"ref_err_cm,omitempty"`
	RefErrL    *float64       `json:"ref_err_l,omitempty"`
	Verdicts   []Verdict      `json:"verdicts"`
	CSVPath    string         `json:"csv_path"`
	ReportPath string         `json:"report_path"`
}

type Verdict struct {
	Level string `json:"level"` // ok | warn | fail
	Text  string `json:"text"`
}

func f(p *float64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

func (t *Test) analyse() *TestResult {
	r := &TestResult{Label: t.Label, Started: t.Start.Format("2006-01-02 15:04:05"),
		Seconds: int(time.Since(t.Start).Seconds()), Samples: len(t.Samples), TX: map[string]int{}}
	var dist, hgt, vol, lvl, tmp, sup []float64
	ds0 := 0
	for _, s := range t.Samples {
		if v, ok := f(s.R.DistanceCm); ok {
			r.EchoOK++
			dist = append(dist, v)
		}
		if v, ok := f(s.R.HeightCm); ok {
			hgt = append(hgt, v)
		}
		if v, ok := f(s.R.VolumeL); ok {
			vol = append(vol, v)
		}
		if v, ok := f(s.R.LevelPct); ok {
			lvl = append(lvl, v)
		}
		if v, ok := f(s.R.TempC); ok {
			tmp = append(tmp, v)
		}
		if v, ok := f(s.R.BattMv); ok {
			sup = append(sup, v)
		}
		if s.R.DS18B20 == 0 {
			ds0++
		}
		r.TX[s.R.TX]++
	}
	if r.Samples > 0 {
		r.EchoPct = 100 * float64(r.EchoOK) / float64(r.Samples)
	}
	r.Distance, r.Height, r.Volume, r.Level, r.Temp, r.Supply = stat(dist), stat(hgt), stat(vol), stat(lvl), stat(tmp), stat(sup)

	add := func(level, format string, a ...any) {
		r.Verdicts = append(r.Verdicts, Verdict{level, fmt.Sprintf(format, a...)})
	}
	switch {
	case r.Samples < 5:
		add("fail", "Only %d readings recorded - run the test for longer.", r.Samples)
	case r.EchoPct >= 98:
		add("ok", "Ultrasonic echo reliable: %d of %d readings (%.0f%%).", r.EchoOK, r.Samples, r.EchoPct)
	case r.EchoPct >= 90:
		add("warn", "Some missed echoes: %.0f%% success. Check the sensor is square to the water and clear of the tank wall, inlet and ladder.", r.EchoPct)
	default:
		add("fail", "Echo unreliable: only %.0f%% success. Check wiring, aim, condensation on the transducer face, and that the water is more than 25 cm below the sensor.", r.EchoPct)
	}
	if r.Distance != nil && r.Distance.N > 2 {
		switch {
		case r.Distance.SD <= 0.5:
			add("ok", "Readings steady: spread (SD) %.2f cm.", r.Distance.SD)
		case r.Distance.SD <= 2:
			add("warn", "Readings a little noisy: SD %.2f cm. Fine if the tank was filling or rippling; otherwise check the beam path.", r.Distance.SD)
		default:
			add("fail", "Readings noisy: SD %.2f cm (range %.1f-%.1f cm). Likely echoes off the wall/inlet, or water moving.", r.Distance.SD, r.Distance.Min, r.Distance.Max)
		}
	}
	if t.Cfg != nil {
		c := t.Cfg
		if c.EmptyCm <= c.FullCm {
			add("fail", "Calibration invalid: empty (%.1f cm) must be a bigger distance than full (%.1f cm).", c.EmptyCm, c.FullCm)
		}
		if c.FullCm < 22 {
			add("warn", "Full point is %.0f cm from the sensor - the JSN-SR04T can't see closer than ~20-25 cm, so a full tank may read as no echo.", c.FullCm)
		}
		if r.Distance != nil && r.Distance.Mean > c.EmptyCm+2 {
			add("warn", "Measured distance (%.1f cm) is past the empty point (%.1f cm) - check CAL EMPTY.", r.Distance.Mean, c.EmptyCm)
		}
		if t.RefHeight != nil && r.Height != nil {
			e := r.Height.Mean - *t.RefHeight
			el := volumeAt(*c, r.Height.Mean) - volumeAt(*c, *t.RefHeight)
			r.RefHeight, r.RefErrCm, r.RefErrL = t.RefHeight, &e, &el
			switch {
			case math.Abs(e) <= 1.5:
				add("ok", "Matches your dipstick: %+.1f cm (%+.0f L).", e, el)
			case math.Abs(e) <= 4:
				add("warn", "Off your dipstick by %+.1f cm (%+.0f L). Re-check the empty distance, or a hot headspace can read a few cm short.", e, el)
			default:
				add("fail", "Off your dipstick by %+.1f cm (%+.0f L). CAL EMPTY is probably wrong: set it to sensor-to-floor distance. Expect an empty value near %.1f cm.", e, el, c.EmptyCm-e)
			}
		}
	}
	if r.Temp == nil {
		add("warn", "No temperature readings - DS18B20 not detected (check the 4.7 kΩ pull-up and data pin).")
	} else if r.Temp.Max >= 84.9 && r.Temp.Max <= 85.1 {
		add("warn", "Temperature read 85.0 °C - that is the DS18B20 power-on value, so the sensor is resetting. Check wiring / pin clash on GPIO4.")
	} else {
		add("ok", "Temperature %.1f °C (range %.1f-%.1f).", r.Temp.Mean, r.Temp.Min, r.Temp.Max)
	}
	if r.Supply != nil {
		switch {
		case r.Supply.Min < 3000:
			add("warn", "Supply/battery reading low (min %.0f mV). If the sense divider isn't fitted this is expected - ignore it.", r.Supply.Min)
		case r.Supply.Max > 4300 && r.Supply.Min < 4500:
			add("warn", "USB supply dipped to %.0f mV - try a shorter/thicker USB lead or a better plug pack.", r.Supply.Min)
		default:
			add("ok", "Supply steady: %.0f-%.0f mV.", r.Supply.Min, r.Supply.Max)
		}
	}
	if n := r.TX["ERR"]; n > 0 {
		add("warn", "LoRa transmit errors on %d readings.", n)
	}
	return r
}

// ---------------- app ----------------

type App struct {
	mu        sync.Mutex
	hub       *Hub
	port      Port
	portName  string
	gen       int
	connected bool
	ident     map[string]any
	legacy    bool
	cfg       *Config
	cfgRaw    json.RawMessage
	last      json.RawMessage
	lastAt    time.Time
	lines     []string
	failed    map[string]bool
	auto      bool
	test      *Test
	logDir    string
	probing   bool
	upd       *Updater
	flash     *FlashJob
	srv       *http.Server
	slog      *SessionLog
	// brings the tool's own window to the front (nil in browser mode)
	showWindow func()
	url        string
}

func (a *App) status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked()
}

func (a *App) statusLocked() map[string]any {
	st := map[string]any{
		"connected": a.connected, "port": a.portName, "ident": a.ident,
		"legacy": a.legacy, "auto": a.auto, "probing": a.probing, "min_fw": minFW,
		"app_version": appVersion, "log_dir": a.logDir,
		"flashing":    a.flash != nil && a.flash.Running,
		"session_log": a.slog.Path(),
	}
	return st
}

func (a *App) pushStatus() { a.hub.send("status", a.status()) }

func (a *App) logLine(s string) {
	a.slog.Line(s)
	a.lines = append(a.lines, s)
	if len(a.lines) > 600 {
		a.lines = a.lines[len(a.lines)-500:]
	}
	a.hub.send("line", s)
}

func (a *App) sysLine(format string, v ...any) {
	a.mu.Lock()
	a.logLine("# " + fmt.Sprintf(format, v...))
	a.mu.Unlock()
}

func (a *App) handleLine(gen int, line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return
	}
	a.logLine(line)
	switch {
	case strings.HasPrefix(line, "@ID "):
		var m map[string]any
		if json.Unmarshal([]byte(line[4:]), &m) == nil && m["fw"] == fwID {
			a.ident = m
			a.legacy = false
			a.hub.send("status", a.statusLocked())
		}
	case strings.HasPrefix(line, "@CFG "):
		var c Config
		raw := json.RawMessage(line[5:])
		if json.Unmarshal(raw, &c) == nil {
			a.cfg, a.cfgRaw = &c, raw
			a.hub.send("cfg", raw)
			if a.ident != nil {
				if id, _ := a.ident["node_id"].(float64); int(id) != c.NodeID {
					a.ident["node_id"] = float64(c.NodeID)
					a.hub.send("status", a.statusLocked())
				}
			}
		}
	case strings.HasPrefix(line, "@R "):
		raw := json.RawMessage(line[3:])
		var r Reading
		if json.Unmarshal(raw, &r) == nil {
			a.last, a.lastAt = raw, time.Now()
			a.hub.send("reading", raw)
			if a.test != nil && !a.test.Done {
				a.test.Samples = append(a.test.Samples, Sample{time.Now(), r})
			}
		}
	case strings.HasPrefix(line, "@ERR "):
		a.hub.send("nodeerror", line[5:])
	default:
		// v0.2 firmware prints these but never answers "ID"
		if a.ident == nil && (strings.Contains(line, "ds18b20_count=") || strings.Contains(line, "lora_tx seq=")) {
			if !a.legacy {
				a.legacy = true
				a.hub.send("status", a.statusLocked())
			}
		}
	}
}

func (a *App) readLoop(p Port, gen int) {
	buf := make([]byte, 1024)
	var acc []byte
	for {
		n, err := p.Read(buf)
		a.mu.Lock()
		stale := gen != a.gen
		a.mu.Unlock()
		if stale {
			return
		}
		if err != nil {
			a.lost(gen, err)
			return
		}
		if n == 0 {
			continue
		}
		acc = append(acc, buf[:n]...)
		for {
			i := bytes.IndexByte(acc, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimRight(string(acc[:i]), "\r")
			acc = acc[i+1:]
			if line != "" {
				a.handleLine(gen, line)
			}
		}
		if len(acc) > 8192 {
			acc = acc[:0]
		}
	}
}

func (a *App) lost(gen int, err error) {
	a.mu.Lock()
	if gen != a.gen {
		a.mu.Unlock()
		return
	}
	a.gen++
	if a.port != nil {
		a.port.Close()
	}
	name := a.portName
	a.port, a.connected, a.ident, a.legacy, a.portName = nil, false, nil, false, ""
	a.auto = true // look for it again when it's plugged back in
	a.logLine(fmt.Sprintf("# lost %s: %v", name, err))
	a.mu.Unlock()
	a.pushStatus()
}

func (a *App) write(cmd string) error {
	a.mu.Lock()
	p := a.port
	a.mu.Unlock()
	if p == nil {
		return fmt.Errorf("not connected")
	}
	a.sysLine("> %s", cmd)
	_, err := p.Write([]byte(cmd + "\n"))
	return err
}

func (a *App) disconnect() {
	a.mu.Lock()
	a.gen++
	if a.port != nil {
		a.port.Close()
		a.logLine("# disconnected " + a.portName)
	}
	a.port, a.connected, a.ident, a.legacy, a.portName = nil, false, nil, false, ""
	a.mu.Unlock()
	a.pushStatus()
}

// connect opens a port, starts reading, and asks the node to identify.
// keepUnknown=false (auto-detect) closes the port again if nothing answers.
func (a *App) connect(name string, keepUnknown bool) (bool, error) {
	a.disconnect()
	p, err := openPort(name)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.gen++
	gen := a.gen
	a.port, a.portName, a.connected = p, name, true
	a.probing = true
	a.cfg, a.cfgRaw, a.last = nil, nil, nil // don't show the previous board's data
	a.hub.send("cleared", true)
	a.logLine("# opened " + name + " at 115200")
	a.mu.Unlock()
	a.pushStatus()
	go a.readLoop(p, gen)

	// the board may reset when the port opens; firmware announces itself on
	// boot, and we also ask a few times in case it didn't reset
	deadline := time.Now().Add(5 * time.Second)
	nextAsk := time.Now()
	for time.Now().Before(deadline) {
		if time.Now().After(nextAsk) {
			p.Write([]byte("\nID\n"))
			nextAsk = time.Now().Add(1500 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		a.mu.Lock()
		ok, legacy, still := a.ident != nil, a.legacy, gen == a.gen
		a.mu.Unlock()
		if !still {
			return false, fmt.Errorf("connection to %s lost", name)
		}
		if ok {
			a.mu.Lock()
			a.probing = false
			a.mu.Unlock()
			a.pushStatus()
			a.write("CAL SHOW")
			return true, nil
		}
		if legacy && time.Now().After(deadline.Add(-2*time.Second)) {
			break
		}
	}
	a.mu.Lock()
	a.probing = false
	legacy := a.legacy
	a.mu.Unlock()
	if legacy {
		a.pushStatus()
		return true, nil // keep: old firmware, user needs to flash v0.3
	}
	if !keepUnknown {
		a.disconnect()
		return false, nil
	}
	a.pushStatus()
	return false, nil
}

// watcher: auto-connects when a node is plugged in, and exits the program
// once the window has been closed for a while
func (a *App) watch() {
	for {
		time.Sleep(2 * time.Second)
		a.hub.mu.Lock()
		idle := a.hub.everClient && len(a.hub.clients) == 0 && time.Since(a.hub.lastSeen) > 20*time.Second &&
			os.Getenv("WATERSHED_NO_AUTOEXIT") == ""
		a.hub.mu.Unlock()
		if idle {
			a.disconnect()
			a.slog.Close()
			os.Exit(0)
		}
		a.mu.Lock()
		busy := a.connected || !a.auto || a.probing
		a.mu.Unlock()
		if busy {
			continue
		}
		ports := listPorts()
		seen := map[string]bool{}
		for _, p := range ports {
			seen[p.Name] = true
		}
		a.mu.Lock()
		for k := range a.failed { // forget ports that were unplugged
			if !seen[k] {
				delete(a.failed, k)
			}
		}
		a.mu.Unlock()
		for _, p := range ports {
			a.mu.Lock()
			skip := a.failed[p.Name] || !p.Likely || !a.auto || a.connected
			a.mu.Unlock()
			if skip {
				continue
			}
			a.sysLine("probing %s (%s)", p.Name, p.Desc)
			ok, err := a.connect(p.Name, false)
			a.mu.Lock()
			legacy := a.legacy
			a.mu.Unlock()
			if ok || legacy {
				break
			}
			if err != nil {
				a.sysLine("%s: %v", p.Name, err)
			} else {
				a.sysLine("%s: no Watershed node answered", p.Name)
			}
			a.mu.Lock()
			a.failed[p.Name] = true
			a.mu.Unlock()
		}
	}
}

// ---------------- test control ----------------

func (a *App) startTest(label string, dur time.Duration, ref *float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.connected || a.ident == nil {
		return fmt.Errorf("connect a node running firmware v%s first", minFW)
	}
	if a.test != nil && !a.test.Done {
		return fmt.Errorf("a test is already running")
	}
	var cfg *Config
	if a.cfg != nil {
		c := *a.cfg
		cfg = &c
	}
	a.test = &Test{Label: label, Start: time.Now(), Duration: dur, RefHeight: ref, Cfg: cfg}
	t := a.test
	go func() {
		for {
			time.Sleep(time.Second)
			a.mu.Lock()
			if a.test != t || t.Done {
				a.mu.Unlock()
				return
			}
			el := time.Since(t.Start)
			n := len(t.Samples)
			a.mu.Unlock()
			a.hub.send("test", map[string]any{"running": true, "elapsed": int(el.Seconds()), "total": int(t.Duration.Seconds()), "samples": n, "label": t.Label})
			if el >= t.Duration {
				a.finishTest()
				return
			}
		}
	}()
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (a *App) finishTest() {
	a.mu.Lock()
	t := a.test
	if t == nil || t.Done {
		a.mu.Unlock()
		return
	}
	t.Done = true
	samples := append([]Sample(nil), t.Samples...)
	a.mu.Unlock()

	res := t.analyse()
	os.MkdirAll(a.logDir, 0o755)
	base := "tanktest_" + t.Start.Format("20060102_150405")
	if l := strings.Trim(unsafeName.ReplaceAllString(t.Label, "_"), "_"); l != "" {
		base += "_" + l
	}
	csvPath := filepath.Join(a.logDir, base+".csv")
	if fh, err := os.Create(csvPath); err == nil {
		w := csv.NewWriter(fh)
		w.Write([]string{"timestamp", "seq", "distance_cm", "height_cm", "level_pct", "volume_l", "temp_c", "batt_mv", "lora_tx"})
		fs := func(p *float64, d int) string {
			if p == nil {
				return ""
			}
			return strconv.FormatFloat(*p, 'f', d, 64)
		}
		for _, s := range samples {
			r := s.R
			w.Write([]string{s.T.Format("2006-01-02 15:04:05"), strconv.Itoa(r.Seq), fs(r.DistanceCm, 1), fs(r.HeightCm, 1),
				fs(r.LevelPct, 1), fs(r.VolumeL, 0), fs(r.TempC, 2), fs(r.BattMv, 0), r.TX})
		}
		w.Flush()
		fh.Close()
		res.CSVPath = csvPath
	}
	repPath := filepath.Join(a.logDir, base+"_summary.txt")
	if err := os.WriteFile(repPath, []byte(report(res, t.Cfg)), 0o644); err == nil {
		res.ReportPath = repPath
	}
	a.mu.Lock()
	t.Result = res
	a.mu.Unlock()
	a.hub.send("test", map[string]any{"running": false, "result": res})
}

func report(r *TestResult, c *Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Watershed local tank test\r\n=========================\r\n")
	fmt.Fprintf(&b, "Label:    %s\r\nStarted:  %s\r\nDuration: %d s, %d readings\r\n\r\n", r.Label, r.Started, r.Seconds, r.Samples)
	if c != nil {
		fmt.Fprintf(&b, "Node %d  shape %s  diam %.1f cm  length %.1f cm  width %.1f cm\r\n", c.NodeID, c.Shape, c.DiamCm, c.LenCm, c.WidthCm)
		fmt.Fprintf(&b, "Empty %.1f cm  Full %.1f cm  Capacity %.0f L\r\n\r\n", c.EmptyCm, c.FullCm, c.CapacityL)
	}
	line := func(name string, s *Stat, unit string, d int) {
		if s == nil {
			fmt.Fprintf(&b, "%-12s  no data\r\n", name)
			return
		}
		fmt.Fprintf(&b, "%-12s  mean %.*f %s  SD %.2f  min %.*f  max %.*f\r\n", name, d, s.Mean, unit, s.SD, d, s.Min, d, s.Max)
	}
	fmt.Fprintf(&b, "Echo OK       %d/%d (%.0f%%)\r\n", r.EchoOK, r.Samples, r.EchoPct)
	line("Distance", r.Distance, "cm", 1)
	line("Height", r.Height, "cm", 1)
	line("Level", r.Level, "%", 1)
	line("Volume", r.Volume, "L", 0)
	line("Temperature", r.Temp, "C", 1)
	line("Supply", r.Supply, "mV", 0)
	keys := make([]string, 0, len(r.TX))
	for k := range r.TX {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&b, "LoRa TX      ")
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, r.TX[k])
	}
	fmt.Fprintf(&b, "\r\n")
	if r.RefErrCm != nil {
		fmt.Fprintf(&b, "Dipstick     %.1f cm, error %+.1f cm (%+.0f L)\r\n", *r.RefHeight, *r.RefErrCm, *r.RefErrL)
	}
	fmt.Fprintf(&b, "\r\nFindings\r\n--------\r\n")
	for _, v := range r.Verdicts {
		fmt.Fprintf(&b, "[%s] %s\r\n", strings.ToUpper(v.Level), v.Text)
	}
	return b.String()
}

// ---------------- HTTP ----------------

var cmdOK = regexp.MustCompile(`^[A-Za-z0-9 ._\-]{1,64}$`)

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func errOut(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	jsonOut(w, map[string]string{"error": msg})
}

func (a *App) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	m.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]string{"app": "watershed-tank-tool", "version": appVersion})
	})
	m.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan []byte, 256)
		a.hub.mu.Lock()
		a.hub.clients[ch] = struct{}{}
		a.hub.everClient = true
		a.hub.mu.Unlock()
		defer func() {
			a.hub.mu.Lock()
			delete(a.hub.clients, ch)
			a.hub.lastSeen = time.Now()
			a.hub.mu.Unlock()
		}()
		// initial snapshot
		a.mu.Lock()
		snap := map[string]any{"status": a.statusLocked(), "cfg": a.cfgRaw, "last": a.last, "lines": a.lines}
		if a.flash != nil {
			snap["flash"] = a.flash.snapshot()
		}
		if a.test != nil {
			if a.test.Done {
				snap["test"] = map[string]any{"running": false, "result": a.test.Result}
			} else {
				snap["test"] = map[string]any{"running": true, "elapsed": int(time.Since(a.test.Start).Seconds()), "total": int(a.test.Duration.Seconds()), "samples": len(a.test.Samples), "label": a.test.Label}
			}
		}
		a.mu.Unlock()
		snap["update"] = a.updateState()
		b, _ := json.Marshal(snap)
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b)
		fl.Flush()
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case msg := <-ch:
				w.Write(msg)
				fl.Flush()
			case <-ping.C:
				w.Write([]byte(": ping\n\n"))
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	post := func(path string, h func(w http.ResponseWriter, body map[string]any)) {
		m.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			// custom header = browsers won't send this cross-site without a
			// CORS preflight we never answer, so other web pages can't drive us
			if r.Method != http.MethodPost || r.Header.Get("X-Watershed") != "1" {
				errOut(w, 403, "forbidden")
				return
			}
			body := map[string]any{}
			json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
			h(w, body)
		})
	}
	str := func(b map[string]any, k string) string { s, _ := b[k].(string); return s }
	num := func(b map[string]any, k string) (float64, bool) {
		switch v := b[k].(type) {
		case float64:
			return v, true
		case string:
			x, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return x, err == nil
		}
		return 0, false
	}

	m.HandleFunc("/api/ports", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, listPorts()) })
	m.HandleFunc("/api/drivers", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"missing": missingDrivers()})
	})
	post("/api/connect", func(w http.ResponseWriter, b map[string]any) {
		name := str(b, "port")
		if name == "" {
			errOut(w, 400, "no port given")
			return
		}
		a.mu.Lock()
		flashing := a.flash != nil && a.flash.Running
		a.mu.Unlock()
		if flashing {
			errOut(w, 400, "wait for flashing to finish")
			return
		}
		a.mu.Lock()
		a.auto = false // user picked manually
		a.mu.Unlock()
		ok, err := a.connect(name, true)
		if err != nil {
			errOut(w, 400, err.Error())
			return
		}
		jsonOut(w, map[string]any{"identified": ok})
	})
	post("/api/disconnect", func(w http.ResponseWriter, b map[string]any) {
		a.mu.Lock()
		a.auto = false
		a.mu.Unlock()
		a.disconnect()
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/autodetect", func(w http.ResponseWriter, b map[string]any) {
		a.mu.Lock()
		unknown := a.connected && a.ident == nil
		a.mu.Unlock()
		if unknown { // old firmware / wrong port: let go so we can look again
			a.disconnect()
		}
		a.mu.Lock()
		a.auto = true
		a.failed = map[string]bool{}
		a.mu.Unlock()
		a.pushStatus()
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/cmd", func(w http.ResponseWriter, b map[string]any) {
		c := strings.TrimSpace(str(b, "cmd"))
		if !cmdOK.MatchString(c) {
			errOut(w, 400, "command contains characters the node doesn't accept")
			return
		}
		if err := a.write(c); err != nil {
			errOut(w, 400, err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/config", func(w http.ResponseWriter, b map[string]any) {
		var cmds []string
		if v, ok := num(b, "node_id"); ok {
			if v < 1 || v > 255 {
				errOut(w, 400, "node ID must be 1-255")
				return
			}
			cmds = append(cmds, fmt.Sprintf("NODE SET %d", int(v)))
		}
		if s := strings.ToUpper(str(b, "shape")); s != "" {
			if s != "VERT" && s != "HORIZ" && s != "RECT" {
				errOut(w, 400, "bad shape")
				return
			}
			cmds = append(cmds, "TANK SET SHAPE "+s)
		}
		for _, d := range []struct{ key, cmd, label string }{
			{"diam_cm", "TANK SET DIAM", "diameter"}, {"len_cm", "TANK SET LEN", "length"},
			{"width_cm", "TANK SET WIDTH", "width"}, {"empty_cm", "CAL SET EMPTY", "empty distance"},
			{"full_cm", "CAL SET FULL", "full distance"},
		} {
			if v, ok := num(b, d.key); ok {
				if v <= 0 || v > 5000 {
					errOut(w, 400, d.label+" must be between 0 and 5000 cm")
					return
				}
				cmds = append(cmds, fmt.Sprintf("%s %.1f", d.cmd, v))
			}
		}
		if v, ok := b["tx_on"].(bool); ok {
			if v {
				cmds = append(cmds, "RADIO TX ON")
			} else {
				cmds = append(cmds, "RADIO TX OFF")
			}
		}
		for _, c := range cmds {
			if err := a.write(c); err != nil {
				errOut(w, 400, err.Error())
				return
			}
			time.Sleep(120 * time.Millisecond)
		}
		jsonOut(w, map[string]any{"sent": cmds})
	})
	post("/api/test/start", func(w http.ResponseWriter, b map[string]any) {
		secs, _ := num(b, "seconds")
		if secs < 10 || secs > 24*3600 {
			errOut(w, 400, "duration must be 10 s to 24 h")
			return
		}
		var ref *float64
		if v, ok := num(b, "ref_height_cm"); ok && v >= 0 {
			ref = &v
		}
		if err := a.startTest(str(b, "label"), time.Duration(secs)*time.Second, ref); err != nil {
			errOut(w, 400, err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/test/stop", func(w http.ResponseWriter, b map[string]any) {
		a.finishTest()
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/openlogs", func(w http.ResponseWriter, b map[string]any) {
		os.MkdirAll(a.logDir, 0o755)
		openFolder(a.logDir)
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/update/check", func(w http.ResponseWriter, b map[string]any) {
		go a.checkUpdates(false)
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/update/tool", func(w http.ResponseWriter, b map[string]any) {
		a.mu.Lock()
		busy := (a.test != nil && !a.test.Done) || (a.flash != nil && a.flash.Running)
		a.mu.Unlock()
		if busy {
			errOut(w, 400, "wait for the test or flash to finish")
			return
		}
		go func() {
			if err := a.updateTool(); err != nil {
				a.sysLine("tool update failed: %v", err)
				a.hub.send("nodeerror", "Tool update failed: "+err.Error())
			}
		}()
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/flash/release", func(w http.ResponseWriter, b map[string]any) {
		force, _ := b["force"].(bool)
		if err := a.flashRelease(str(b, "port"), force); err != nil {
			errOut(w, 400, err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true})
	})
	// test build upload: raw .bin body (not JSON)
	m.HandleFunc("/api/flash/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Watershed") != "1" {
			errOut(w, 403, "forbidden")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAppSize+1))
		if err != nil {
			errOut(w, 400, "file too big or unreadable")
			return
		}
		name := filepath.Base(r.Header.Get("X-Filename"))
		if err := a.flashUpload(r.URL.Query().Get("port"), name, data); err != nil {
			errOut(w, 400, err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/support/bundle", func(w http.ResponseWriter, b map[string]any) {
		p, err := a.supportBundle()
		if err != nil {
			errOut(w, 500, "couldn't save the support bundle: "+err.Error())
			return
		}
		go func() { pruneFiles(a.logDir, "support_", 10) }()
		if reveal, _ := b["reveal"].(bool); reveal {
			revealFile(p)
		}
		jsonOut(w, map[string]any{"path": p})
	})
	post("/api/support/diagnostics", func(w http.ResponseWriter, b map[string]any) {
		jsonOut(w, map[string]any{"text": a.diagnostics()})
	})
	post("/api/show", func(w http.ResponseWriter, b map[string]any) {
		a.mu.Lock()
		show, url := a.showWindow, a.url
		a.mu.Unlock()
		if show != nil {
			show()
		} else {
			openWindow(url)
		}
		jsonOut(w, map[string]any{"ok": true})
	})
	post("/api/quit", func(w http.ResponseWriter, b map[string]any) {
		jsonOut(w, map[string]any{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); a.disconnect(); a.slog.Close(); os.Exit(0) }()
	})
	return m
}

func pickLogDir() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Join(filepath.Dir(exe), "logs")
		if os.MkdirAll(d, 0o755) == nil {
			probe := filepath.Join(d, ".write_test")
			if os.WriteFile(probe, []byte("x"), 0o644) == nil {
				os.Remove(probe)
				return d
			}
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Documents", "WatershedTankTool", "logs")
}

func main() {
	updated := false
	for _, a := range os.Args[1:] {
		if a == "--updated" {
			updated = true
		}
	}
	fmt.Printf("%s v%s\n", appName, appVersion)
	go cleanupOldExe()

	// already running? just bring up another window onto it
	if !updated {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", defaultPort)); err == nil {
			var p map[string]string
			json.NewDecoder(resp.Body).Decode(&p)
			resp.Body.Close()
			if p["app"] == "watershed-tank-tool" {
				// ask the running copy to show itself
				req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/show", defaultPort), strings.NewReader("{}"))
				req.Header.Set("X-Watershed", "1")
				if r, err := http.DefaultClient.Do(req); err == nil {
					r.Body.Close()
				}
				return
			}
		}
	}

	// after an update the old copy may need a moment to let go of the port
	var ln net.Listener
	var err error
	for i := 0; i < 30; i++ {
		if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", defaultPort)); err == nil || !updated {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			showMessage(appName, "The Watershed Tank Tool couldn't start:\n\n"+err.Error())
			return
		}
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())

	app := &App{hub: &Hub{clients: map[chan []byte]struct{}{}}, failed: map[string]bool{}, auto: true,
		logDir: pickLogDir(), upd: newUpdater()}
	app.slog = newSessionLog(app.logDir, sessionHeader())
	if updated {
		app.sysLine("restarted after updating to v%s", appVersion)
	}
	go app.watch()
	if app.upd.enabled() {
		go app.updateLoop()
	}

	fmt.Println("Window:", url)
	fmt.Println("Logs:", app.logDir)
	if app.upd.enabled() {
		fmt.Println("Updates from: github.com/" + app.upd.repo)
	}
	fmt.Println("Leave this window open while you use the tool. Closing the app window quits it.")
	if app.upd.enabled() { // release builds only, not Dave's dev builds
		go ensureShortcuts(app)
	}
	app.url = url
	app.srv = &http.Server{Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	go app.srv.Serve(ln)

	if os.Getenv("WATERSHED_NO_WINDOW") != "" {
		select {}
	}
	// the tool's own window; returns when it's closed
	if runAppWindow(url, app) {
		app.disconnect()
		app.slog.Close()
		os.Exit(0)
	}
	// no WebView2: use an Edge/Chrome app window, and quit a little while
	// after it's closed (see watch)
	if updated {
		// an existing browser window reconnects by itself after an update
		time.Sleep(8 * time.Second)
		app.hub.mu.Lock()
		had := app.hub.everClient
		app.hub.mu.Unlock()
		if had {
			select {}
		}
	} else {
		time.Sleep(300 * time.Millisecond)
	}
	openWindow(url)
	select {}
}
