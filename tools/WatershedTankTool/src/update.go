package main

// Updates from GitHub Releases.
//
// The release workflow (.github/workflows/release.yml) publishes, for each
// version tag, the Windows exe, the firmware parts, esptool.exe and a
// manifest.json listing them with SHA-256 hashes. This file checks the
// latest release, downloads + verifies assets into a cache, and replaces
// the running exe with a newer one.
//
// Repo comes from the build (-X main.updateRepo=owner/name). A private
// repo needs a read-only token in update.json next to the exe:
//   {"repo": "owner/name", "token": "github_pat_..."}

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	appVersion = "dev" // set by the release build: -X main.appVersion=0.4.0
	updateRepo = ""    // set by the release build: -X main.updateRepo=owner/name
)

type Asset struct {
	Version string `json:"version,omitempty"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
}

type FwPart struct {
	Offset string `json:"offset"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Version   string `json:"version"`
	Tag       string `json:"tag"`
	Notes     string `json:"notes"`
	Published string `json:"published"`
	Tool      Asset  `json:"tool"`
	Esptool   Asset  `json:"esptool"`
	Firmware  struct {
		Version string   `json:"version"`
		Board   string   `json:"board"`
		Chip    string   `json:"chip"`
		Parts   []FwPart `json:"parts"`
	} `json:"firmware"`
}

type Updater struct {
	mu       sync.Mutex
	repo     string
	token    string
	apiBase  string
	autoTool bool
	cacheDir string
	client   *http.Client
	latest   *Manifest
	assets   map[string]string // asset name -> API download URL
	checked  time.Time
	lastErr  string
	busy     string
}

type updateCfg struct {
	Repo     string `json:"repo"`
	Token    string `json:"token"`
	AutoTool *bool  `json:"auto_update_tool"`
}

func newUpdater() *Updater {
	u := &Updater{repo: updateRepo, apiBase: "https://api.github.com", autoTool: true,
		client: &http.Client{Timeout: 5 * time.Minute}}
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "update.json")); err == nil {
			var c updateCfg
			if json.Unmarshal(b, &c) == nil {
				if c.Repo != "" {
					u.repo = c.Repo
				}
				u.token = c.Token
				if c.AutoTool != nil {
					u.autoTool = *c.AutoTool
				}
			}
		}
	}
	if v := os.Getenv("WATERSHED_API_BASE"); v != "" { // tests
		u.apiBase = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("WATERSHED_REPO"); v != "" {
		u.repo = v
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	u.cacheDir = filepath.Join(base, "WatershedTankTool", "cache")
	return u
}

func (u *Updater) enabled() bool { return u.repo != "" }

func (u *Updater) get(url, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "WatershedTankTool/"+appVersion)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	return u.client.Do(req) // Go drops Authorization on the redirect to GitHub's file host
}

// Check asks GitHub for the latest release and reads its manifest.
func (u *Updater) Check() (*Manifest, error) {
	if !u.enabled() {
		return nil, fmt.Errorf("updates are off in this build (no repository set)")
	}
	m, assets, err := u.check()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = time.Now()
	if err != nil {
		u.lastErr = err.Error()
		return nil, err
	}
	u.lastErr = ""
	u.latest, u.assets = m, assets
	return m, nil
}

func (u *Updater) check() (*Manifest, map[string]string, error) {
	resp, err := u.get(fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase, u.repo), "application/vnd.github+json")
	if err != nil {
		return nil, nil, fmt.Errorf("can't reach GitHub (no internet?): %v", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404:
		if u.token == "" {
			return nil, nil, fmt.Errorf("no release found for %s (none published yet, or the repository is private and needs a token in update.json)", u.repo)
		}
		return nil, nil, fmt.Errorf("no release found for %s", u.repo)
	case 401, 403:
		return nil, nil, fmt.Errorf("GitHub refused the request (%s): check the token in update.json, or try again in an hour if rate-limited", resp.Status)
	default:
		return nil, nil, fmt.Errorf("GitHub said %s", resp.Status)
	}
	var rel struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, nil, fmt.Errorf("bad reply from GitHub: %v", err)
	}
	assets := map[string]string{}
	for _, a := range rel.Assets {
		assets[a.Name] = a.URL
	}
	murl, ok := assets["manifest.json"]
	if !ok {
		return nil, nil, fmt.Errorf("release %s has no manifest.json (did the build finish?)", rel.TagName)
	}
	mr, err := u.get(murl, "application/octet-stream")
	if err != nil {
		return nil, nil, err
	}
	defer mr.Body.Close()
	if mr.StatusCode != 200 {
		return nil, nil, fmt.Errorf("manifest download: %s", mr.Status)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(mr.Body, 1<<20)).Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("bad manifest.json: %v", err)
	}
	if m.Tag == "" {
		m.Tag = rel.TagName
	}
	if m.Published == "" {
		m.Published = rel.PublishedAt
	}
	return &m, assets, nil
}

