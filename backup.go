package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Phases of a running backup, shown in the UI and in Telegram.
const (
	PhaseQueued     = "queued"
	PhaseSnapshot   = "snapshot"
	PhaseConnecting = "connecting"
	PhasePasscode   = "passcode"
	PhaseTransfer   = "transfer"
)

type Live struct {
	JobID    int64
	UDID     string
	Phase    string
	Progress int
}

// Runner executes backups one at a time.
type Runner struct {
	app   *App
	queue chan int64
	mu    sync.Mutex
	live  map[string]*Live // by UDID
}

func newRunner(app *App) *Runner {
	return &Runner{app: app, queue: make(chan int64, 64), live: map[string]*Live{}}
}

func (r *Runner) Busy(udid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[udid] != nil
}

func (r *Runner) Live(udid string) *Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.live[udid]; l != nil {
		c := *l
		return &c
	}
	return nil
}

// update applies f to the device's live state and broadcasts it when f reports a change.
func (r *Runner) update(udid string, f func(*Live) bool) {
	r.mu.Lock()
	l := r.live[udid]
	changed := l != nil && f(l)
	var c Live
	if changed {
		c = *l
	}
	r.mu.Unlock()
	if changed {
		r.app.broker.Publish(LiveEvent{Kind: "job", UDID: udid, JobID: c.JobID, Phase: c.Phase, Progress: c.Progress})
		r.app.tgProgress(c)
	}
}

func setPhase(phase string) func(*Live) bool {
	return func(l *Live) bool {
		changed := l.Phase != phase
		l.Phase = phase
		return changed
	}
}

// Enqueue starts a backup unless one is already queued or running for the device.
func (r *Runner) Enqueue(udid, trigger string) (int64, error) {
	r.mu.Lock()
	if l := r.live[udid]; l != nil {
		r.mu.Unlock()
		return l.JobID, nil
	}
	id, err := r.app.store.CreateJob(udid, trigger)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	r.live[udid] = &Live{JobID: id, UDID: udid, Phase: PhaseQueued}
	r.mu.Unlock()
	r.app.broker.Publish(LiveEvent{Kind: "job", UDID: udid, JobID: id, Phase: PhaseQueued})
	r.queue <- id
	return id, nil
}

func (r *Runner) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.queue:
			job, err := r.app.store.Job(id)
			if err != nil {
				log.Printf("job %d: %v", id, err)
				continue
			}
			r.run(ctx, job)
			r.mu.Lock()
			delete(r.live, job.UDID)
			r.mu.Unlock()
			r.app.broker.Publish(LiveEvent{Kind: "job", UDID: job.UDID, JobID: job.ID, Phase: "done", Status: job.Status})
			r.app.jobFinished(job)
		}
	}
}

const minFreeBytes = 5 << 30

func (r *Runner) run(ctx context.Context, job *Job) {
	a := r.app
	d, err := a.store.Device(job.UDID)
	if err != nil {
		job.Status, job.Error = JobFailed, "unknown device"
		a.store.FinishJob(job)
		return
	}
	if free := freeBytes(a.backupDir); free >= 0 && free < minFreeBytes {
		job.Status, job.Error = JobFailed, fmt.Sprintf("only %s free on the backup disk", humanBytes(free))
		a.store.FinishJob(job)
		return
	}

	liveDir := filepath.Join(a.backupDir, job.UDID)
	if d.KeepVersions > 0 && a.reflink && fileExists(filepath.Join(liveDir, "Manifest.db")) {
		r.update(job.UDID, setPhase(PhaseSnapshot))
		snap := filepath.Join(a.snapshotDir(job.UDID), job.UDID+"-"+time.Now().Format("20060102-150405"))
		os.MkdirAll(filepath.Dir(snap), 0o755)
		if out, err := exec.Command("cp", "-a", "--reflink=always", liveDir, snap).CombinedOutput(); err != nil {
			log.Printf("snapshot %s: %v %s", snap, err, out)
			os.RemoveAll(snap)
		} else {
			job.Snapshot = snap
		}
	}
	job.StartedAt = now()
	a.store.StartJob(job.ID, job.Snapshot)
	r.update(job.UDID, setPhase(PhaseConnecting))

	res := r.exec(ctx, job.UDID)
	job.Files, job.Log = res.files, res.log
	switch {
	case res.ok:
		job.Status = JobOK
		job.Bytes = dirSize(liveDir)
		a.pruneSnapshots(job.UDID, d.KeepVersions)
	case res.errorCode == 208:
		// Nobody typed the passcode; the device sent nothing, so the snapshot is a duplicate.
		job.Status, job.Error = JobNoPasscode, "passcode not entered on the device"
		if job.Snapshot != "" {
			os.RemoveAll(job.Snapshot)
			job.Snapshot = ""
		}
	default:
		job.Status, job.Error = JobFailed, res.err
	}
	a.store.FinishJob(job)
}

type backupResult struct {
	ok        bool
	files     int64
	errorCode int
	err       string
	log       string
}

var (
	ansiRe     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	overallRe  = regexp.MustCompile(`^Backup\s+\[[^\]]*\]\s+(\d+)%`)
	receivedRe = regexp.MustCompile(`Received (\d+) files from device`)
	errCodeRe  = regexp.MustCompile(`ErrorCode (\d+): ?(.*)`)
)

