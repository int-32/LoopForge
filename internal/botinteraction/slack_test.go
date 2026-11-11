package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseSlackSecretAndText(t *testing.T) {
	bot, app := ParseSlackSecret(" xapp-1-A, xoxb-2-B\n")
	if bot != "xoxb-2-B" || app != "xapp-1-A" || !IsSlackSecret("xoxb-1 xapp-2") || IsSlackSecret("wecom-secret") {
		t.Fatalf("parse = %q %q", bot, app)
	}
	got := slackText("### 标题\n**粗体** a<b & [链接](https://x.test/a?b=1)")
	want := "*标题*\n*粗体* a&lt;b &amp; <https://x.test/a?b=1|链接>"
	if got != want {
		t.Fatalf("slackText = %q", got)
	}
	if ch, th := slackChat("C1:171.5"); ch != "C1" || th != "171.5" {
		t.Fatal("slackChat")
	}
}

func TestNormalizeSlack(t *testing.T) {
	bot := domain.NotificationBot{ID: "b", BotID: "A1"}
	env := func(typ string, payload any) slackEnvelope {
		raw, _ := json.Marshal(payload)
		return slackEnvelope{EnvelopeID: "e", Type: typ, Payload: raw}
	}
	event := func(ev map[string]any) slackEnvelope { return env("events_api", map[string]any{"event": ev}) }

	item, kind, chat, ok := normalizeSlack(bot, "UBOT", event(map[string]any{"type": "message", "channel_type": "im", "channel": "D1", "user": "U1", "ts": "1.1", "text": "你好 &amp; 再见"}))
	if !ok || kind != "single" || chat != "D1" || item.SenderID != "U1" || item.Content != "你好 & 再见" {
		t.Fatalf("im = %+v %s %s %v", item, kind, chat, ok)
	}
	item, kind, chat, ok = normalizeSlack(bot, "UBOT", event(map[string]any{"type": "app_mention", "channel": "C1", "user": "U1", "ts": "2.2", "thread_ts": "1.0", "text": "<@UBOT> 看下进度"}))
	if !ok || kind != "group" || chat != "C1:1.0" || item.Content != "看下进度" {
		t.Fatalf("mention = %+v %s %s %v", item, kind, chat, ok)
	}
	if _, _, chat, _ = normalizeSlack(bot, "UBOT", event(map[string]any{"type": "app_mention", "channel": "C1", "user": "U1", "ts": "3.3", "text": "<@UBOT> hi"})); chat != "C1:3.3" {
		t.Fatalf("top-level mention starts a thread: %s", chat)
	}
	for _, ev := range []map[string]any{
		{"type": "message", "channel_type": "im", "channel": "D1", "user": "U1", "ts": "1", "text": "x", "bot_id": "B1"},
		{"type": "message", "channel_type": "im", "channel": "D1", "user": "U1", "ts": "1", "text": "x", "subtype": "message_changed"},
		{"type": "message", "channel_type": "im", "channel": "D1", "user": "UBOT", "ts": "1", "text": "x"},
		{"type": "message", "channel_type": "channel", "channel": "C1", "user": "U1", "ts": "1", "text": "x"},
		{"type": "app_mention", "channel": "C1", "user": "U1", "ts": "1", "text": "<@UBOT>"},
	} {
		if _, _, _, ok := normalizeSlack(bot, "UBOT", event(ev)); ok {
			t.Fatalf("accepted %v", ev)
		}
	}
	action := env("interactive", map[string]any{"type": "block_actions", "user": map[string]string{"id": "U1"}, "channel": map[string]string{"id": "C9"}, "message": map[string]string{"ts": "5.5"},
		"actions": []map[string]string{{"action_id": "approve", "value": "nd_1_abc", "action_ts": "6.6"}}})
	item, kind, chat, ok = normalizeSlack(bot, "UBOT", action)
	if !ok || kind != "group" || chat != "C9" || item.Action != "approve" || item.DeliveryID != "nd_1" || item.CardTaskID != "nd_1_abc" || item.RequestID != "C9|5.5" {
		t.Fatalf("action = %+v %s %s %v", item, kind, chat, ok)
	}
}

type fakeSlack struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	conn    *websocket.Conn
	writeMu sync.Mutex
	acks    map[string]int
	calls   chan map[string]string
	ready   chan struct{}
	ts      int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{t: t, acks: map[string]int{}, calls: make(chan map[string]string, 50), ready: make(chan struct{}, 4)}
	var upgrader websocket.Upgrader
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conn = ws
			f.mu.Unlock()
			f.write(map[string]any{"type": "hello"})
			f.ready <- struct{}{}
			for {
				var ack map[string]string
				if ws.ReadJSON(&ack) != nil {
					return
				}
				f.mu.Lock()
				f.acks[ack["envelope_id"]]++
				f.mu.Unlock()
			}
		}
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "apps.connections.open":
			if token != "xapp-test" {
				t.Errorf("connections.open token %q", token)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"})
			return
		case "auth.test":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "UBOT"})
			return
		}
		if token != "xoxb-test" {
			t.Errorf("%s token %q", method, token)
		}
		call := map[string]string{"method": method}
		for k := range r.PostForm {
			call[k] = r.PostForm.Get(k)
		}
		f.mu.Lock()
		f.ts++
		ts := "100." + string(rune('0'+f.ts%10))
		f.mu.Unlock()
		f.calls <- call
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
	}))
	return f
}

func (f *fakeSlack) write(v any) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if err := conn.WriteJSON(v); err != nil {
		f.t.Error(err)
	}
}

func (f *fakeSlack) envelope(id, typ string, payload any) {
	f.write(map[string]any{"envelope_id": id, "type": typ, "payload": payload})
}

