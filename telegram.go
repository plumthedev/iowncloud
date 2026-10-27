package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Telegram sends reminders with an inline "Back up now" button and edits the message
// as the backup progresses. Button taps arrive via long polling, so the NAS needs no
// public address.

type tgRef struct {
	bot  string
	chat int64
	msg  int64
}

type Telegram struct {
	mu       sync.Mutex
	pollers  map[string]context.CancelFunc // by bot token
	pending  map[string][]tgRef            // by action token: messages carrying the button
	jobs     map[int64][]tgRef             // by job: messages to keep updated
	lastEdit map[int64]time.Time
	handled  map[int64]bool // ponytail: grows by one bool per Telegram-started job; trim if it ever matters
}

func newTelegram() *Telegram {
	return &Telegram{pollers: map[string]context.CancelFunc{}, pending: map[string][]tgRef{}, jobs: map[int64][]tgRef{},
		lastEdit: map[int64]time.Time{}, handled: map[int64]bool{}}
}

func parseTelegramURL(raw string) (bot string, chats []string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return "", nil, errors.New("invalid telegram URL")
	}
	pass, _ := u.User.Password()
	bot = u.User.Username() + ":" + pass
	for _, key := range []string{"chats", "channels"} {
		for _, c := range strings.Split(u.Query().Get(key), ",") {
			if c = strings.TrimSpace(c); c != "" {
				chats = append(chats, c)
			}
		}
	}
	return bot, chats, nil
}

var tgClient = &http.Client{Timeout: 70 * time.Second}

func tgCall(ctx context.Context, bot, method string, payload any, out any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+bot+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tgClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func tgKeyboard(label, token string) any {
	if token == "" {
		return map[string]any{"inline_keyboard": [][]any{}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "▶️ " + label, "callback_data": "b:" + token}}}}
}

