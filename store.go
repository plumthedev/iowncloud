package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Device struct {
	UDID          string
	Name          string
	ProductType   string
	IOS           string
	Encrypted     int // -1 unknown, 0 no, 1 yes
	IntervalHours int
	WindowStart   string
	WindowEnd     string
	KeepVersions  int
	InfoAt        int64
	LastSeen      int64
	LastReminded  int64
	StaleAlerted  int64
	CreatedAt     int64

	LastOK  int64 // finished_at of the newest successful job, filled by queries
	LastJob *Job
}

type Job struct {
	ID         int64
	UDID       string
	Trigger    string
	Status     string
	StartedAt  int64
	FinishedAt int64
	Files      int64
	Bytes      int64
	Snapshot   string
	Error      string
	Log        string
	CreatedAt  int64

	DeviceName string
}

type Channel struct {
	ID        int64
	Name      string
	URL       string
	Enabled   bool
	OnSuccess bool
}

const (
	JobQueued     = "queued"
	JobRunning    = "running"
	JobOK         = "ok"
	JobFailed     = "failed"
	JobNoPasscode = "nopasscode"
)

func openStore(path string, migrations fs.FS) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// ponytail: one connection serializes all writes; fine for a handful of devices.
	db.SetMaxOpenConns(1)
	s := &Store{db}
	return s, s.migrate(migrations)
}

// migrate applies migrations/NNN_*.sql files whose number is above PRAGMA user_version.
func (s *Store) migrate(migrations fs.FS) error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		var n int
		if _, err := fmt.Sscanf(f, "migrations/%d_", &n); err != nil || n <= version {
			continue
		}
		body, err := fs.ReadFile(migrations, f)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", f, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() int64 { return time.Now().Unix() }

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Settings

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// Devices

const deviceCols = `udid, name, product_type, ios, encrypted, interval_hours, window_start, window_end,
	keep_versions, info_at, last_seen, last_reminded, stale_alerted, created_at,
	COALESCE((SELECT MAX(finished_at) FROM jobs WHERE jobs.udid = devices.udid AND status = 'ok'), 0)`

func scanDevice(row interface{ Scan(...any) error }) (*Device, error) {
	d := &Device{}
	err := row.Scan(&d.UDID, &d.Name, &d.ProductType, &d.IOS, &d.Encrypted, &d.IntervalHours, &d.WindowStart, &d.WindowEnd,
		&d.KeepVersions, &d.InfoAt, &d.LastSeen, &d.LastReminded, &d.StaleAlerted, &d.CreatedAt, &d.LastOK)
	return d, err
}

func (s *Store) Devices() ([]*Device, error) {
	rows, err := s.db.Query("SELECT " + deviceCols + " FROM devices ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		d.LastJob, _ = s.LastJob(d.UDID)
	}
	return out, nil
}

func (s *Store) Device(udid string) (*Device, error) {
	d, err := scanDevice(s.db.QueryRow("SELECT "+deviceCols+" FROM devices WHERE udid = ?", udid))
	if err != nil {
		return nil, err
	}
	d.LastJob, _ = s.LastJob(udid)
	return d, nil
}

// EnsureDevice registers a paired device; returns true when it was new.
func (s *Store) EnsureDevice(udid string) (bool, error) {
	res, err := s.db.Exec("INSERT OR IGNORE INTO devices (udid, created_at) VALUES (?, ?)", udid, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) UpdateDeviceInfo(udid, name, productType, ios string, encrypted int) error {
	_, err := s.db.Exec("UPDATE devices SET name = ?, product_type = ?, ios = ?, encrypted = ?, info_at = ? WHERE udid = ?",
		name, productType, ios, encrypted, now(), udid)
	return err
}

func (s *Store) UpdateDeviceSchedule(d *Device) error {
	_, err := s.db.Exec("UPDATE devices SET interval_hours = ?, window_start = ?, window_end = ?, keep_versions = ? WHERE udid = ?",
		d.IntervalHours, d.WindowStart, d.WindowEnd, d.KeepVersions, d.UDID)
	return err
}

func (s *Store) TouchDevice(udid string) error {
	_, err := s.db.Exec("UPDATE devices SET last_seen = ? WHERE udid = ?", now(), udid)
	return err
}

func (s *Store) SetDeviceTime(udid, column string, t int64) error {
	if column != "last_reminded" && column != "stale_alerted" {
		return errors.New("bad column")
	}
	_, err := s.db.Exec("UPDATE devices SET "+column+" = ? WHERE udid = ?", t, udid)
	return err
}

func (s *Store) DeleteDevice(udid string) error {
	_, err := s.db.Exec("DELETE FROM devices WHERE udid = ?", udid)
	return err
}

// Jobs

const jobCols = `jobs.id, jobs.udid, trigger, status, started_at, finished_at, files, bytes, snapshot, error, log, jobs.created_at,
	COALESCE(NULLIF(devices.name, ''), jobs.udid)`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	j := &Job{}
	err := row.Scan(&j.ID, &j.UDID, &j.Trigger, &j.Status, &j.StartedAt, &j.FinishedAt, &j.Files, &j.Bytes,
		&j.Snapshot, &j.Error, &j.Log, &j.CreatedAt, &j.DeviceName)
	return j, err
}

