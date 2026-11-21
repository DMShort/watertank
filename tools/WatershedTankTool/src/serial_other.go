//go:build !windows

package main

// Non-Windows build: used for development/testing only (e.g. against a
// simulated node on a pty). Extra port paths can be supplied in
// WATERSHED_PORTS (comma separated).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type unixPort struct{ f *os.File }

func openPort(name string) (Port, error) {
	exec.Command("stty", "-F", name, "115200", "raw", "-echo", "-hupcl").Run()
	f, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", name, err)
	}
	return &unixPort{f}, nil
}

func (p *unixPort) Read(b []byte) (int, error) {
	p.f.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n, err := p.f.Read(b)
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		return n, nil
	}
	return n, err
}
func (p *unixPort) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *unixPort) Close() error                { return p.f.Close() }

func listPorts() []PortInfo {
	var names []string
	for _, g := range []string{"/dev/ttyUSB*", "/dev/ttyACM*"} {
		m, _ := filepath.Glob(g)
		names = append(names, m...)
	}
	if extra := os.Getenv("WATERSHED_PORTS"); extra != "" {
		for _, e := range strings.Split(extra, ",") {
			if _, err := os.Stat(e); err == nil {
				names = append(names, e)
			}
		}
	}
	sort.Strings(names)
	var out []PortInfo
	for _, n := range names {
		pi := PortInfo{Name: n, Desc: "serial device"}
		classify(&pi)
		if strings.Contains(n, "pts") {
			pi.Likely = true
		}
		out = append(out, pi)
	}
	return out
}

func openWindow(url string)  { exec.Command("xdg-open", url).Start() }
func openFolder(path string) { exec.Command("xdg-open", path).Start() }

func hideWindow(c *exec.Cmd) {}
