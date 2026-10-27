package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Muxer supervises netmuxd, which serves the usbmuxd protocol for USB and Wi-Fi devices.
type Muxer struct {
	mu       sync.Mutex
	running  bool
	restarts int
	lastErr  string
	since    time.Time
	notified time.Time
}

type MuxerStatus struct {
	Enabled  bool
	Running  bool
	Restarts int
	LastErr  string
	Since    time.Time
}

func (m *Muxer) Status() MuxerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MuxerStatus{true, m.running, m.restarts, m.lastErr, m.since}
}

// Run keeps netmuxd alive until ctx ends, restarting it with backoff.
func (m *Muxer) Run(ctx context.Context, app *App, lockdownDir string) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "netmuxd", "--plist-storage", lockdownDir)
		cmd.Env = append(os.Environ(), "RUST_LOG=warn")
		out, _ := cmd.StderrPipe() // stdout only carries a device-list dump on every client request
		start := time.Now()
		err := cmd.Start()
		if err == nil {
			m.set(true, "")
			go prefixLog("netmuxd", out)
			err = cmd.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		m.mu.Lock()
		m.restarts++
		m.mu.Unlock()
		m.set(false, msg)
		log.Printf("netmuxd %s, restarting in %s", msg, backoff)
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		if time.Since(m.notified) > time.Hour { // a crash loop must not spam
			m.notified = time.Now()
			app.notify(Event{Kind: EvMuxer, Detail: msg})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (m *Muxer) set(running bool, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = running
	m.since = time.Now()
	if errMsg != "" {
		m.lastErr = errMsg
	}
}

func prefixLog(prefix string, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Printf("[%s] %s", prefix, sc.Text())
	}
}

// Presence of a device as reported by the muxer.
type Presence struct {
	USB bool
	Net bool
}

func (p Presence) Online() bool { return p.USB || p.Net }

// Watcher polls the muxer for connected devices and keeps device info fresh.
type Watcher struct {
	mu         sync.Mutex
	present    map[string]Presence
	refreshing map[string]bool
	attempted  map[string]time.Time // last info refresh attempt; only touched by the poll loop
}

var idLine = regexp.MustCompile(`^([0-9A-Fa-f-]{20,})\s+\((USB|Network)\)`)

func parseDeviceList(out string) map[string]Presence {
	m := map[string]Presence{}
	for _, line := range strings.Split(out, "\n") {
		g := idLine.FindStringSubmatch(strings.TrimSpace(line))
		if g == nil {
			continue
		}
		p := m[g[1]]
		if g[2] == "USB" {
			p.USB = true
		} else {
			p.Net = true
		}
		m[g[1]] = p
	}
	return m
}

func (w *Watcher) Presence(udid string) Presence {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.present[udid]
}

// UnpairedUSB lists devices plugged in over USB that have no pairing record yet.
func (w *Watcher) UnpairedUSB(lockdownDir string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for udid, p := range w.present {
		if p.USB && !fileExists(filepath.Join(lockdownDir, udid+".plist")) {
			out = append(out, udid)
		}
	}
	return out
}

func (w *Watcher) Run(ctx context.Context, app *App) {
	w.present = map[string]Presence{}
	w.refreshing = map[string]bool{}
	w.attempted = map[string]time.Time{}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		w.poll(ctx, app)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *Watcher) poll(ctx context.Context, app *App) {
	for _, udid := range pairedUDIDs(app.lockdownDir) {
		if isNew, _ := app.store.EnsureDevice(udid); isNew {
			log.Printf("registered paired device %s", udid)
		}
	}
	out, _ := runCmd(ctx, 15*time.Second, nil, "idevice_id")
	present := parseDeviceList(out)
	w.mu.Lock()
	changed := len(present) != len(w.present)
	for k, v := range present {
		if w.present[k] != v {
			changed = true
		}
	}
	w.present = present
	w.mu.Unlock()
	if changed {
		app.broker.Publish(LiveEvent{Kind: "presence"})
	}

	for udid, p := range present {
		if !p.Online() {
			continue
		}
		d, err := app.store.Device(udid)
		if err != nil {
			continue // not paired with us
		}
		app.store.TouchDevice(udid)
		if time.Since(time.Unix(d.InfoAt, 0)) > time.Hour && time.Since(w.attempted[udid]) > 10*time.Minute && !app.runner.Busy(udid) {
			w.attempted[udid] = time.Now()
			w.refreshInfo(app, udid)
		}
	}
}

func (w *Watcher) refreshInfo(app *App, udid string) {
	w.mu.Lock()
	if w.refreshing[udid] {
		w.mu.Unlock()
		return
	}
	w.refreshing[udid] = true
	w.mu.Unlock()
	go func() {
		defer func() { w.mu.Lock(); delete(w.refreshing, udid); w.mu.Unlock() }()
		if err := app.refreshDeviceInfo(udid); err != nil {
			log.Printf("device info %s: %v", udid, err)
		}
	}()
}

// refreshDeviceInfo reads name, model, iOS version and backup encryption from the device.
func (a *App) refreshDeviceInfo(udid string) error {
	out, err := runCmd(context.Background(), 30*time.Second, nil, "ideviceinfo", "-n", "-u", udid)
	if err != nil {
		return err
	}
	info := parseKeyValues(out)
	enc := -1
	if v, err := runCmd(context.Background(), 30*time.Second, nil, "ideviceinfo", "-n", "-u", udid, "-q", "com.apple.mobile.backup", "-k", "WillEncrypt"); err == nil {
		switch strings.TrimSpace(v) {
		case "true":
			enc = 1
		case "false", "":
			enc = 0
		}
	}
	err = a.store.UpdateDeviceInfo(udid, info["DeviceName"], info["ProductType"], info["ProductVersion"], enc)
	a.broker.Publish(LiveEvent{Kind: "presence"})
	return err
}

func parseKeyValues(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok && !strings.HasPrefix(k, " ") {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

func pairedUDIDs(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.plist"))
	var out []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".plist")
		if validUDID(name) {
			out = append(out, name)
		}
	}
	return out
}

var udidRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-?[0-9A-Fa-f]{16}$|^[0-9A-Fa-f]{40}$`)

func validUDID(s string) bool { return udidRe.MatchString(s) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// runCmd runs a tool with a timeout and returns combined output.
func runCmd(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Pairing over USB.

type PairResult struct {
	State string // "trust" (tap Trust on the phone), "ok", "error"
	Out   string
}

// pairUSB runs one pairing attempt; the first attempt usually asks to tap Trust on the phone.
func (a *App) pairUSB(udid string) PairResult {
	out, _ := runCmd(context.Background(), 60*time.Second, nil, "idevicepair", "-u", udid, "pair")
	switch {
	case strings.Contains(out, "SUCCESS"):
	case strings.Contains(out, "trust dialog") || strings.Contains(out, "passcode"):
		return PairResult{"trust", out}
	default:
		return PairResult{"error", out}
	}
	if wout, err := runCmd(context.Background(), 30*time.Second, nil, "idevicewifi", "-u", udid, "on"); err != nil {
		return PairResult{"error", out + "\n" + wout}
	}
	os.Chmod(filepath.Join(a.lockdownDir, udid+".plist"), 0o600)
	a.store.EnsureDevice(udid)
	go a.refreshDeviceInfo(udid)
	return PairResult{"ok", out}
}

func usbAvailable() bool {
	entries, err := os.ReadDir("/dev/bus/usb")
	return err == nil && len(entries) > 0
}