func (s *Store) queryJobs(where string, args ...any) ([]*Job, error) {
	rows, err := s.db.Query("SELECT "+jobCols+" FROM jobs LEFT JOIN devices ON devices.udid = jobs.udid "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) Jobs(udid string, limit int) ([]*Job, error) {
	if udid == "" {
		return s.queryJobs("ORDER BY jobs.id DESC LIMIT ?", limit)
	}
	return s.queryJobs("WHERE jobs.udid = ? ORDER BY jobs.id DESC LIMIT ?", udid, limit)
}

func (s *Store) Job(id int64) (*Job, error) {
	jobs, err := s.queryJobs("WHERE jobs.id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, sql.ErrNoRows
	}
	return jobs[0], nil
}

func (s *Store) LastJob(udid string) (*Job, error) {
	jobs, err := s.Jobs(udid, 1)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], nil
}

// ActiveJob returns the queued or running job of a device, if any.
func (s *Store) ActiveJob(udid string) (*Job, error) {
	jobs, err := s.queryJobs("WHERE jobs.udid = ? AND status IN ('queued', 'running') ORDER BY jobs.id DESC LIMIT 1", udid)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], nil
}

func (s *Store) CreateJob(udid, trigger string) (int64, error) {
	res, err := s.db.Exec("INSERT INTO jobs (udid, trigger, status, created_at) VALUES (?, ?, ?, ?)", udid, trigger, JobQueued, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) StartJob(id int64, snapshot string) error {
	_, err := s.db.Exec("UPDATE jobs SET status = ?, started_at = ?, snapshot = ? WHERE id = ?", JobRunning, now(), snapshot, id)
	return err
}

func (s *Store) FinishJob(j *Job) error {
	j.FinishedAt = now()
	_, err := s.db.Exec("UPDATE jobs SET status = ?, finished_at = ?, files = ?, bytes = ?, snapshot = ?, error = ?, log = ? WHERE id = ?",
		j.Status, j.FinishedAt, j.Files, j.Bytes, j.Snapshot, j.Error, j.Log, j.ID)
	return err
}

// FailStaleJobs marks jobs left running by a crash or restart as failed.
func (s *Store) FailStaleJobs() error {
	_, err := s.db.Exec("UPDATE jobs SET status = ?, error = 'interrupted by restart', finished_at = ? WHERE status IN ('queued', 'running')", JobFailed, now())
	return err
}

// Channels

func (s *Store) Channels() ([]*Channel, error) {
	rows, err := s.db.Query("SELECT id, name, url, enabled, on_success FROM channels ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Channel
	for rows.Next() {
		c := &Channel{}
		if err := rows.Scan(&c.ID, &c.Name, &c.URL, &c.Enabled, &c.OnSuccess); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Channel(id int64) (*Channel, error) {
	c := &Channel{}
	err := s.db.QueryRow("SELECT id, name, url, enabled, on_success FROM channels WHERE id = ?", id).
		Scan(&c.ID, &c.Name, &c.URL, &c.Enabled, &c.OnSuccess)
	return c, err
}

func (s *Store) AddChannel(c *Channel) error {
	_, err := s.db.Exec("INSERT INTO channels (name, url, enabled, on_success) VALUES (?, ?, 1, ?)", c.Name, c.URL, c.OnSuccess)
	return err
}

func (s *Store) SetChannelFlag(id int64, column string, v bool) error {
	if column != "enabled" && column != "on_success" {
		return errors.New("bad column")
	}
	_, err := s.db.Exec("UPDATE channels SET "+column+" = ? WHERE id = ?", v, id)
	return err
}

func (s *Store) DeleteChannel(id int64) error {
	_, err := s.db.Exec("DELETE FROM channels WHERE id = ?", id)
	return err
}

// Action tokens: one-time links that start a backup.

func (s *Store) NewActionToken(udid string, ttl time.Duration) (string, error) {
	t := randomToken()
	_, err := s.db.Exec("INSERT INTO action_tokens (token, udid, expires_at) VALUES (?, ?, ?)", t, udid, time.Now().Add(ttl).Unix())
	return t, err
}

// PeekActionToken returns the device of a valid, unused token without consuming it.
func (s *Store) PeekActionToken(token string) (string, error) {
	var udid string
	err := s.db.QueryRow("SELECT udid FROM action_tokens WHERE token = ? AND used_at = 0 AND expires_at > ?", token, now()).Scan(&udid)
	return udid, err
}

func (s *Store) ConsumeActionToken(token string) (string, error) {
	var udid string
	err := s.db.QueryRow("UPDATE action_tokens SET used_at = ? WHERE token = ? AND used_at = 0 AND expires_at > ? RETURNING udid",
		now(), token, now()).Scan(&udid)
	return udid, err
}

// Sessions

func (s *Store) NewSession(ttl time.Duration) (string, error) {
	t := randomToken()
	_, err := s.db.Exec("INSERT INTO sessions (token, expires_at) VALUES (?, ?)", t, time.Now().Add(ttl).Unix())
	return t, err
}

func (s *Store) ValidSession(token string) bool {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token = ? AND expires_at > ?", token, now()).Scan(&n)
	return n > 0
}

func (s *Store) DeleteSession(token string) {
	s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
}

func (s *Store) Cleanup() {
	s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", now())
	s.db.Exec("DELETE FROM action_tokens WHERE expires_at < ?", now()-86400)
}
