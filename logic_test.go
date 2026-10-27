package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReminderRules(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 26, h, m, 0, 0, time.Local) }
	if !inWindow(at(18, 0), "18:00", "22:00") || inWindow(at(22, 0), "18:00", "22:00") || inWindow(at(9, 0), "18:00", "22:00") {
		t.Error("plain window")
	}
	if !inWindow(at(23, 30), "22:00", "02:00") || !inWindow(at(1, 0), "22:00", "02:00") || inWindow(at(3, 0), "22:00", "02:00") {
		t.Error("window over midnight")
	}

	now := at(19, 0)
	d := &Device{IntervalHours: 72, WindowStart: "18:00", WindowEnd: "22:00"}
	if !shouldRemind(now, d, true, false) {
		t.Error("never backed up, online, in window: remind")
	}
	if shouldRemind(now, d, false, false) || shouldRemind(now, d, true, true) {
		t.Error("offline or busy: no reminder")
	}
	d.LastOK = now.Add(-24 * time.Hour).Unix()
	if shouldRemind(now, d, true, false) {
		t.Error("not due yet")
	}
	d.LastOK = now.Add(-80 * time.Hour).Unix()
	d.LastReminded = now.Add(-2 * time.Hour).Unix()
	if shouldRemind(now, d, true, false) {
		t.Error("already reminded today")
	}
	d.LastReminded = now.Add(-25 * time.Hour).Unix()
	if !shouldRemind(now, d, true, false) {
		t.Error("due again next day")
	}

	s := &Device{CreatedAt: now.Add(-10 * 24 * time.Hour).Unix()}
	if !shouldAlertStale(now, s, 7) {
		t.Error("stale since creation")
	}
	s.StaleAlerted = now.Add(-time.Hour).Unix()
	if shouldAlertStale(now, s, 7) {
		t.Error("stale alert at most daily")
	}
}

func TestParseBackupOutput(t *testing.T) {
	// Real output captured from an iOS 27 device.
	out := "Incremental backup mode.\r\n*** Waiting for passcode to be entered on the device ***\r\n" +
		"Backup     [##############................]  47%\rReceiving  ...Library/foo.plist\r\n" +
		"Received 75 files from device.\r\nBackup Successful.\r\n"
	var res backupResult
	var phases []string
	last := -1
	for _, line := range strings.FieldsFunc(out, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if p, pr := parseLine(strings.TrimSpace(line), &res); p != "" {
			phases = append(phases, p)
			if pr >= 0 {
				last = pr
			}
		}
	}
	if strings.Join(phases, ",") != "passcode,transfer" || last != 47 || res.files != 75 || !res.ok {
		t.Errorf("phases=%v progress=%d res=%+v", phases, last, res)
	}

	res = backupResult{}
	parseLine("ErrorCode 208: Device locked (MBErrorDomain/208)", &res)
	if res.errorCode != 208 || res.ok {
		t.Errorf("208: %+v", res)
	}
}

func TestDeviceList(t *testing.T) {
	m := parseDeviceList("00008110-000A1C263410401E (USB)\n00008110-000A1C263410401E (Network)\nabc (USB)\n")
	p := m["00008110-000A1C263410401E"]
	if len(m) != 1 || !p.USB || !p.Net {
		t.Errorf("%+v", m)
	}
}

func TestServiceURL(t *testing.T) {
	u, err := buildServiceURL("ntfy", map[string]string{"Host": "ntfy.example.com", "Topic": "phone"})
	// shoutrrr omits values equal to defaults; a bare config would emit firebase=No and break iOS push.
	if err != nil || !strings.HasPrefix(u, "ntfy://ntfy.example.com/phone") || strings.Contains(strings.ToLower(u), "firebase=no") {
		t.Errorf("ntfy url %q err %v (defaults must be applied)", u, err)
	}
	if _, err := buildServiceURL("telegram", map[string]string{"Token": "1:a"}); err == nil {
		t.Error("missing required chats must fail")
	}
	u, _ = buildServiceURL("telegram", map[string]string{"Token": "123:abc", "Chats": "456"})
	bot, chats, err := parseTelegramURL(u)
	if err != nil || bot != "123:abc" || len(chats) != 1 || chats[0] != "456" {
		t.Errorf("telegram %q -> %q %v %v", u, bot, chats, err)
	}
}

func TestNtfyActionButton(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	a := &App{}
	m := message{Title: "t", Body: "b", Button: "Back up now", Link: "http://nas:8080/go/abc"}
	if err := a.sendShoutrrr("ntfy://"+u.Host+"/phone?scheme=http", m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Get("Actions"), "http, Back up now, http://nas:8080/go/abc, method=POST") {
		t.Errorf("Actions header: %q", got.Get("Actions"))
	}
}

func TestLocalesComplete(t *testing.T) {
	b, err := loadBundle(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	en := b.LanguageTags()
	if len(en) < 2 {
		t.Fatalf("languages: %v", en)
	}
	a := &App{bundle: b}
	for _, id := range []string{"notify.reminder.title", "time.days_ago", "job.status.nopasscode", "phase.passcode"} {
		if s := trn(a.localizer("pl"), id, 5, map[string]any{"Device": "x"}); s == id {
			t.Errorf("pl missing %s", id)
		}
	}
	if got := trn(a.localizer("pl"), "time.minutes_ago", 3, nil); got != "3 minuty temu" {
		t.Errorf("polish plural: %q", got)
	}
}
