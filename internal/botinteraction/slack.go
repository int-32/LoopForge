package botinteraction

// Slack adapter. It reuses the platform's bot queue, conversations, account
// binding and card actions, and replaces only the transport: Socket Mode for
// inbound events (no public callback endpoint) and the Web API for replies.

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PlatformSlack marks a NotificationBot whose credentials are Slack tokens.
const PlatformSlack = "slack"

const slackReceipt = "已收到，正在处理…"

// IsSlackSecret reports whether a submitted bot secret holds Slack tokens.
func IsSlackSecret(secret string) bool {
	bot, _ := ParseSlackSecret(secret)
	return bot != ""
}

// ParseSlackSecret splits the stored secret "xoxb-… xapp-…" (any order,
// separated by spaces, commas or new lines) into the bot and app tokens.
func ParseSlackSecret(secret string) (botToken, appToken string) {
	for _, field := range strings.FieldsFunc(secret, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' || r == '\r' || r == '\t' }) {
		switch {
		case strings.HasPrefix(field, "xoxb-"):
			botToken = field
		case strings.HasPrefix(field, "xapp-"):
			appToken = field
		}
	}
	return botToken, appToken
}

func defaultSlackAPI() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("AGENTFLOW_SLACK_API_URL")), "/"); v != "" {
		return v
	}
	return "https://slack.com/api"
}

// SlackError is a Slack Web API business error (ok=false). The request
// reached Slack; retrying the same reply over a new connection will not help.
type SlackError struct{ Code string }

func (e *SlackError) Error() string { return "Slack 拒绝了这次请求：" + e.Code }

type slackClient struct {
	api      string
	botToken string
	appToken string
	http     *http.Client
}

