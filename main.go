// iowncloud backs up iPhones to your own server over Wi-Fi.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/crypto/bcrypt"
)

//go:embed migrations locales web
var assets embed.FS

var version = "dev"

type App struct {
	store       *Store
	bundle      *i18n.Bundle
	muxer       *Muxer
	watcher     *Watcher
	runner      *Runner
	broker      *Broker
	tg          *Telegram
	pages       map[string]*pageTemplate
	dataDir     string
	backupDir   string
	lockdownDir string
	authOff     bool
	muxerOff    bool
	reflink     bool
	ctx         context.Context
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &App{
		dataDir:   env("IOWNCLOUD_DATA", "/data"),
		backupDir: env("IOWNCLOUD_BACKUPS", "/backups"),
		authOff:   env("IOWNCLOUD_AUTH", "on") == "off",
		muxerOff:  env("IOWNCLOUD_MUXER", "on") == "off",
		muxer:     &Muxer{},
		watcher:   &Watcher{},
		broker:    newBroker(),
		tg:        newTelegram(),
		ctx:       ctx,
	}
	a.lockdownDir = filepath.Join(a.dataDir, "lockdown")
	for _, d := range []string{a.dataDir, a.lockdownDir, a.backupDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Fatalf("create %s: %v", d, err)
		}
	}

	var err error
	if a.store, err = openStore(filepath.Join(a.dataDir, "iowncloud.db"), assets); err != nil {
		log.Fatalf("database: %v", err)
	}
	if a.bundle, err = loadBundle(assets); err != nil {
		log.Fatalf("locales: %v", err)
	}
	web, _ := fs.Sub(assets, "web")
	if a.pages, err = parsePages(web); err != nil {
		log.Fatalf("templates: %v", err)
	}
	if pw := os.Getenv("IOWNCLOUD_PASSWORD"); pw != "" && a.store.Setting("password_hash", "") == "" {
		hash, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		a.store.SetSetting("password_hash", string(hash))
	}
	a.store.FailStaleJobs()
	a.reflink = checkReflink(a.backupDir)
	if !a.reflink {
		log.Printf("WARNING: %s does not support reflinks; keeping a single copy per device (use Btrfs or XFS for versions)", a.backupDir)
	}

	a.runner = newRunner(a)
	if !a.muxerOff {
		go a.muxer.Run(ctx, a, a.lockdownDir)
	}
	go a.watcher.Run(ctx, a)
	go a.runner.Run(ctx)
	a.syncTelegram(ctx)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.schedule()
			}
		}
	}()

	srv := &http.Server{Addr: env("IOWNCLOUD_LISTEN", ":8080"), Handler: a.routes(web), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("iowncloud %s listening on %s", version, srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (a *App) settingInt(key string, def int) int {
	if n, err := strconv.Atoi(a.store.Setting(key, "")); err == nil {
		return n
	}
	return def
}

// baseURL is the address phones use to reach this server, for links in notifications.
func (a *App) baseURL() string {
	return strings.TrimRight(a.store.Setting("base_url", "http://localhost:8080"), "/")
}

func (a *App) startFromToken(token, trigger string) (string, int64, error) {
	udid, err := a.store.ConsumeActionToken(token)
	if err != nil {
		return "", 0, err
	}
	id, err := a.runner.Enqueue(udid, trigger)
	return udid, id, err
}
