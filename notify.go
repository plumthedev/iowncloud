package main

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr/pkg/format"
	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"
)

const (
	EvReminder   = "reminder"
	EvOK         = "ok"
	EvFailed     = "failed"
	EvNoPasscode = "nopasscode"
	EvStale      = "stale"
	EvMuxer      = "muxer"
	EvTest       = "test"
)

type Event struct {
	Kind   string
	Device *Device
	Job    *Job
	Token  string // one-time "start backup" token, adds an action button
	Detail string
}

// notify fans an event out to every enabled channel.
func (a *App) notify(ev Event) {
	chans, err := a.store.Channels()
	if err != nil {
		log.Printf("notify: %v", err)
		return
	}
	for _, c := range chans {
		if !c.Enabled || (ev.Kind == EvOK && !c.OnSuccess) {
			continue
		}
		go func(c *Channel) {
			if err := a.sendTo(c, ev); err != nil {
				log.Printf("notify %s: %v", c.Name, err)
			}
		}(c)
	}
}

type message struct {
	Title, Body, Button, Link string
}

func (a *App) renderEvent(ev Event) message {
	l := a.localizer(a.store.Setting("language", "en"))
	data := map[string]any{}
	if ev.Device != nil {
		data["Device"] = deviceLabel(ev.Device)
		data["Ago"] = ago(l, ev.Device.LastOK)
	}
	if ev.Job != nil {
		data["Duration"] = duration(l, ev.Job.FinishedAt-ev.Job.StartedAt)
		data["Files"] = ev.Job.Files
		data["Size"] = humanBytes(ev.Job.Bytes)
		data["Error"] = ev.Job.Error
	}
	data["Detail"] = ev.Detail
	m := message{
		Title: tr(l, "notify."+ev.Kind+".title", data),
		Body:  tr(l, "notify."+ev.Kind+".body", data),
	}
	if ev.Token != "" {
		m.Button = tr(l, "notify.button", nil)
		m.Link = a.baseURL() + "/go/" + ev.Token
	}
	return m
}

func (a *App) sendTo(c *Channel, ev Event) error {
	m := a.renderEvent(ev)
	if strings.HasPrefix(c.URL, "telegram://") {
		return a.tgSend(c.URL, m, ev)
	}
	return a.sendShoutrrr(c.URL, m)
}

func (a *App) sendShoutrrr(channelURL string, m message) error {
	params := types.Params{}
	params.SetTitle(m.Title)
	body := m.Body
	if m.Link != "" {
		if strings.HasPrefix(channelURL, "ntfy://") {
			// Native button: the ntfy app POSTs to our link without opening anything.
			params["actions"] = fmt.Sprintf("http, %s, %s, method=POST, clear=true", m.Button, m.Link)
			params["click"] = m.Link
		} else {
			body += "\n\n" + m.Button + ": " + m.Link
		}
	}
	sender, err := router.New(nil, channelURL)
	if err != nil {
		return err
	}
	return errors.Join(sender.Send(body, &params)...)
}

// Channel forms are generated from shoutrrr's own config metadata.

type FormField struct {
	Key      string
	Label    string
	Help     string
	Default  string
	Required bool
	Bool     bool
	Options  []string
	Advanced bool
	urlPart  bool
}

// popular services are listed first in the picker.
var popularServices = []string{"ntfy", "telegram", "discord", "smtp", "pushover", "gotify", "signal", "slack", "matrix", "generic"}

func notificationServices() []string {
	all := (&router.ServiceRouter{}).ListServices()
	slices.Sort(all)
	out := slices.Clone(popularServices)
	for _, s := range all {
		if !slices.Contains(out, s) && s != "logger" {
			out = append(out, s)
		}
	}
	return out
}

func serviceForm(scheme string) ([]FormField, error) {
	svc, err := (&router.ServiceRouter{}).NewService(scheme)
	if err != nil {
		return nil, err
	}
	var out []FormField
	for _, n := range format.GetServiceConfigFormat(svc).Items {
		f := n.Field()
		if f.Type.Kind() == reflect.Struct || f.Type.Kind() == reflect.Map {
			continue
		}
		ff := FormField{
			Key:      f.Name,
			Label:    splitCamel(f.Name),
			Help:     f.Description,
			Default:  f.DefaultValue,
			Required: f.Required,
			Bool:     f.Type.Kind() == reflect.Bool,
			Advanced: !f.Required && len(f.URLParts) == 0,
			urlPart:  len(f.URLParts) > 0,
		}
		if f.IsEnum() {
			ff.Options = f.EnumFormatter.Names()
		}
		out = append(out, ff)
	}
	slices.SortStableFunc(out, func(x, y FormField) int { return fieldRank(x) - fieldRank(y) })
	return out, nil
}

// fieldRank orders form fields: required credentials, other required, optional URL parts, advanced.
func fieldRank(f FormField) int {
	switch {
	case f.Required && f.urlPart:
		return 0
	case f.Required:
		return 1
	case !f.Advanced:
		return 2
	default:
		return 3
	}
}