// next returns the next Web API call matching method, skipping others.
func (f *fakeSlack) next(method string) map[string]string {
	f.t.Helper()
	deadline := time.After(6 * time.Second)
	for {
		select {
		case c := <-f.calls:
			if c["method"] == method {
				return c
			}
		case <-deadline:
			f.t.Fatalf("no %s call", method)
		}
	}
}

func TestSlackChatAndCardFlow(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bot := domain.NotificationBot{ID: "bot-slack", BotID: "A1", Platform: PlatformSlack, ProjectID: "p", OwnerID: 1, Enabled: true, InteractionEnabled: true}
	if err := store.SaveNotificationBot(bot, "xoxb-test xapp-test"); err != nil {
		t.Fatal(err)
	}
	fake := newFakeSlack(t)
	defer fake.srv.Close()

	service := New(store)
	service.SlackAPI = fake.srv.URL
	var genMu sync.Mutex
	chats := []string{}
	service.Generate = func(_ context.Context, _ domain.NotificationBot, c domain.BotConversation, _ []domain.BotInteraction, item domain.BotInteraction) (string, error) {
		genMu.Lock()
		chats = append(chats, c.ChatID)
		genMu.Unlock()
		return "**答复**：" + item.Content, nil
	}
	actions := make(chan domain.BotConversation, 2)
	service.Action = func(_ context.Context, _ domain.NotificationBot, item domain.BotInteraction, c domain.BotConversation) (string, error) {
		if item.Action != "approve" || item.SenderID != "U1" {
			t.Errorf("action %+v", item)
		}
		actions <- c
		return "「Review」操作已完成：确认通过，继续流程。", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.Start(ctx)
	defer func() { cancel(); <-service.Done }()
	select {
	case <-fake.ready:
	case <-time.After(6 * time.Second):
		t.Fatal("socket mode not connected")
	}

	// Direct message: receipt, then the receipt is edited into the reply.
	dm := map[string]any{"event": map[string]any{"type": "message", "channel_type": "im", "channel": "D1", "user": "U1", "ts": "1.1", "text": "你好"}}
	fake.envelope("e1", "events_api", dm)
	fake.envelope("e1-retry", "events_api", dm) // Slack retry: same message must not run twice.
	receipt := fake.next("chat.postMessage")
	if receipt["channel"] != "D1" || receipt["text"] != slackReceipt {
		t.Fatalf("receipt = %v", receipt)
	}
	final := fake.next("chat.update")
	if final["channel"] != "D1" || final["text"] != "*答复*：你好" {
		t.Fatalf("final = %v", final)
	}

	// Channel mention: answered in the message's thread, own conversation.
	fake.envelope("e2", "events_api", map[string]any{"event": map[string]any{"type": "app_mention", "channel": "C1", "user": "U2", "ts": "2.2", "text": "<@UBOT> 进度"}})
	receipt = fake.next("chat.postMessage")
	if receipt["channel"] != "C1" || receipt["thread_ts"] != "2.2" {
		t.Fatalf("thread receipt = %v", receipt)
	}
	fake.next("chat.update")
	genMu.Lock()
	if len(chats) != 2 || chats[0] != "D1" || chats[1] != "C1:2.2" {
		t.Fatalf("chats = %v", chats)
	}
	genMu.Unlock()

	// Approval card for a waiting node, then a button click.
	run := domain.Run{ID: "run-1", Status: "waiting",
		WorkflowSnapshot: domain.WorkflowDefinition{Name: "WF", Nodes: []domain.WorkflowNode{{ID: "review", Name: "Review"}}},
		Nodes:            map[string]*domain.NodeRun{"review": {NodeID: "review", Status: "waiting_approval", Attempt: 1}}}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	delivery := domain.NotificationDelivery{ID: "nd-1", BotID: bot.ID, ProjectID: "p", RunID: run.ID, NodeID: "review", Attempt: 1, EventType: "waiting",
		SourceName: "Review", TargetType: "group", TargetID: "C9", Status: "sending", CreatedAt: time.Now()}
	if err := store.DB.Create(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() { queued <- service.QueueNotificationCard(ctx, bot, delivery, "需求：Issue\n等待确认") }()
	card := fake.next("chat.postMessage")
	if card["channel"] != "C9" || !strings.Contains(card["blocks"], `"action_id":"approve"`) {
		t.Fatalf("card = %v", card)
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(card["blocks"]), &blocks)
	value := blocks[1]["elements"].([]any)[0].(map[string]any)["value"].(string)
	if !strings.HasPrefix(value, "nd-1_") {
		t.Fatalf("button value = %q", value)
	}
	if err := <-queued; err != nil {
		t.Fatalf("queue card = %v", err)
	}
	click := map[string]any{"type": "block_actions", "user": map[string]string{"id": "U1"}, "channel": map[string]string{"id": "C9"}, "message": map[string]string{"ts": "9.9"},
		"actions": []map[string]string{{"action_id": "approve", "value": value, "action_ts": "10.1"}}}
	fake.envelope("e3", "interactive", click)
	fake.envelope("e3-dup", "interactive", click)
	select {
	case c := <-actions:
		if c.ChatID != "C9" {
			t.Fatalf("action chat = %s", c.ChatID)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("action not processed")
	}
	done := fake.next("chat.update")
	if done["channel"] != "C9" || done["ts"] != "9.9" || !strings.Contains(done["blocks"], "操作已完成") {
		t.Fatalf("card update = %v", done)
	}
	select {
	case <-actions:
		t.Fatal("duplicate click executed")
	case <-time.After(500 * time.Millisecond):
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, id := range []string{"e1", "e1-retry", "e2", "e3", "e3-dup"} {
		if fake.acks[id] != 1 {
			t.Fatalf("ack %s = %d", id, fake.acks[id])
		}
	}
}
