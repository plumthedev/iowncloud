package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// Languages offered in the UI; adding one means dropping locales/<code>.toml in place.
var languages = []struct{ Code, Name string }{{"en", "English"}, {"pl", "Polski"}}

func loadBundle(fsys fs.FS) (*i18n.Bundle, error) {
	b := i18n.NewBundle(language.English)
	b.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	files, _ := fs.Glob(fsys, "locales/*.toml")
	for _, f := range files {
		if _, err := b.LoadMessageFileFS(fsys, f); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return b, nil
}

func (a *App) localizer(langs ...string) *i18n.Localizer {
	return i18n.NewLocalizer(a.bundle, langs...)
}

func tr(l *i18n.Localizer, id string, data map[string]any) string {
	s, err := l.Localize(&i18n.LocalizeConfig{MessageID: id, TemplateData: data})
	if err != nil && s == "" {
		return id
	}
	return s
}

func trn(l *i18n.Localizer, id string, n int, data map[string]any) string {
	if data == nil {
		data = map[string]any{}
	}
	data["Count"] = n
	s, err := l.Localize(&i18n.LocalizeConfig{MessageID: id, TemplateData: data, PluralCount: n})
	if err != nil && s == "" {
		return id
	}
	return s
}

func ago(l *i18n.Localizer, unix int64) string {
	if unix == 0 {
		return tr(l, "time.never", nil)
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return tr(l, "time.just_now", nil)
	case d < time.Hour:
		return trn(l, "time.minutes_ago", int(d.Minutes()), nil)
	case d < 24*time.Hour:
		return trn(l, "time.hours_ago", int(d.Hours()), nil)
	default:
		return trn(l, "time.days_ago", int(d.Hours()/24), nil)
	}
}

func duration(_ *i18n.Localizer, secs int64) string {
	switch {
	case secs < 0:
		return ""
	case secs < 60:
		return fmt.Sprintf("%d s", secs)
	case secs < 3600:
		return fmt.Sprintf("%d min", (secs+30)/60)
	default:
		return fmt.Sprintf("%d h %d min", secs/3600, secs%3600/60)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func phaseText(l *i18n.Localizer, phase string) string {
	return tr(l, "phase."+phase, nil)
}

// ponytail: hand-kept table of recent iPhones; unknown identifiers fall back to the raw value.
var modelNames = map[string]string{
	"iPhone12,1": "iPhone 11", "iPhone12,3": "iPhone 11 Pro", "iPhone12,5": "iPhone 11 Pro Max", "iPhone12,8": "iPhone SE (2nd gen)",
	"iPhone13,1": "iPhone 12 mini", "iPhone13,2": "iPhone 12", "iPhone13,3": "iPhone 12 Pro", "iPhone13,4": "iPhone 12 Pro Max",
	"iPhone14,4": "iPhone 13 mini", "iPhone14,5": "iPhone 13", "iPhone14,2": "iPhone 13 Pro", "iPhone14,3": "iPhone 13 Pro Max",
	"iPhone14,6": "iPhone SE (3rd gen)", "iPhone14,7": "iPhone 14", "iPhone14,8": "iPhone 14 Plus",
	"iPhone15,2": "iPhone 14 Pro", "iPhone15,3": "iPhone 14 Pro Max", "iPhone15,4": "iPhone 15", "iPhone15,5": "iPhone 15 Plus",
	"iPhone16,1": "iPhone 15 Pro", "iPhone16,2": "iPhone 15 Pro Max", "iPhone17,3": "iPhone 16", "iPhone17,4": "iPhone 16 Plus",
	"iPhone17,1": "iPhone 16 Pro", "iPhone17,2": "iPhone 16 Pro Max", "iPhone17,5": "iPhone 16e",
}

func modelName(productType string) string {
	if n, ok := modelNames[productType]; ok {
		return n
	}
	return productType
}

func deviceLabel(d *Device) string {
	switch {
	case d.Name != "":
		return d.Name
	case d.ProductType != "":
		return modelName(d.ProductType)
	default:
		return shortUDID(d.UDID)
	}
}

func shortUDID(udid string) string {
	if len(udid) > 8 {
		return "…" + strings.ToUpper(udid[len(udid)-6:])
	}
	return udid
}