// parseLine interprets one line of idevicebackup2 output; it updates res and returns
// the phase and overall progress it implies (empty phase / -1 when unchanged).
func parseLine(line string, res *backupResult) (phase string, progress int) {
	progress = -1
	switch {
	case strings.Contains(line, "Waiting for passcode"):
		phase = PhasePasscode
	case overallRe.MatchString(line):
		p, _ := strconv.Atoi(overallRe.FindStringSubmatch(line)[1])
		phase, progress = PhaseTransfer, p
	case receivedRe.MatchString(line):
		res.files, _ = strconv.ParseInt(receivedRe.FindStringSubmatch(line)[1], 10, 64)
	case strings.Contains(line, "Backup Successful"):
		res.ok = true
	case errCodeRe.MatchString(line):
		g := errCodeRe.FindStringSubmatch(line)
		res.errorCode, _ = strconv.Atoi(g[1])
		res.err = strings.TrimSpace(line)
	case strings.HasPrefix(line, "ERROR:") || strings.Contains(line, "No device found"):
		if res.err == "" {
			res.err = strings.TrimSpace(line)
		}
	}
	return phase, progress
}

// exec runs idevicebackup2 under a pty (it buffers progress output otherwise).
func (r *Runner) exec(ctx context.Context, udid string) backupResult {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, "idevicebackup2", "-n", "-u", udid, "backup", r.app.backupDir)
	f, err := pty.Start(cmd)
	if err != nil {
		return backupResult{err: err.Error()}
	}
	defer f.Close()

	var res backupResult
	var logLines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	sc.Split(scanLinesCR)
	for sc.Scan() {
		line := strings.TrimSpace(ansiRe.ReplaceAllString(sc.Text(), ""))
		if line == "" {
			continue
		}
		phase, progress := parseLine(line, &res)
		if phase != "" {
			r.update(udid, func(l *Live) bool {
				changed := l.Phase != phase || (progress >= 0 && progress != l.Progress)
				l.Phase = phase
				if progress >= 0 {
					l.Progress = progress
				}
				return changed
			})
		}
		// Keep the log readable: skip per-file and progress-bar noise.
		if !strings.HasPrefix(line, "Receiving") && !strings.HasPrefix(line, "Sending") && !strings.HasPrefix(line, "[") &&
			!strings.HasPrefix(line, "Backup ") && !strings.HasPrefix(line, "Status") && len(logLines) < 400 {
			if n := len(logLines); n == 0 || logLines[n-1] != line {
				logLines = append(logLines, line)
			}
		}
	}
	waitErr := cmd.Wait()
	res.log = strings.Join(logLines, "\n")
	if !res.ok && res.err == "" {
		res.err = "idevicebackup2 failed"
		if waitErr != nil {
			res.err = waitErr.Error()
		}
		if ctx.Err() != nil {
			res.err = "timed out"
		}
	}
	return res
}

// scanLinesCR splits on \n and \r so progress redraws become separate lines.
func scanLinesCR(data []byte, atEOF bool) (int, []byte, error) {
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

// Snapshots

func (a *App) snapshotDir(udid string) string {
	return filepath.Join(a.backupDir, ".snapshots", udid)
}

type Version struct {
	Path string
	Time time.Time
	Live bool
}

// Versions lists restorable copies of a device, newest first.
func (a *App) Versions(d *Device) []Version {
	var out []Version
	live := filepath.Join(a.backupDir, d.UDID)
	if d.LastOK > 0 && fileExists(filepath.Join(live, "Manifest.db")) {
		out = append(out, Version{live, time.Unix(d.LastOK, 0), true})
	}
	entries, _ := os.ReadDir(a.snapshotDir(d.UDID))
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, e := range entries {
		ts := strings.TrimPrefix(e.Name(), d.UDID+"-")
		t, err := time.ParseInLocation("20060102-150405", ts, time.Local)
		if err != nil || !e.IsDir() {
			continue
		}
		out = append(out, Version{filepath.Join(a.snapshotDir(d.UDID), e.Name()), t, false})
	}
	return out
}

func (a *App) pruneSnapshots(udid string, keep int) {
	entries, _ := os.ReadDir(a.snapshotDir(udid))
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), udid+"-") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, n := range names {
		if i >= keep {
			os.RemoveAll(filepath.Join(a.snapshotDir(udid), n))
		}
	}
}

// checkReflink reports whether dir supports copy-on-write copies (Btrfs, XFS).
func checkReflink(dir string) bool {
	src := filepath.Join(dir, ".reflink-test")
	dst := src + ".copy"
	defer os.Remove(src)
	defer os.Remove(dst)
	if err := os.WriteFile(src, []byte("iowncloud"), 0o600); err != nil {
		return false
	}
	return exec.Command("cp", "--reflink=always", src, dst).Run() == nil
}

func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// enableEncryption turns on backup encryption with a user-chosen password.
// The password only lives in the child's environment; it is never stored.
func (a *App) enableEncryption(udid, password string) error {
	out, _ := runCmd(context.Background(), 3*time.Minute, []string{"BACKUP_PASSWORD=" + password},
		"idevicebackup2", "-n", "-u", udid, "encryption", "on")
	if !strings.Contains(out, "enabled successfully") && !strings.Contains(out, "already enabled") {
		return errors.New(strings.TrimSpace(ansiRe.ReplaceAllString(out, "")))
	}
	return a.refreshDeviceInfo(udid)
}
