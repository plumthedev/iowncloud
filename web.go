package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "iowncloud_session"

// Live events (SSE)

type LiveEvent struct {
	Kind     string `json:"kind"` // job, presence
	UDID     string `json:"udid,omitempty"`
	JobID    int64  `json:"job,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Progress int    `json:"progress"`
	Status   string `json:"status,omitempty"`
	Text     string `json:"text,omitempty"`
}

type Broker struct {
	mu   sync.Mutex
	subs map[chan LiveEvent]bool
}

func newBroker() *Broker { return &Broker{subs: map[chan LiveEvent]bool{}} }

func (b *Broker) Publish(e LiveEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // slow client; it will catch up on the next event
		}
	}
}

func (b *Broker) subscribe() chan LiveEvent {
	ch := make(chan LiveEvent, 32)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan LiveEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// Templates

type pageTemplate struct{ t *template.Template }

func templateFuncs() template.FuncMap {
	kv := func(args []any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(args); i += 2 {
			m[fmt.Sprint(args[i])] = args[i+1]
		}
		return m
	}
	return template.FuncMap{
		"t":     func(l *i18n.Localizer, id string, args ...any) string { return tr(l, id, kv(args)) },
		"tn":    func(l *i18n.Localizer, id string, n int, args ...any) string { return trn(l, id, n, kv(args)) },
		"ago":   ago,
		"dur":   func(secs int64) string { return duration(nil, secs) },
		"bytes": humanBytes,
		"model": modelName,
		"label": deviceLabel,
		"phase": phaseText,
		"short": shortUDID,
		"date":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"unixdate": func(u int64) string {
			if u == 0 {
				return ""
			}
			return time.Unix(u, 0).Format("2006-01-02 15:04")
		},
		"days": func(h int) int { return max(1, h/24) },
		"sub":  func(a, b int64) int64 { return a - b },
		"int":  func(n int64) int { return int(n) },
		"navActive": func(path, href string) bool {
			if href == "/" {
				return path == "/" || strings.HasPrefix(path, "/devices")
			}
			return strings.HasPrefix(path, href) || (href == "/activity" && strings.HasPrefix(path, "/jobs"))
		},
		"dict":   func(args ...any) map[string]any { return kv(args) },
		"redact": redactURL,
		"scheme": func(u string) string { s, _, _ := strings.Cut(u, "://"); return s },
	}
}

func parsePages(web fs.FS) (map[string]*pageTemplate, error) {
	files, err := fs.Glob(web, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*pageTemplate{}
	for _, f := range files {
		t, err := template.New("").Funcs(templateFuncs()).ParseFS(web, "templates/layout.html", "templates/partials.html", f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		pages[strings.TrimSuffix(path.Base(f), ".html")] = &pageTemplate{t}
	}
	return pages, nil
}

type view map[string]any

func (a *App) requestLocalizer(r *http.Request) (*i18n.Localizer, string) {
	lang := ""
	if c, err := r.Cookie("lang"); err == nil {
		lang = c.Value
	}
	l := a.localizer(lang, r.Header.Get("Accept-Language"))
	// Resolve which language actually won, for <html lang> and the switcher.
	_, tag, _ := l.LocalizeWithTag(&i18n.LocalizeConfig{MessageID: "app.name"})
	return l, tag.String()
}

func (a *App) render(w http.ResponseWriter, r *http.Request, page, block string, v view) {
	p := a.pages[page]
	if p == nil {
		http.Error(w, "no page "+page, 500)
		return
	}
	l, lang := a.requestLocalizer(r)
	if v == nil {
		v = view{}
	}
	v["L"], v["Lang"], v["Languages"] = l, lang, languages
	v["Path"], v["Version"], v["AuthOn"] = r.URL.Path, version, !a.authOff
	v["Flash"] = r.URL.Query().Get("ok")
	v["Bare"] = page == "login" || page == "setup" || page == "action"
	var buf bytes.Buffer
	if err := p.t.ExecuteTemplate(&buf, block, v); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (a *App) page(w http.ResponseWriter, r *http.Request, page string, v view) {
	a.render(w, r, page, "layout", v)
}

// Routes

func (a *App) routes(web fs.FS) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(web, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static))))
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		f, _ := static.Open("manifest.webmanifest")
		io.Copy(w, f)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /setup", a.setupPage)
	mux.HandleFunc("POST /setup", a.setup)
	mux.HandleFunc("POST /lang", a.setLang)

	mux.HandleFunc("GET /go/{token}", a.actionPage)
	mux.HandleFunc("POST /go/{token}", a.actionStart)

	mux.HandleFunc("GET /{$}", a.devicesPage)
	mux.HandleFunc("GET /events", a.events)
	mux.HandleFunc("GET /devices/add", a.addDevicePage)
	mux.HandleFunc("GET /devices/add/usb", a.usbPartial)
	mux.HandleFunc("POST /devices/pair", a.pair)
	mux.HandleFunc("POST /devices/upload", a.uploadPairing)
	mux.HandleFunc("GET /devices/{udid}", a.devicePage)
	mux.HandleFunc("GET /devices/{udid}/card", a.deviceCard)
	mux.HandleFunc("POST /devices/{udid}/backup", a.backupNow)
	mux.HandleFunc("POST /devices/{udid}/schedule", a.saveSchedule)
	mux.HandleFunc("POST /devices/{udid}/encryption", a.encryption)
	mux.HandleFunc("POST /devices/{udid}/remove", a.removeDevice)
	mux.HandleFunc("GET /activity", a.activityPage)
	mux.HandleFunc("GET /jobs/{id}", a.jobPage)
	mux.HandleFunc("GET /notifications", a.notificationsPage)
	mux.HandleFunc("GET /notifications/form", a.channelForm)
	mux.HandleFunc("POST /notifications", a.addChannel)
	mux.HandleFunc("POST /notifications/{id}/test", a.testChannel)
	mux.HandleFunc("POST /notifications/{id}/toggle", a.toggleChannel)
	mux.HandleFunc("POST /notifications/{id}/delete", a.deleteChannel)
	mux.HandleFunc("GET /settings", a.settingsPage)
	mux.HandleFunc("POST /settings", a.saveSettings)
	mux.HandleFunc("GET /restore", func(w http.ResponseWriter, r *http.Request) { a.page(w, r, "restore", nil) })
	return a.auth(mux)
}

func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

var publicPrefixes = []string{"/static/", "/go/", "/login", "/setup", "/lang", "/healthz", "/manifest.webmanifest"}

func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if a.authOff {
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range publicPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if a.store.Setting("password_hash", "") == "" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil && a.store.ValidSession(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// Auth pages

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, "login", nil)
}

var loginLimiter = struct {
	sync.Mutex
	last time.Time
}{}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	// ponytail: global 1s spacing between attempts; per-IP limits if this is ever exposed publicly.
	loginLimiter.Lock()
	if wait := time.Second - time.Since(loginLimiter.last); wait > 0 {
		time.Sleep(wait)
	}
	loginLimiter.last = time.Now()
	loginLimiter.Unlock()

	hash := a.store.Setting("password_hash", "")
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		a.page(w, r, "login", view{"Error": true})
		return
	}
	a.startSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) startSession(w http.ResponseWriter) {
	tok, _ := a.store.NewSession(90 * 24 * time.Hour)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 90 * 24 * 3600})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	if a.store.Setting("password_hash", "") != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.page(w, r, "setup", nil)
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	if a.store.Setting("password_hash", "") != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 8 || pw != r.FormValue("password2") {
		a.page(w, r, "setup", view{"Error": true})
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	a.store.SetSetting("password_hash", string(hash))
	if a.store.Setting("base_url", "") == "" {
		a.store.SetSetting("base_url", requestOrigin(r))
	}
	a.startSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) setLang(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "lang", Value: r.FormValue("lang"), Path: "/", MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode})
	back := r.Referer()
	if back == "" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Devices

type DeviceView struct {
	*Device
	Presence Presence
	Live     *Live
	Health   string // never, ok, due, stale
}

func (a *App) deviceView(d *Device) DeviceView {
	v := DeviceView{Device: d, Presence: a.watcher.Presence(d.UDID), Live: a.runner.Live(d.UDID)}
	age := time.Since(time.Unix(d.LastOK, 0))
	switch {
	case d.LastOK == 0:
		v.Health = "never"
	case age >= time.Duration(a.settingInt("stale_days", 7))*24*time.Hour:
		v.Health = "stale"
	case age >= time.Duration(d.IntervalHours)*time.Hour:
		v.Health = "due"
	default:
		v.Health = "ok"
	}
	return v
}

func (a *App) devicesPage(w http.ResponseWriter, r *http.Request) {
	devices, err := a.store.Devices()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var views []DeviceView
	for _, d := range devices {
		views = append(views, a.deviceView(d))
	}
	a.page(w, r, "devices", view{"Devices": views, "Unpaired": a.watcher.UnpairedUSB(a.lockdownDir), "Muxer": a.muxerStatus()})
}

func (a *App) muxerStatus() MuxerStatus {
	if a.muxerOff {
		return MuxerStatus{Enabled: false}
	}
	return a.muxer.Status()
}

func (a *App) deviceCard(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.Device(r.PathValue("udid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "devices", "device-card", view{"D": a.deviceView(d), "Link": r.URL.Query().Get("detail") == ""})
}

func (a *App) devicePage(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.Device(r.PathValue("udid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, _ := a.store.Jobs(d.UDID, 5)
	a.page(w, r, "device", view{
		"D": a.deviceView(d), "Jobs": jobs, "Versions": a.Versions(d), "Reflink": a.reflink,
		"BackupDir": a.backupDir, "Error": r.URL.Query().Get("err"),
	})
}

func (a *App) backupNow(w http.ResponseWriter, r *http.Request) {
	udid := r.PathValue("udid")
	if _, err := a.store.Device(udid); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := a.runner.Enqueue(udid, "manual"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		a.deviceCard(w, r)
		return
	}
	http.Redirect(w, r, "/devices/"+udid, http.StatusSeeOther)
}

func (a *App) saveSchedule(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.Device(r.PathValue("udid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if days, err := strconv.Atoi(r.FormValue("interval_days")); err == nil && days >= 1 && days <= 60 {
		d.IntervalHours = days * 24
	}
	if keep, err := strconv.Atoi(r.FormValue("keep_versions")); err == nil && keep >= 0 && keep <= 100 {
		d.KeepVersions = keep
	}
	for _, f := range []struct {
		dst  *string
		name string
	}{{&d.WindowStart, "window_start"}, {&d.WindowEnd, "window_end"}} {
		if _, err := time.Parse("15:04", r.FormValue(f.name)); err == nil {
			*f.dst = r.FormValue(f.name)
		}
	}
	if err := a.store.UpdateDeviceSchedule(d); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/devices/"+d.UDID+"?ok=saved", http.StatusSeeOther)
}

func (a *App) encryption(w http.ResponseWriter, r *http.Request) {
	udid := r.PathValue("udid")
	pw := r.FormValue("password")
	l, _ := a.requestLocalizer(r)
	fail := func(msg string) {
		http.Redirect(w, r, "/devices/"+udid+"?err="+url.QueryEscape(msg)+"#encryption", http.StatusSeeOther)
	}
	switch {
	case len(pw) < 8:
		fail(tr(l, "encryption.err_short", nil))
	case pw != r.FormValue("password2"):
		fail(tr(l, "encryption.err_mismatch", nil))
	case r.FormValue("saved") == "":
		fail(tr(l, "encryption.err_saved", nil))
	case a.runner.Busy(udid):
		fail(tr(l, "encryption.err_busy", nil))
	default:
		if err := a.enableEncryption(udid, pw); err != nil {
			fail(err.Error())
			return
		}
		http.Redirect(w, r, "/devices/"+udid+"?ok=encrypted#encryption", http.StatusSeeOther)
	}
}

func (a *App) removeDevice(w http.ResponseWriter, r *http.Request) {
	udid := r.PathValue("udid")
	if !validUDID(udid) || a.runner.Busy(udid) {
		http.Error(w, "busy", http.StatusConflict)
		return
	}
	os.Remove(filepath.Join(a.lockdownDir, udid+".plist"))
	a.store.DeleteDevice(udid)
	http.Redirect(w, r, "/?ok=removed", http.StatusSeeOther)
}

// Adding devices

func (a *App) addDevicePage(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, "add", view{"USB": usbAvailable(), "Unpaired": a.watcher.UnpairedUSB(a.lockdownDir), "Error": r.URL.Query().Get("err")})
}

func (a *App) usbPartial(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "add", "usb-devices", view{"Unpaired": a.watcher.UnpairedUSB(a.lockdownDir)})
}

func (a *App) pair(w http.ResponseWriter, r *http.Request) {
	udid := r.FormValue("udid")
	if !validUDID(udid) {
		http.Error(w, "bad udid", 400)
		return
	}
	res := a.pairUSB(udid)
	a.render(w, r, "add", "pair-result", view{"UDID": udid, "Result": res})
}

func (a *App) uploadPairing(w http.ResponseWriter, r *http.Request) {
	l, _ := a.requestLocalizer(r)
	fail := func(key string) {
		http.Redirect(w, r, "/devices/add?err="+url.QueryEscape(tr(l, key, nil))+"#upload", http.StatusSeeOther)
	}
	f, hdr, err := r.FormFile("record")
	if err != nil {
		fail("add.upload_err_missing")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		fail("add.upload_err_missing")
		return
	}
	udid := strings.TrimSuffix(filepath.Base(hdr.Filename), filepath.Ext(hdr.Filename))
	if !validUDID(udid) {
		fail("add.upload_err_name")
		return
	}
	// XML and binary plists both store keys as plain ASCII.
	if !bytes.Contains(data, []byte("HostCertificate")) || !bytes.Contains(data, []byte("HostPrivateKey")) {
		fail("add.upload_err_invalid")
		return
	}
	if err := os.WriteFile(filepath.Join(a.lockdownDir, udid+".plist"), data, 0o600); err != nil {
		fail("add.upload_err_invalid")
		return
	}
	a.store.EnsureDevice(udid)
	http.Redirect(w, r, "/devices/"+udid+"?ok=paired", http.StatusSeeOther)
}

// Activity

func (a *App) activityPage(w http.ResponseWriter, r *http.Request) {
	jobs, err := a.store.Jobs("", 100)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.page(w, r, "activity", view{"Jobs": jobs})
}

func (a *App) jobPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j, err := a.store.Job(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.page(w, r, "job", view{"J": j})
}

// Notifications

func (a *App) notificationsPage(w http.ResponseWriter, r *http.Request) {
	chans, _ := a.store.Channels()
	a.page(w, r, "notifications", view{
		"Channels": chans, "Services": notificationServices(), "BaseURL": a.baseURL(),
		"Error": r.URL.Query().Get("err"), "TestErr": r.URL.Query().Get("testerr"),
	})
}

func (a *App) channelForm(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	fields, err := serviceForm(svc)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.render(w, r, "notifications", "channel-fields", view{"Service": svc, "Fields": fields})
}

func (a *App) addChannel(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	svc := r.FormValue("service")
	raw := strings.TrimSpace(r.FormValue("raw_url"))
	var u string
	var err error
	if raw != "" {
		u, svc = raw, strings.Split(raw, "://")[0]
	} else {
		values := map[string]string{}
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "f_") && len(v) > 0 {
				values[strings.TrimPrefix(k, "f_")] = v[len(v)-1]
			}
		}
		u, err = buildServiceURL(svc, values)
	}
	if err == nil {
		_, err = notifySender(u)
	}
	if err != nil {
		http.Redirect(w, r, "/notifications?err="+url.QueryEscape(err.Error())+"#add", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = svc
	}
	a.store.AddChannel(&Channel{Name: name, URL: u, OnSuccess: r.FormValue("on_success") != ""})
	a.syncTelegram(a.ctx)
	http.Redirect(w, r, "/notifications?ok=channel_added", http.StatusSeeOther)
}

func (a *App) channelFromPath(r *http.Request) (*Channel, error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return a.store.Channel(id)
}

func (a *App) testChannel(w http.ResponseWriter, r *http.Request) {
	c, err := a.channelFromPath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.sendTo(c, Event{Kind: EvTest}); err != nil {
		http.Redirect(w, r, "/notifications?testerr="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/notifications?ok=test_sent", http.StatusSeeOther)
}

func (a *App) toggleChannel(w http.ResponseWriter, r *http.Request) {
	c, err := a.channelFromPath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch r.FormValue("field") {
	case "enabled":
		a.store.SetChannelFlag(c.ID, "enabled", !c.Enabled)
	case "on_success":
		a.store.SetChannelFlag(c.ID, "on_success", !c.OnSuccess)
	}
	a.syncTelegram(a.ctx)
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

func (a *App) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if c, err := a.channelFromPath(r); err == nil {
		a.store.DeleteChannel(c.ID)
		a.syncTelegram(a.ctx)
	}
	http.Redirect(w, r, "/notifications?ok=channel_deleted", http.StatusSeeOther)
}

// Settings

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, "settings", view{
		"BaseURL": a.baseURL(), "Origin": requestOrigin(r), "NotifyLang": a.store.Setting("language", "en"),
		"StaleDays": a.settingInt("stale_days", 7), "Muxer": a.muxerStatus(), "USB": usbAvailable(),
		"Reflink": a.reflink, "Free": freeBytes(a.backupDir), "BackupDir": a.backupDir,
	})
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	if u := strings.TrimRight(strings.TrimSpace(r.FormValue("base_url")), "/"); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		a.store.SetSetting("base_url", u)
	}
	for _, l := range languages {
		if r.FormValue("language") == l.Code {
			a.store.SetSetting("language", l.Code)
		}
	}
	if n, err := strconv.Atoi(r.FormValue("stale_days")); err == nil && n >= 1 && n <= 365 {
		a.store.SetSetting("stale_days", strconv.Itoa(n))
	}
	http.Redirect(w, r, "/settings?ok=saved", http.StatusSeeOther)
}

// One-time action links from notifications

func (a *App) actionPage(w http.ResponseWriter, r *http.Request) {
	udid, err := a.store.PeekActionToken(r.PathValue("token"))
	v := view{"Token": r.PathValue("token"), "Valid": err == nil}
	if err == nil {
		if d, err := a.store.Device(udid); err == nil {
			v["D"] = d
		}
	}
	a.page(w, r, "action", v)
}

func (a *App) actionStart(w http.ResponseWriter, r *http.Request) {
	_, _, err := a.startFromToken(r.PathValue("token"), "link")
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(map[string]bool{"ok": false})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// SSE

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	l, _ := a.requestLocalizer(r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := a.broker.subscribe()
	defer a.broker.unsubscribe(ch)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, ": hi\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case e := <-ch:
			if e.Phase != "" && e.Phase != "done" {
				e.Text = phaseText(l, e.Phase)
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		flusher.Flush()
	}
}