// buildServiceURL turns form values into a shoutrrr URL, applying shoutrrr defaults
// first (a bare config would e.g. disable Firebase delivery for ntfy).
func buildServiceURL(scheme string, values map[string]string) (string, error) {
	svc, err := (&router.ServiceRouter{}).NewService(scheme)
	if err != nil {
		return "", err
	}
	cfg := format.GetServiceConfig(svc)
	cv := reflect.Indirect(reflect.ValueOf(cfg))
	for _, n := range format.GetConfigFormat(cfg).Items {
		f := n.Field()
		v := strings.TrimSpace(values[f.Name])
		if v == "" {
			if f.Required {
				return "", fmt.Errorf("%s: %w", splitCamel(f.Name), errRequired)
			}
			v = f.DefaultValue
		}
		if v == "" {
			continue
		}
		if _, err := format.SetConfigField(cv, f, v); err != nil {
			return "", fmt.Errorf("%s: %w", splitCamel(f.Name), err)
		}
	}
	u := cfg.GetURL()
	if u == nil {
		return "", errors.New("could not build URL")
	}
	return u.String(), nil
}

var errRequired = errors.New("required")

// notifySender validates a shoutrrr URL.
func notifySender(u string) (*router.ServiceRouter, error) { return router.New(nil, u) }

func splitCamel(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' && !(s[i-1] >= 'A' && s[i-1] <= 'Z') {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// redactURL hides credentials of a channel URL for display.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "…"
	}
	if u.User != nil {
		u.User = url.User("•••")
	}
	q := u.Query()
	for k := range q {
		if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "pass") || strings.Contains(strings.ToLower(k), "key") {
			q.Set(k, "•••")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Scheduler: reminders and stale alerts.

func inWindow(t time.Time, start, end string) bool {
	s, err1 := time.Parse("15:04", start)
	e, err2 := time.Parse("15:04", end)
	if err1 != nil || err2 != nil {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	sm, em := s.Hour()*60+s.Minute(), e.Hour()*60+e.Minute()
	if sm == em {
		return true
	}
	if sm < em {
		return m >= sm && m < em
	}
	return m >= sm || m < em // window over midnight
}

// shouldRemind: backup due, phone reachable, inside the reminder window, at most once per ~day.
func shouldRemind(t time.Time, d *Device, online, busy bool) bool {
	if !online || busy {
		return false
	}
	if d.LastOK > 0 && t.Sub(time.Unix(d.LastOK, 0)) < time.Duration(d.IntervalHours)*time.Hour {
		return false
	}
	if !inWindow(t, d.WindowStart, d.WindowEnd) {
		return false
	}
	return d.LastReminded == 0 || t.Sub(time.Unix(d.LastReminded, 0)) >= 20*time.Hour
}

func shouldAlertStale(t time.Time, d *Device, staleDays int) bool {
	since := d.LastOK
	if since == 0 {
		since = d.CreatedAt
	}
	if t.Sub(time.Unix(since, 0)) < time.Duration(staleDays)*24*time.Hour {
		return false
	}
	return d.StaleAlerted == 0 || t.Sub(time.Unix(d.StaleAlerted, 0)) >= 24*time.Hour
}

func (a *App) schedule() {
	devices, err := a.store.Devices()
	if err != nil {
		log.Printf("schedule: %v", err)
		return
	}
	t := time.Now()
	staleDays := a.settingInt("stale_days", 7)
	for _, d := range devices {
		if shouldRemind(t, d, a.watcher.Presence(d.UDID).Online(), a.runner.Busy(d.UDID)) {
			a.store.SetDeviceTime(d.UDID, "last_reminded", t.Unix())
			a.notifyWithAction(Event{Kind: EvReminder, Device: d})
		}
		if shouldAlertStale(t, d, staleDays) {
			a.store.SetDeviceTime(d.UDID, "stale_alerted", t.Unix())
			a.notifyWithAction(Event{Kind: EvStale, Device: d, Detail: fmt.Sprint(staleDays)})
		}
	}
	a.store.Cleanup()
}

// notifyWithAction attaches a fresh one-time "start backup" link to the event.
func (a *App) notifyWithAction(ev Event) {
	tok, err := a.store.NewActionToken(ev.Device.UDID, 12*time.Hour)
	if err != nil {
		log.Printf("token: %v", err)
	}
	ev.Token = tok
	a.notify(ev)
}

func (a *App) jobFinished(job *Job) {
	a.tgFinished(job) // before notify, so Telegram is not messaged twice
	d, err := a.store.Device(job.UDID)
	if err != nil {
		return
	}
	switch job.Status {
	case JobOK:
		a.notify(Event{Kind: EvOK, Device: d, Job: job})
	case JobNoPasscode:
		a.notifyWithAction(Event{Kind: EvNoPasscode, Device: d, Job: job})
	case JobFailed:
		a.notifyWithAction(Event{Kind: EvFailed, Device: d, Job: job})
	}
}