// Fetch returns a cached, hash-verified copy of a release asset.
func (u *Updater) Fetch(name, sha string) (string, error) {
	u.mu.Lock()
	m, assets := u.latest, u.assets
	u.mu.Unlock()
	if m == nil {
		return "", fmt.Errorf("no release information yet")
	}
	if name == "" || strings.ContainsAny(name, `/\`) || sha == "" {
		return "", fmt.Errorf("release manifest is missing a file name or hash")
	}
	dir := filepath.Join(u.cacheDir, safeName(m.Tag))
	path := filepath.Join(dir, name)
	if h, err := fileSHA256(path); err == nil && strings.EqualFold(h, sha) {
		return path, nil
	}
	url, ok := assets[name]
	if !ok {
		return "", fmt.Errorf("release %s has no file %s", m.Tag, name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	resp, err := u.get(url, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("download %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: %s", name, resp.Status)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("download %s: %v", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sha) {
		os.Remove(tmp)
		return "", fmt.Errorf("%s failed its integrity check (download corrupted?), not used", name)
	}
	os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		os.Chmod(path, 0o755)
	}
	return path, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if s == "" {
		return "unknown"
	}
	return s
}

// compareVersions: -1 if a<b, 0 equal, 1 if a>b. "v0.4.0" == "0.4" etc.
// Anything unparsable (e.g. "dev") counts as older than any real version.
func compareVersions(a, b string) int {
	pa, okA := parseVer(a)
	pb, okB := parseVer(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVer(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return out, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ---------------- app side ----------------

func (a *App) updateState() map[string]any {
	u := a.upd
	u.mu.Lock()
	defer u.mu.Unlock()
	st := map[string]any{"enabled": u.enabled(), "repo": u.repo, "tool_version": appVersion,
		"error": u.lastErr, "busy": u.busy, "auto_tool": u.autoTool}
	if !u.checked.IsZero() {
		st["checked"] = u.checked.Format("2006-01-02 15:04")
	}
	if m := u.latest; m != nil {
		st["latest"] = map[string]any{"version": m.Version, "tag": m.Tag, "notes": m.Notes, "published": m.Published,
			"tool_version": m.Tool.Version, "fw_version": m.Firmware.Version, "fw_board": m.Firmware.Board}
		st["tool_update"] = m.Tool.File != "" && compareVersions(m.Tool.Version, appVersion) > 0
	}
	return st
}

func (a *App) pushUpdate() { a.hub.send("update", a.updateState()) }

func (a *App) setBusy(s string) {
	a.upd.mu.Lock()
	a.upd.busy = s
	a.upd.mu.Unlock()
	a.pushUpdate()
}

func (a *App) checkUpdates(auto bool) {
	if !a.upd.enabled() {
		a.pushUpdate()
		return
	}
	m, err := a.upd.Check()
	a.pushUpdate()
	if err != nil {
		a.sysLine("update check: %v", err)
		return
	}
	a.sysLine("latest release %s (tool %s, firmware %s); this tool is %s", m.Tag, m.Tool.Version, m.Firmware.Version, appVersion)
	if auto && a.upd.autoTool && compareVersions(m.Tool.Version, appVersion) > 0 {
		a.mu.Lock()
		busy := (a.test != nil && !a.test.Done) || (a.flash != nil && a.flash.Running)
		a.mu.Unlock()
		if busy {
			a.sysLine("tool update %s waiting until the test/flash finishes", m.Tool.Version)
			return
		}
		if err := a.updateTool(); err != nil {
			a.sysLine("tool update failed: %v", err)
		}
	}
}

func (a *App) updateLoop() {
	time.Sleep(2 * time.Second)
	for {
		a.checkUpdates(true)
		for i := 0; i < 6*60; i++ { // every 6 h; also retry pending updates each minute
			time.Sleep(time.Minute)
			a.upd.mu.Lock()
			pending := a.upd.latest != nil && a.upd.autoTool && compareVersions(a.upd.latest.Tool.Version, appVersion) > 0
			a.upd.mu.Unlock()
			if pending {
				break
			}
		}
	}
}

// updateTool downloads the new exe, swaps it in and restarts.
func (a *App) updateTool() error {
	a.upd.mu.Lock()
	m := a.upd.latest
	a.upd.mu.Unlock()
	if m == nil || m.Tool.File == "" {
		return fmt.Errorf("no tool update available")
	}
	a.setBusy("Downloading tool " + m.Tool.Version + "…")
	path, err := a.upd.Fetch(m.Tool.File, m.Tool.SHA256)
	if err != nil {
		a.setBusy("")
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		a.setBusy("")
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	old := exe + ".old"
	os.Remove(old)
	// a running exe can't be overwritten on Windows, but it can be renamed
	if err := os.Rename(exe, old); err != nil {
		a.setBusy("")
		return fmt.Errorf("can't replace %s (keep the tool in a normal folder such as Desktop or Documents, not Program Files): %v", filepath.Base(exe), err)
	}
	if err := copyFile(path, exe); err != nil {
		os.Rename(old, exe)
		a.setBusy("")
		return fmt.Errorf("install update: %v", err)
	}
	a.sysLine("installed tool %s, restarting", m.Tool.Version)
	a.restart(exe)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (a *App) restart(exe string) {
	a.hub.send("restarting", appVersion)
	time.Sleep(400 * time.Millisecond)
	a.disconnect()
	a.slog.Close()
	if a.srv != nil {
		a.srv.Close() // free the port for the new copy
	}
	cmd := exec.Command(exe, "--updated")
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		fmt.Println("Restart failed - please start the tool again:", err)
		time.Sleep(5 * time.Second)
	}
	os.Exit(0)
}

// removes the previous exe left behind by an update (it may still be
// locked for a moment while the old process exits)
func cleanupOldExe() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	old := exe + ".old"
	for i := 0; i < 20; i++ {
		err := os.Remove(old)
		if err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Second)
	}
}