func (a *App) tgSend(channelURL string, m message, ev Event) error {
	if ev.Job != nil && a.tg.handledJob(ev.Job.ID) {
		return nil // the original message was edited with the outcome instead
	}
	bot, chats, err := parseTelegramURL(channelURL)
	if err != nil {
		return err
	}
	var errs []error
	for _, chat := range chats {
		var sent struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		}
		payload := map[string]any{"chat_id": chat, "text": m.Title + "\n" + m.Body}
		if ev.Token != "" {
			payload["reply_markup"] = tgKeyboard(m.Button, ev.Token)
		}
		err := tgCall(context.Background(), bot, "sendMessage", payload, &sent)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev.Token != "" {
			a.tg.mu.Lock()
			a.tg.pending[ev.Token] = append(a.tg.pending[ev.Token], tgRef{bot, sent.Chat.ID, sent.MessageID})
			a.tg.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

// syncTelegram starts a poller per configured bot and stops pollers of removed bots.
func (a *App) syncTelegram(ctx context.Context) {
	chans, _ := a.store.Channels()
	want := map[string]bool{}
	for _, c := range chans {
		if c.Enabled && strings.HasPrefix(c.URL, "telegram://") {
			if bot, _, err := parseTelegramURL(c.URL); err == nil {
				want[bot] = true
			}
		}
	}
	a.tg.mu.Lock()
	defer a.tg.mu.Unlock()
	for bot, cancel := range a.tg.pollers {
		if !want[bot] {
			cancel()
			delete(a.tg.pollers, bot)
		}
	}
	for bot := range want {
		if a.tg.pollers[bot] == nil {
			pctx, cancel := context.WithCancel(ctx)
			a.tg.pollers[bot] = cancel
			go a.tgPoll(pctx, bot)
		}
	}
}

func (a *App) tgPoll(ctx context.Context, bot string) {
	var offset int64
	for ctx.Err() == nil {
		var updates []struct {
			UpdateID      int64 `json:"update_id"`
			CallbackQuery *struct {
				ID      string `json:"id"`
				Data    string `json:"data"`
				Message *struct {
					MessageID int64 `json:"message_id"`
					Chat      struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
			} `json:"callback_query"`
		}
		err := tgCall(ctx, bot, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"callback_query"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("telegram poll: %v", err)
				time.Sleep(15 * time.Second)
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if cq := u.CallbackQuery; cq != nil && strings.HasPrefix(cq.Data, "b:") {
				a.tgButton(ctx, bot, cq.ID, strings.TrimPrefix(cq.Data, "b:"), cq.Message)
			}
		}
	}
}

func (a *App) tgButton(ctx context.Context, bot, callbackID, token string, msg *struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}) {
	l := a.localizer(a.store.Setting("language", "en"))
	_, jobID, err := a.startFromToken(token, "telegram")
	answer := tr(l, "tg.started", nil)
	if err != nil {
		answer = tr(l, "tg.expired", nil)
	}
	tgCall(ctx, bot, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": answer}, nil)
	if err != nil {
		return
	}
	a.tg.mu.Lock()
	refs := a.tg.pending[token]
	delete(a.tg.pending, token)
	if msg != nil && !containsRef(refs, msg.Chat.ID, msg.MessageID) {
		refs = append(refs, tgRef{bot, msg.Chat.ID, msg.MessageID})
	}
	a.tg.jobs[jobID] = append(a.tg.jobs[jobID], refs...)
	a.tg.mu.Unlock()
	if live := a.runner.Live(a.jobUDID(jobID)); live != nil {
		a.tgProgress(*live)
	}
}

func containsRef(refs []tgRef, chat, msg int64) bool {
	for _, r := range refs {
		if r.chat == chat && r.msg == msg {
			return true
		}
	}
	return false
}

func (a *App) jobUDID(id int64) string {
	if j, err := a.store.Job(id); err == nil {
		return j.UDID
	}
	return ""
}

// tgProgress edits the job's messages; throttled because Telegram limits edit rates.
func (a *App) tgProgress(live Live) {
	a.tg.mu.Lock()
	refs := a.tg.jobs[live.JobID]
	if len(refs) == 0 || (live.Phase == PhaseTransfer && time.Since(a.tg.lastEdit[live.JobID]) < 5*time.Second) {
		a.tg.mu.Unlock()
		return
	}
	a.tg.lastEdit[live.JobID] = time.Now()
	a.tg.mu.Unlock()

	l := a.localizer(a.store.Setting("language", "en"))
	name := live.UDID
	if d, err := a.store.Device(live.UDID); err == nil {
		name = deviceLabel(d)
	}
	text := name + "\n" + phaseText(l, live.Phase)
	if live.Phase == PhaseTransfer {
		text += "\n" + progressBar(live.Progress) + fmt.Sprintf(" %d%%", live.Progress)
	}
	a.tgEdit(refs, text, "", "")
}

func (a *App) tgFinished(job *Job) {
	a.tg.mu.Lock()
	refs := a.tg.jobs[job.ID]
	delete(a.tg.jobs, job.ID)
	delete(a.tg.lastEdit, job.ID)
	a.tg.mu.Unlock()
	if len(refs) == 0 {
		return
	}
	d, err := a.store.Device(job.UDID)
	if err != nil {
		return
	}
	ev := Event{Kind: job.Status, Device: d, Job: job}
	if job.Status != JobOK {
		ev.Token, _ = a.store.NewActionToken(job.UDID, 12*time.Hour)
	}
	m := a.renderEvent(ev)
	a.tgEdit(refs, m.Title+"\n"+m.Body, m.Button, ev.Token)
	if ev.Token != "" {
		a.tg.mu.Lock()
		a.tg.pending[ev.Token] = refs
		a.tg.mu.Unlock()
	}
	a.tg.markHandled(job.ID)
}

func (a *App) tgEdit(refs []tgRef, text, button, token string) {
	for _, r := range refs {
		err := tgCall(context.Background(), r.bot, "editMessageText", map[string]any{
			"chat_id": r.chat, "message_id": r.msg, "text": text, "reply_markup": tgKeyboard(button, token),
		}, nil)
		if err != nil && !strings.Contains(err.Error(), "not modified") {
			log.Printf("telegram edit: %v", err)
		}
	}
}

// handled remembers jobs whose outcome was already shown by editing the Telegram message,
// so the regular outcome notification skips Telegram.
func (t *Telegram) markHandled(jobID int64) {
	t.mu.Lock()
	t.handled[jobID] = true
	t.mu.Unlock()
}

func (t *Telegram) handledJob(jobID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.handled[jobID]
}

func progressBar(p int) string {
	n := max(0, min(10, p/10))
	return strings.Repeat("▓", n) + strings.Repeat("░", 10-n)
}