func newSlackClient(api, secret string) (*slackClient, error) {
	botToken, appToken := ParseSlackSecret(secret)
	if botToken == "" {
		return nil, errors.New("Slack 凭据缺少 Bot Token（xoxb-）")
	}
	if api == "" {
		api = defaultSlackAPI()
	}
	return &slackClient{api: api, botToken: botToken, appToken: appToken, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (c *slackClient) call(ctx context.Context, method, token string, params map[string]any, out any) error {
	form := url.Values{}
	for k, v := range params {
		switch val := v.(type) {
		case string:
			if val != "" {
				form.Set(k, val)
			}
		case bool:
			form.Set(k, fmt.Sprint(val))
		case int:
			form.Set(k, fmt.Sprint(val))
		default:
			raw, _ := json.Marshal(val)
			form.Set(k, string(raw))
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	rsp, err := c.http.Do(req)
	if err != nil {
		return errors.New("Slack 网络请求失败")
	}
	defer rsp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(rsp.Body, 4<<20))
	if err != nil {
		return errors.New("Slack 响应读取失败")
	}
	if rsp.StatusCode == http.StatusTooManyRequests {
		return errors.New("Slack 限流，请稍后重试")
	}
	var base struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &base) != nil {
		return fmt.Errorf("Slack 响应无效（HTTP %d）", rsp.StatusCode)
	}
	if !base.OK {
		return &SlackError{Code: base.Error}
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// post sends a message and returns its ts. thread may be empty.
func (c *slackClient) post(ctx context.Context, channel, thread, text string, blocks []map[string]any) (string, error) {
	params := map[string]any{"channel": channel, "thread_ts": thread, "text": slackText(text), "unfurl_links": false}
	if blocks != nil {
		params["blocks"] = blocks
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", c.botToken, params, &out)
	return out.TS, err
}

func (c *slackClient) update(ctx context.Context, channel, ts, text string, blocks []map[string]any) error {
	params := map[string]any{"channel": channel, "ts": ts, "text": slackText(text)}
	if blocks != nil {
		params["blocks"] = blocks
	} else {
		params["blocks"] = []map[string]any{}
	}
	return c.call(ctx, "chat.update", c.botToken, params, nil)
}

func (c *slackClient) botUserID(ctx context.Context) (string, error) {
	var out struct {
		UserID string `json:"user_id"`
	}
	err := c.call(ctx, "auth.test", c.botToken, nil, &out)
	return out.UserID, err
}

// SlackSessions lists the channels and direct messages the bot can post to.
func SlackSessions(ctx context.Context, api, secret string) ([]domain.BotChat, error) {
	c, err := newSlackClient(api, secret)
	if err != nil {
		return nil, err
	}
	chats := []domain.BotChat{}
	cursor := ""
	names := map[string]string{}
	for page := 0; page < 10; page++ {
		var out struct {
			Channels []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				IsIM      bool   `json:"is_im"`
				User      string `json:"user"`
				IsPrivate bool   `json:"is_private"`
			} `json:"channels"`
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "users.conversations", c.botToken, map[string]any{"types": "public_channel,private_channel,im", "exclude_archived": true, "limit": 200, "cursor": cursor}, &out); err != nil {
			return nil, err
		}
		for _, ch := range out.Channels {
			if ch.IsIM {
				name, ok := names[ch.User]
				if !ok {
					name = slackUserName(ctx, c, ch.User)
					names[ch.User] = name
				}
				chats = append(chats, domain.BotChat{ID: ch.ID, Name: "私信 · " + name, Type: "single"})
				continue
			}
			chats = append(chats, domain.BotChat{ID: ch.ID, Name: "#" + ch.Name, Type: "group"})
		}
		cursor = out.Meta.Next
		if cursor == "" {
			break
		}
	}
	return chats, nil
}

func slackUserName(ctx context.Context, c *slackClient, user string) string {
	var out struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				RealName    string `json:"real_name"`
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if user == "" || c.call(ctx, "users.info", c.botToken, map[string]any{"user": user}, &out) != nil {
		return user
	}
	for _, name := range []string{out.User.Profile.DisplayName, out.User.Profile.RealName, out.User.Name} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return user
}

// SlackSend posts a plain notification to a channel or direct message.
func SlackSend(ctx context.Context, api, secret, channel, text string) error {
	c, err := newSlackClient(api, secret)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, channel, "", text, nil)
	return err
}

// Chat IDs: a direct message is its D… channel; a channel thread is
// "C…:thread_ts" so every thread keeps its own conversation context. Cards
// and notifications target the bare channel ID chosen from SlackSessions.
func slackChat(chat string) (channel, thread string) {
	channel, thread, _ = strings.Cut(chat, ":")
	return channel, thread
}

var (
	slackLinkRe    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	slackBoldRe    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	slackHeadingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	slackMentionRe = regexp.MustCompile(`<@[A-Z0-9]+>`)
)

// slackText converts the platform's Markdown replies to Slack mrkdwn.
func slackText(md string) string {
	text := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(md)
	text = slackLinkRe.ReplaceAllString(text, "<$2|$1>")
	text = slackBoldRe.ReplaceAllString(text, "*$1*")
	text = slackHeadingRe.ReplaceAllString(text, "*$1*")
	return text
}

type slackEnvelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Reason     string          `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
}

type slackEvent struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
}

type slackAction struct {
	Type    string              `json:"type"`
	User    struct{ ID string } `json:"user"`
	Channel struct{ ID string } `json:"channel"`
	Message struct {
		TS string `json:"ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

// normalizeSlack turns one Socket Mode envelope into a queued interaction.
func normalizeSlack(bot domain.NotificationBot, botUser string, env slackEnvelope) (domain.BotInteraction, string, string, bool) {
	switch env.Type {
	case "events_api":
		var payload struct {
			Event slackEvent `json:"event"`
		}
		if json.Unmarshal(env.Payload, &payload) != nil {
			return domain.BotInteraction{}, "", "", false
		}
		ev := payload.Event
		if ev.BotID != "" || ev.Subtype != "" || ev.User == "" || ev.User == botUser || ev.Channel == "" || ev.TS == "" {
			return domain.BotInteraction{}, "", "", false
		}
		kind, chat := "", ""
		switch {
		case ev.Type == "message" && ev.ChannelType == "im":
			kind, chat = "single", ev.Channel
		case ev.Type == "app_mention" && !strings.HasPrefix(ev.Channel, "D"):
			thread := ev.ThreadTS
			if thread == "" {
				thread = ev.TS
			}
			kind, chat = "group", ev.Channel+":"+thread
		default:
			return domain.BotInteraction{}, "", "", false
		}
		text := html.UnescapeString(strings.TrimSpace(slackMentionRe.ReplaceAllString(ev.Text, "")))
		text = domain.NotificationText(text, 16000)
		if text == "" || len(chat) > 191 || len(ev.User) > 191 {
			return domain.BotInteraction{}, "", "", false
		}
		return domain.BotInteraction{ID: repo.BotMessageID(bot.BotID, ev.Channel+":"+ev.TS), BotID: bot.ID, SenderID: ev.User, Content: text}, kind, chat, true
	case "interactive":
		var p slackAction
		if json.Unmarshal(env.Payload, &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 || p.User.ID == "" || p.Channel.ID == "" || p.Message.TS == "" {
			return domain.BotInteraction{}, "", "", false
		}
		a := p.Actions[0]
		delivery := a.Value
		if i := strings.LastIndex(delivery, "_"); i > 0 {
			delivery = delivery[:i]
		}
		if a.ActionID == "" || a.ActionID == "handled" || delivery == "" {
			return domain.BotInteraction{}, "", "", false
		}
		kind := "group"
		if strings.HasPrefix(p.Channel.ID, "D") {
			kind = "single"
		}
		id := repo.BotMessageID(bot.BotID, "action:"+p.Channel.ID+":"+p.Message.TS+":"+p.User.ID+":"+a.ActionTS)
		return domain.BotInteraction{ID: id, BotID: bot.ID, SenderID: p.User.ID, RequestID: p.Channel.ID + "|" + p.Message.TS, Action: a.ActionID, DeliveryID: delivery, CardTaskID: a.Value}, kind, p.Channel.ID, true
	}
	return domain.BotInteraction{}, "", "", false
}

type slackSocket struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	done    chan struct{}
	cancel  context.CancelFunc
}

func (s *slackSocket) close() { s.cancel(); s.conn.Close() }

func dialSlack(ctx context.Context, c *slackClient, onEnvelope func(slackEnvelope)) (*slackSocket, error) {
	if c.appToken == "" {
		return nil, errors.New("Slack 凭据缺少 App Token（xapp-），请开启 Socket Mode")
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, "apps.connections.open", c.appToken, nil, &out); err != nil {
		return nil, err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, out.URL, nil)
	if err != nil {
		return nil, errors.New("Slack 长连接建立失败")
	}
	socketCtx, cancel := context.WithCancel(ctx)
	s := &slackSocket{conn: conn, done: make(chan struct{}), cancel: cancel}
	conn.SetReadLimit(4 << 20)
	go func() {
		defer close(s.done)
		defer cancel()
		defer conn.Close()
		for {
			var env slackEnvelope
			if err := conn.ReadJSON(&env); err != nil {
				return
			}
			if env.EnvelopeID != "" {
				s.writeMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteJSON(map[string]string{"envelope_id": env.EnvelopeID})
				s.writeMu.Unlock()
				if err != nil {
					return
				}
			}
			if env.Type == "disconnect" {
				return
			}
			onEnvelope(env)
		}
	}()
	go func() { <-socketCtx.Done(); conn.Close() }()
	return s, nil
}

// slackReceipt tracks the "已收到" message that is edited into progress and
// the final reply. ready closes once the post returns; ts stays empty if it failed.
type slackReceiptMsg struct {
	ready       chan struct{}
	channel, ts string
}

func (r *slackReceiptMsg) wait(d time.Duration) bool {
	select {
	case <-r.ready:
	case <-time.After(d):
		return false
	}
	return r.ts != ""
}

func (s *Service) slackAPI() string {
	if s.SlackAPI != "" {
		return s.SlackAPI
	}
	return defaultSlackAPI()
}

func (s *Service) runSlack(ctx context.Context, c *connection) {
	owner := randomID()
	claimed, err := s.Store.ClaimBotConnection(c.bot.BotID, owner, time.Now().UTC())
	if err != nil || !claimed {
		return
	}
	defer s.Store.ReleaseBotConnection(c.bot.BotID, owner)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.Store.RecoverBotInteractions(c.bot.ID); err != nil {
		return
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				ok, err := s.Store.RenewBotConnection(c.bot.BotID, owner, "", "", time.Now().UTC())
				if err != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	backoff := time.Second
	for runCtx.Err() == nil {
		secret, err := s.Store.BotCredential(c.bot.ID)
		if err != nil {
			s.state(c.bot, owner, "error", "机器人凭据不可用")
			return
		}
		client, err := newSlackClient(s.slackAPI(), secret)
		if err != nil {
			s.state(c.bot, owner, "error", err.Error())
			return
		}
		botUser, err := client.botUserID(runCtx)
		var ws *slackSocket
		if err == nil {
			ws, err = dialSlack(runCtx, client, func(env slackEnvelope) {
				s.receiveSlack(runCtx, c.bot, client, botUser, env)
			})
		}
		if err == nil {
			s.state(c.bot, owner, "connected", "")
			err = s.processSlack(runCtx, c.bot, owner, client, ws)
			ws.close()
			backoff = time.Second
		} else {
			log.Printf("slack connect failed bot=%s: %v", c.bot.ID, err)
			var business *SlackError
			if errors.As(err, &business) && (business.Code == "invalid_auth" || business.Code == "not_authed" || business.Code == "account_inactive" || business.Code == "token_revoked") {
				s.state(c.bot, owner, "error", "Slack 凭据无效："+business.Code)
			}
		}
		if runCtx.Err() != nil {
			return
		}
		s.state(c.bot, owner, "reconnecting", "连接中断，正在重连")
		timer := time.NewTimer(backoff)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Service) receiveSlack(ctx context.Context, bot domain.NotificationBot, client *slackClient, botUser string, env slackEnvelope) {
	item, kind, chat, ok := normalizeSlack(bot, botUser, env)
	if !ok {
		return
	}
	inserted, err := s.Store.EnqueueBotInteraction(item, kind, chat)
	if err != nil || !inserted {
		if err != nil {
			log.Printf("slack event enqueue failed action=%s", item.Action)
		}
		return
	}
	if item.Action == "" {
		channel, thread := slackChat(chat)
		receipt := &slackReceiptMsg{ready: make(chan struct{}), channel: channel}
		s.slackPending.Store(item.ID, receipt)
		go func() {
			defer close(receipt.ready)
			postCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if ts, err := client.post(postCtx, channel, thread, slackReceipt, nil); err == nil {
				receipt.ts = ts
			}
		}()
	}
}

// slackDeliver edits the receipt into the final reply, or posts a new message
// when no receipt exists (for example after a restart).
func (s *Service) slackDeliver(ctx context.Context, client *slackClient, item domain.BotInteraction, chat, text string) error {
	channel, thread := slackChat(chat)
	if value, ok := s.slackPending.LoadAndDelete(item.ID); ok {
		if r := value.(*slackReceiptMsg); r.wait(5*time.Second) && client.update(ctx, r.channel, r.ts, text, nil) == nil {
			return nil
		}
	}
	_, err := client.post(ctx, channel, thread, text, nil)
	return err
}

func (s *Service) slackProgress(ctx context.Context, client *slackClient, item domain.BotInteraction) (context.Context, func()) {
	updates := make(chan string, 1)
	publish := func(status string) {
		select {
		case updates <- status:
		default:
		}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(stopped)
		phase := "正在理解你的请求"
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case phase = <-updates:
			case <-ticker.C:
				if value, ok := s.slackPending.Load(item.ID); ok {
					if r := value.(*slackReceiptMsg); r.wait(0) {
						_ = client.update(ctx, r.channel, r.ts, fmt.Sprintf("%s…（已等待 %d 秒）", phase, int(time.Since(started).Seconds())), nil)
					}
				}
			}
		}
	}()
	return context.WithValue(ctx, progressKey{}, publish), func() { close(done); <-stopped }
}

func slackCardBlocks(content string, actions []map[string]any, taskID string) []map[string]any {
	buttons, _ := notificationCardActions(actions)
	elements := []map[string]any{}
	for _, b := range buttons {
		key, _ := b["key"].(string)
		text, _ := b["text"].(string)
		button := map[string]any{"type": "button", "action_id": key, "value": taskID, "text": map[string]any{"type": "plain_text", "text": text}}
		if style, _ := b["style"].(int); style == 1 {
			button["style"] = "primary"
		}
		elements = append(elements, button)
	}
	body := slackText(content)
	if r := []rune(body); len(r) > 2900 {
		body = string(r[:2899]) + "…"
	}
	return []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": body}},
		{"type": "actions", "elements": elements},
	}
}

func slackResultBlocks(result string) []map[string]any {
	title := "操作未完成"
	if strings.Contains(result, "操作已完成") {
		title = "操作已完成"
	} else if strings.Contains(result, "过期") {
		title = "提醒已失效"
	}
	return []map[string]any{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*" + title + "*\n" + slackText(cardText(result, 2800))}}}
}

func (s *Service) processSlack(ctx context.Context, bot domain.NotificationBot, owner string, client *slackClient, ws *slackSocket) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ws.done:
			return errors.New("连接中断")
		case <-ticker.C:
		}
		item, err := s.Store.ClaimBotInteraction(bot.ID, bot.BotID, owner)
		if err != nil {
			return err
		}
		if item == nil {
			continue
		}
		conv, err := s.Store.BotConversation(item.ConversationID)
		if err != nil {
			return err
		}
		if item.Action == "notification" {
			delivery, lookupErr := s.Store.NotificationDelivery(item.DeliveryID)
			actions := []map[string]any{}
			if lookupErr == nil {
				actions = ActionsForDelivery(s.Store, delivery)
			}
			if len(actions) == 0 {
				if err = s.Store.ExpireBotCard(item.ID); err != nil {
					return err
				}
				continue
			}
			if item.Status != "ready" {
				if err = s.Store.SaveBotReply(*item, "", ""); err != nil {
					return err
				}
			}
			taskID := item.DeliveryID + "_" + randomID()[:12]
			if err = s.Store.SetBotCardTaskID(item.ID, taskID); err != nil {
				return err
			}
			channel, _ := slackChat(conv.ChatID)
			if _, err = client.post(ctx, channel, "", item.Content, slackCardBlocks(item.Content, actions, taskID)); err != nil {
				if done, ferr := s.slackFailed(item.ID, err); done {
					if ferr != nil {
						return ferr
					}
					continue
				}
				return err
			}
			if err = s.Store.MarkBotReplySent(item.ID); err != nil {
				return err
			}
			continue
		}
		if item.Action == "notification_details" {
			delivery, lookupErr := s.Store.NotificationDelivery(item.DeliveryID)
			if lookupErr != nil || len(ActionsForDelivery(s.Store, delivery)) == 0 {
				if err = s.Store.ExpireBotCard(item.ID); err != nil {
					return err
				}
				continue
			}
		}
		if item.Status != "ready" {
			reply, detail := "", ""
			if item.Action == "notification_details" {
				reply = item.Content
			} else if time.Since(item.CreatedAt) > 10*time.Minute {
				reply = "这条消息等待处理的时间较长，已停止自动执行。请重新发送当前请求。"
			} else if newConversationCommand(item.Content) {
				err = s.Store.NewBotConversation(*item)
				reply = "已开启新会话。当前聊天将从新的上下文开始，历史记录保留。"
			} else {
				turnCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				var stopProgress func()
				if item.Action == "" {
					turnCtx, stopProgress = s.slackProgress(turnCtx, client, *item)
				}
				if item.Action != "" {
					if s.Action == nil {
						err = errors.New("操作服务未就绪")
					} else {
						reply, err = s.Action(turnCtx, bot, *item, conv)
					}
				} else {
					history, historyErr := s.Store.BotHistory(conv.SessionID)
					err = historyErr
					if err == nil {
						if s.Generate == nil {
							err = errors.New("助手服务未就绪")
						} else {
							reply, err = s.Generate(turnCtx, bot, conv, history, *item)
						}
					}
				}
				if stopProgress != nil {
					stopProgress()
				}
				cancel()
			}
			if errors.Is(err, ErrCardActionDeferred) {
				detail = ErrCardActionDeferred.Error()
			} else if err != nil {
				log.Printf("slack turn failed bot=%s: %v", bot.ID, err)
				detail = "本次处理失败"
				reply = "本次处理未完成，请稍后重试或到平台查看状态。"
				if errors.Is(err, context.DeadlineExceeded) {
					detail = "模型回复提交超时"
					reply = "本次处理已超时，未能提交最终回复。请重新发送请求；若涉及修改操作，请先在平台核对状态，避免重复执行。"
				}
			}
			if strings.TrimSpace(reply) == "" {
				detail = "助手未提交回复"
				reply = "本次没有得到有效回复，请稍后重试。"
			}
			reply = domain.NotificationText(reply, 16000)
			if reply, detail, err = s.saveReply(*item, reply, detail); err != nil {
				return err
			}
			item.Reply = reply
			item.LastError = detail
		}
		switch {
		case item.Action != "" && item.Action != "notification_details":
			channel, ts, _ := strings.Cut(item.RequestID, "|")
			if item.LastError != ErrCardActionDeferred.Error() && ts != "" {
				err = client.update(ctx, channel, ts, item.Reply, slackResultBlocks(item.Reply))
				if err != nil {
					_, err = client.post(ctx, channel, ts, item.Reply, nil)
				}
			} else {
				// Not yet authorised (for example the sender must bind an account):
				// keep the buttons and answer in the card's thread.
				_, err = client.post(ctx, channel, ts, item.Reply, nil)
			}
		case item.Action == "notification_details":
			channel, _ := slackChat(conv.ChatID)
			_, err = client.post(ctx, channel, "", item.Reply, nil)
		default:
			err = s.slackDeliver(ctx, client, *item, conv.ChatID, item.Reply)
		}
		if err != nil {
			if done, ferr := s.slackFailed(item.ID, err); done {
				if ferr != nil {
					return ferr
				}
				continue
			}
			return err
		}
		if err = s.Store.MarkBotReplySent(item.ID); err != nil {
			return err
		}
	}
}

// slackFailed records a Slack business rejection on the interaction. Other
// errors (network) return false so the connection restarts and the item is
// recovered later.
func (s *Service) slackFailed(id string, err error) (bool, error) {
	var business *SlackError
	if !errors.As(err, &business) {
		return false, nil
	}
	return true, s.Store.FailBotReply(id, business.Error())
}
