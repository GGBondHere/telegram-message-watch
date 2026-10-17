package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	menuPageSize      = 6
	menuActionTimeout = 30 * time.Second
	menuReplyTimeout  = 5 * time.Second
	menuInputLifetime = 10 * time.Minute
)

type Bot struct {
	token  string
	client *http.Client
}

type botError struct {
	code        int
	retryAfter  time.Duration
	description string
}

func (e *botError) Error() string {
	message := fmt.Sprintf("Telegram Bot API 请求失败（状态 %d）", e.code)
	if e.description != "" {
		message += "：" + e.description
	}
	return message
}

func NewBot(token string) *Bot {
	return &Bot{token: token, client: &http.Client{Timeout: 40 * time.Second}}
}

// Never return the authenticated request URL; redact credentials from API details.
func (b *Bot) api(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.New("无法编码 Bot API 请求")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("无法建立 Bot API 请求，请检查 Bot token")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			err = requestErr.Err
		}
		return fmt.Errorf("Telegram Bot API 网络请求失败：%s", redact(strings.ReplaceAll(err.Error(), b.token, "[凭据已隐藏]")))
	}
	defer res.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&envelope); err != nil {
		if res.StatusCode != http.StatusOK {
			return &botError{code: res.StatusCode, retryAfter: time.Second}
		}
		return errors.New("Telegram Bot API 返回无效数据")
	}
	if !envelope.OK || res.StatusCode != http.StatusOK {
		code := envelope.ErrorCode
		if code == 0 {
			code = res.StatusCode
		}
		delay := time.Duration(envelope.Parameters.RetryAfter) * time.Second
		if delay < time.Second {
			delay = time.Second
		}
		return &botError{code: code, retryAfter: delay, description: truncateText(redact(strings.ReplaceAll(envelope.Description, b.token, "[凭据已隐藏]")), 400)}
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return errors.New("无法读取 Telegram Bot API 结果")
		}
	}
	return nil
}

func botFatal(err error, polling bool) error {
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	var apiErr *botError
	if errors.As(err, &apiErr) {
		if apiErr.code == 401 || apiErr.code == 403 {
			return &ConfigError{Message: "通知 Bot 认证或私聊权限失效，请检查 token，并由管理员私聊 Bot 发送 /start"}
		}
		if polling && apiErr.code == 409 {
			return errors.New("Bot 轮询冲突（409）：请停止使用同一 Bot token 的其他实例，并检查是否配置了 webhook")
		}
	}
	return nil
}

func (b *Bot) Verify(ctx context.Context) (id int64, username string, err error) {
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
	}
	if err = b.api(ctx, "getMe", struct{}{}, &me); err != nil {
		if fatal := botFatal(err, false); fatal != nil {
			err = fatal
		}
		return 0, "", err
	}
	if me.ID == 0 || !me.IsBot {
		return 0, "", &ConfigError{Message: "getMe 未返回有效 Bot 身份"}
	}
	return me.ID, me.Username, nil
}

type botUser struct {
	ID int64 `json:"id"`
}

type botMessage struct {
	ID   int64 `json:"message_id"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From    *botUser `json:"from"`
	Text    string   `json:"text"`
	ReplyTo *struct {
		ID int64 `json:"message_id"`
	} `json:"reply_to_message"`
}

type botCallback struct {
	ID      string      `json:"id"`
	From    botUser     `json:"from"`
	Message *botMessage `json:"message"`
	Data    string      `json:"data"`
}

type botUpdate struct {
	ID       int64        `json:"update_id"`
	Message  *botMessage  `json:"message"`
	Callback *botCallback `json:"callback_query"`
}

func (b *Bot) updates(ctx context.Context, offset int64, timeout int) ([]botUpdate, error) {
	var updates []botUpdate
	err := b.api(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "limit": 100,
		"allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}

func waitBot(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run owns both goroutines and joins them before returning, including on ready
// failure. A shared cancellation context aborts long polls and in-flight sends.
func (b *Bot) Run(ctx context.Context, store *Store, tg TelegramAccess, ownerID int64, ready func() error) error {
	if ownerID <= 0 {
		return &ConfigError{Message: "通知管理员 owner_id 必须是有效用户 ID"}
	}
	if _, _, err := b.Verify(ctx); err != nil {
		return err
	}
	// Negative offset retains only the newest old update. The next request
	// acknowledges it, so management input from a previous run is never replayed.
	old, err := b.updates(ctx, -1, 0)
	if err != nil {
		if fatal := botFatal(err, true); fatal != nil {
			return fatal
		}
		return err
	}
	var offset int64
	for _, update := range old {
		if update.ID >= offset {
			offset = update.ID + 1
		}
	}
	initial, err := b.updates(ctx, offset, 0)
	if err != nil {
		if fatal := botFatal(err, true); fatal != nil {
			return fatal
		}
		return err
	}
	menu := &botMenu{bot: b, store: store, tg: tg, owner: ownerID, session: rand.Text()[:8]}
	if err := menu.publish(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- menu.poll(runCtx, offset, initial) }()
	go func() { results <- b.notices(runCtx, store) }()
	if ready != nil {
		if err := ready(); err != nil {
			cancel()
			<-results
			<-results
			return err
		}
	}
	err = <-results
	cancel()
	other := <-results
	if err == nil || errors.Is(err, context.Canceled) {
		if other != nil && !errors.Is(other, context.Canceled) {
			return other
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

func (b *Bot) notices(ctx context.Context, store *Store) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		enabled, err := store.Enabled()
		if err != nil {
			return err
		}
		var notice *Notice
		if enabled {
			notice, err = store.NextNotice(time.Now())
			if err != nil {
				return err
			}
		}
		if notice == nil {
			if err := waitBot(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		// Check pause again after retrieving a notice; NextNotice does not consume it.
		enabled, err = store.Enabled()
		if err != nil {
			return err
		}
		if !enabled {
			continue
		}
		body := notice.Body
		if notice.Link != "" {
			body += "\n\n" + notice.Link
		}
		err = b.send(ctx, notice.OwnerID, body, nil)
		if err == nil {
			if err = store.Sent(notice.ChatID, notice.MessageID); err != nil {
				return err
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatal := botFatal(err, false); fatal != nil {
			return fatal
		}
		var apiErr *botError
		permanent, count, delay := notice.Attempts >= 4, true, time.Duration(1<<min(notice.Attempts+1, 8))*time.Second
		rateLimited := false
		if errors.As(err, &apiErr) {
			if apiErr.code == 429 {
				permanent, count, delay = false, false, apiErr.retryAfter
				rateLimited = true
			} else if apiErr.code >= 400 && apiErr.code < 500 {
				permanent = true
			}
		}
		if err := store.Retry(*notice, delay, permanent, count); err != nil {
			return err
		}
		if permanent {
			logf("通知永久发送失败（群组 %d，消息 %d）：%v", notice.ChatID, notice.MessageID, err)
		} else {
			logf("通知发送失败（群组 %d，消息 %d），%s 后重试：%v", notice.ChatID, notice.MessageID, delay, err)
		}
		if rateLimited {
			if err := waitBot(ctx, delay); err != nil {
				return err
			}
		}
	}
}

type menuButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type menuMarkup struct {
	Rows [][]menuButton `json:"inline_keyboard"`
}

type menuView struct {
	text string
	rows [][]menuButton
}

type menuInput struct {
	chat, revision     int64
	kind, field, token string
	rule               Rule
	keywords           []string
	senders            []Sender
	rules              []Rule
	skipped            int
	expires            time.Time
	confirming         bool
	promptID           int64
	globalPaused       bool
}

type botMenu struct {
	bot       *Bot
	store     *Store
	tg        TelegramAccess
	owner     int64
	dialogs   []Chat
	loaded    bool
	published bool
	input     *menuInput
	session   string
	committed bool
	watched   bool
}

var botCommands = []struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}{
	{"start", "打开管理首页"},
	{"cancel", "取消当前监听项输入"},
	{"help", "查看使用说明"},
}

func (m *botMenu) publish(ctx context.Context) error {
	if m.published {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := m.bot.api(requestCtx, "setMyCommands", map[string]any{
		"commands": botCommands, "scope": map[string]string{"type": "all_private_chats"},
	}, nil)
	if err == nil {
		err = m.bot.api(requestCtx, "setChatMenuButton", map[string]any{"menu_button": map[string]string{"type": "commands"}}, nil)
	}
	if err != nil {
		if fatal := botFatal(err, false); fatal != nil {
			return fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logf("暂时无法注册 Bot 命令菜单，下次收到管理命令时重试：%v", err)
		return nil
	}
	m.published = true
	return nil
}

func button(text, data string) menuButton { return menuButton{Text: text, Data: data} }

func (b *Bot) send(ctx context.Context, chat int64, text string, rows [][]menuButton) error {
	payload := map[string]any{"chat_id": chat, "text": truncateText(text, 4096), "link_preview_options": map[string]bool{"is_disabled": true}}
	if rows != nil {
		payload["reply_markup"] = menuMarkup{Rows: rows}
	}
	return b.api(ctx, "sendMessage", payload, nil)
}

func (m *botMenu) answer(ctx context.Context, query *botCallback, text string, alert bool) error {
	ctx, cancel := context.WithTimeout(ctx, menuReplyTimeout)
	defer cancel()
	return m.bot.api(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": query.ID, "text": truncateText(text, 200), "show_alert": alert,
	}, nil)
}

func (m *botMenu) display(ctx context.Context, view menuView, query *botCallback) error {
	if view.text == "" { // An input prompt has already been sent and registered.
		return nil
	}
	rows := make([][]menuButton, len(view.rows))
	for i, row := range view.rows {
		for _, b := range row {
			b.Data = m.callbackData(b.Data)
			if len(b.Data) > 64 {
				return errors.New("管理按钮内容过长")
			}
			rows[i] = append(rows[i], b)
		}
	}
	view.rows = rows
	if query == nil {
		return m.bot.send(ctx, m.owner, view.text, view.rows)
	}
	err := m.bot.api(ctx, "editMessageText", map[string]any{
		"chat_id": m.owner, "message_id": query.Message.ID,
		"text": truncateText(view.text, 4096), "reply_markup": menuMarkup{Rows: view.rows},
	}, nil)
	// A 400 also covers clicking the current page. Sending a fresh view makes
	// deleted or uneditable menu messages usable without parsing error prose.
	var apiErr *botError
	if errors.As(err, &apiErr) && apiErr.code == 400 {
		return m.bot.send(ctx, m.owner, view.text, view.rows)
	}
	return err
}

func (m *botMenu) callbackData(data string) string {
	if m.session == "" {
		m.session = rand.Text()[:8]
	}
	return m.session + ":" + data
}

// Only a successfully delivered prompt becomes the current input step.
func (m *botMenu) promptInput(ctx context.Context, group WatchGroup, input *menuInput, hint string) (menuView, error) {
	view := inputView(group, input)
	if hint != "" {
		view.text = hint + "\n\n" + view.text
	}
	var message struct {
		ID int64 `json:"message_id"`
	}
	err := m.bot.api(ctx, "sendMessage", map[string]any{
		"chat_id": m.owner, "text": truncateText(view.text, 4096),
		"reply_markup": map[string]any{"force_reply": true, "input_field_placeholder": "回复此消息填写内容；/cancel 取消"},
	}, &message)
	if err != nil {
		return menuView{}, err
	}
	if message.ID <= 0 {
		return menuView{}, errors.New("无法确认输入提示，请重新打开菜单")
	}
	input.promptID = message.ID
	m.input = input
	return menuView{}, nil
}

func (m *botMenu) confirmation(group WatchGroup, input *menuInput, page int) (menuView, error) {
	enabled, err := m.store.Enabled()
	if err != nil {
		return menuView{}, err
	}
	input.globalPaused = !enabled
	return confirmInput(group, input, page), nil
}

func (m *botMenu) poll(ctx context.Context, offset int64, initial []botUpdate) error {
	updates := initial
	for {
		for _, update := range updates {
			if update.ID < offset {
				continue
			}
			offset = update.ID + 1
			if err := m.handle(ctx, update); err != nil {
				if fatal := botFatal(err, false); fatal != nil {
					return fatal
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logf("Bot 管理操作暂时失败：%v", err)
			}
		}
		var err error
		updates, err = m.bot.updates(ctx, offset, 25)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatal := botFatal(err, true); fatal != nil {
			return fatal
		}
		delay := 3 * time.Second
		var apiErr *botError
		if errors.As(err, &apiErr) && apiErr.code == 429 {
			delay = apiErr.retryAfter
		}
		logf("Bot 轮询暂时失败，%s 后重试：%v", delay, err)
		if err := waitBot(ctx, delay); err != nil {
			return err
		}
	}
}

func (m *botMenu) handle(ctx context.Context, update botUpdate) error {
	m.committed = false
	actionCtx, cancel := context.WithTimeout(ctx, menuActionTimeout)
	defer cancel()
	err := m.handleAction(actionCtx, update)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if fatal := botFatal(err, false); fatal != nil {
		return fatal
	}
	text := "操作失败，请重试；可发送 /start 重新打开菜单。"
	var hint menuHint
	switch {
	case m.committed:
		text = "操作已保存，但页面未能更新。请发送 /start 查看结果，无需重复提交。"
	case errors.As(err, &hint):
		text = string(hint)
	case errors.Is(err, ErrRuleChanged), errors.Is(err, errMenuExpired):
		text = "此按钮或输入已过期，请发送 /start 重新选择群组。当前正在进行的新批次不会被旧按钮修改。"
	case errors.Is(err, context.DeadlineExceeded):
		text = "本次操作超时，尚未保存。请重试，或发送 /start 重新打开菜单。"
	}
	logf("Bot 管理操作未能完成：%v", err)
	feedbackCtx, stop := context.WithTimeout(ctx, menuReplyTimeout)
	defer stop()
	if sendErr := m.bot.send(feedbackCtx, m.owner, text, nil); sendErr != nil {
		return sendErr
	}
	return nil
}

func (m *botMenu) handleAction(ctx context.Context, update botUpdate) error {
	if query := update.Callback; query != nil {
		if query.From.ID != m.owner || query.Message == nil || query.Message.Chat.ID != m.owner || query.Message.Chat.Type != "private" {
			return m.answer(ctx, query, "无权限：仅管理员可在私聊中管理。", true)
		}
		if err := m.answer(ctx, query, "", false); err != nil {
			var apiErr *botError
			if !errors.As(err, &apiErr) || apiErr.code != 400 {
				return err
			}
		}
		view, err := m.callback(ctx, query.Data)
		if err != nil {
			return err
		}
		return m.display(ctx, view, query)
	}
	message := update.Message
	if message == nil {
		return nil
	}
	if message.From == nil || message.From.ID != m.owner || message.Chat.ID != m.owner || message.Chat.Type != "private" {
		if message.Chat.Type == "private" && strings.HasPrefix(message.Text, "/") {
			return m.bot.send(ctx, message.Chat.ID, "这是私人监听器，你没有管理权限。", nil)
		}
		return nil
	}
	if strings.HasPrefix(message.Text, "/") {
		name := strings.ToLower(strings.SplitN(strings.Fields(message.Text)[0], "@", 2)[0])
		if name == "/start" || name == "/cancel" || name == "/help" || m.input == nil {
			awaiting := m.input != nil
			m.input = nil
			if err := m.publish(ctx); err != nil {
				return err
			}
			view, err := m.command(name, awaiting)
			if err != nil {
				if fatal := botFatal(err, false); fatal != nil {
					return fatal
				}
				logf("Bot 命令操作失败：%v", err)
				return m.bot.send(ctx, m.owner, "操作失败，请重试；可查看服务日志。", nil)
			}
			return m.display(ctx, view, nil)
		}
	}
	if message.Text == "" {
		return nil
	}
	if m.input != nil && (message.ReplyTo == nil || message.ReplyTo.ID != m.input.promptID) {
		return menuHint("请回复当前输入提示填写内容；旧提示不能用于其他群组或新批次。也可发送 /cancel 取消。")
	}
	return m.addInput(ctx, message.Text)
}

func (m *botMenu) home() (menuView, error) {
	groups, err := m.store.Groups()
	if err != nil {
		return menuView{}, err
	}
	enabled, err := m.store.Enabled()
	if err != nil {
		return menuView{}, err
	}
	count := 0
	for _, group := range groups {
		if len(group.Rules) > 0 {
			count++
		}
	}
	state, toggle, data := "开启", "⏸ 暂停全部监听", "pause:1"
	if !enabled {
		state, toggle, data = "全部暂停", "▶ 恢复全部监听", "pause:0"
	}
	return menuView{fmt.Sprintf("Telegram Message Watch\n监听：%s\n已配置群组与频道：%d\n选择群组后，添加你想关注的内容。", state, count), [][]menuButton{
		{button("群组 / 频道", "list:0"), button("已监听", "watched:0")},
		{button(toggle, data), button("发送验证提醒", "notify")},
	}}, nil
}

func pageBounds(page, count int) (int, int, int) {
	last := max(0, (count-1)/menuPageSize)
	page = max(0, min(page, last))
	start := page * menuPageSize
	return page, start, min(start+menuPageSize, count)
}

// Base-36 IDs keep callback data below Telegram's byte limit.
func groupKey(group WatchGroup) string {
	return strconv.FormatInt(group.ChatID, 36) + ":" + strconv.FormatInt(group.Revision, 36)
}
func backToGroup(group WatchGroup, page int) menuButton {
	return button("返回当前群组", fmt.Sprintf("group:%s:%d", groupKey(group), page))
}
func groupRule(group WatchGroup, id int64) (Rule, int, error) {
	for _, set := range groupedRules(group.Rules) {
		for index, rule := range set.rules {
			if rule.ID == id {
				return rule, index / menuPageSize, nil
			}
		}
	}
	return Rule{}, 0, errMenuExpired
}

func groupSet(group WatchGroup, id int64) (ruleSet, int, error) {
	for index, set := range groupedRules(group.Rules) {
		for _, rule := range set.rules {
			if rule.ID == id {
				return set, index / menuPageSize, nil
			}
		}
	}
	return ruleSet{}, 0, errMenuExpired
}

func setKey(group WatchGroup, set ruleSet) string {
	return groupKey(group) + ":" + strconv.FormatInt(set.rules[0].ID, 36)
}

func backToSet(group WatchGroup, set ruleSet, page int) menuButton {
	return button("返回监听项", fmt.Sprintf("set:%s:%d", setKey(group, set), page))
}

func (m *botMenu) refreshDialogs(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, menuActionTimeout)
	defer cancel()
	dialogs, err := m.tg.Dialogs(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(dialogs, func(i, j int) bool {
		left, right := fold(dialogs[i].Title), fold(dialogs[j].Title)
		if left == right {
			return dialogs[i].ID < dialogs[j].ID
		}
		return left < right
	})
	m.dialogs, m.loaded = dialogs, true
	return nil
}

func (m *botMenu) requireJoined(ctx context.Context, chat int64) error {
	if err := m.refreshDialogs(ctx); err != nil {
		return err
	}
	for _, dialog := range m.dialogs {
		if dialog.ID == chat {
			return nil
		}
	}
	return menuHint("当前监听账号无法访问该群组或频道，请先加入，再刷新群组列表。")
}

func (m *botMenu) groups(ctx context.Context, page int, refresh, watched bool) (menuView, error) {
	if !m.loaded || refresh {
		if err := m.refreshDialogs(ctx); err != nil {
			return menuView{}, err
		}
	}
	groups, err := m.store.Groups()
	if err != nil {
		return menuView{}, err
	}
	byID := make(map[int64]WatchGroup, len(groups))
	for _, group := range groups {
		byID[group.ChatID] = group
	}
	var dialogs []Chat
	for _, chat := range m.dialogs {
		if !watched || len(byID[chat.ID].Rules) > 0 {
			dialogs = append(dialogs, chat)
		}
	}
	page, start, end := pageBounds(page, len(dialogs))
	heading, route, refreshRoute := "群组 / 频道", "list", "refresh"
	if watched {
		heading, route, refreshRoute = "已监听", "watched", "watched-refresh"
	}
	view := menuView{text: fmt.Sprintf("%s · 第 %d 页\n选择要管理的群组或频道。✅ 表示已添加监听，暂停后仍保留。", heading, page+1)}
	if len(dialogs) == 0 {
		view.text += "\n暂无可显示的群组或频道。"
	}
	for _, chat := range dialogs[start:end] {
		group, ok := byID[chat.ID]
		if !ok {
			group = WatchGroup{ChatID: chat.ID, Revision: -1}
		}
		mark, suffix := "☐ ", ""
		if len(group.Rules) > 0 {
			mark = "✅ "
			if !group.Enabled {
				suffix = " · 已暂停"
			}
		}
		label := fmt.Sprintf("%s%s · %d 项监听%s", mark, truncateText(chat.Title, 40), len(groupedRules(group.Rules)), suffix)
		view.rows = append(view.rows, []menuButton{button(label, "group:"+groupKey(group)+":0")})
	}
	var nav []menuButton
	if page > 0 {
		nav = append(nav, button("上一页", fmt.Sprintf("%s:%d", route, page-1)))
	}
	if end < len(dialogs) {
		nav = append(nav, button("下一页", fmt.Sprintf("%s:%d", route, page+1)))
	}
	if len(nav) > 0 {
		view.rows = append(view.rows, nav)
	}
	view.rows = append(view.rows, []menuButton{button("刷新群组列表", refreshRoute), button("返回首页", "home")})
	m.watched = watched
	return view, nil
}

func (m *botMenu) group(chat int64, page int) (menuView, error) {
	group, err := m.store.Group(chat)
	if err != nil {
		return menuView{}, err
	}
	if group.Revision < 0 {
		return menuView{}, errMenuExpired
	}
	enabled, err := m.store.Enabled()
	if err != nil {
		return menuView{}, err
	}
	status, toggle := "监听中", "暂停本群组"
	if !group.Enabled {
		status, toggle = "本群组已暂停", "恢复本群组"
	} else if !enabled {
		status = "全部监听已暂停"
	} else {
		active := false
		for _, rule := range group.Rules {
			active = active || rule.Enabled && !rule.SetPaused
		}
		if !active {
			status = "暂无开启的监听项"
		}
	}
	sets := groupedRules(group.Rules)
	page, start, end := pageBounds(page, len(sets))
	key := groupKey(group)
	view := menuView{text: fmt.Sprintf("%s\n%s\n共 %d 项监听 · 第 %d 页", truncateText(group.Title, 120), status, len(sets), page+1)}
	for index := start; index < end; index++ {
		set := sets[index]
		mark := "✅"
		if !set.active() {
			mark = "⏸"
		}
		label := fmt.Sprintf("%s %d. %s", mark, index+1, truncateText(set.description(), 48))
		view.rows = append(view.rows, []menuButton{button(label, "set:"+setKey(group, set)+":0")})
	}
	if len(sets) == 0 {
		view.text += "\n\n尚未添加监听。"
	}
	var nav []menuButton
	if page > 0 {
		nav = append(nav, button("上一页", fmt.Sprintf("group:%s:%d", key, page-1)))
	}
	if end < len(sets) {
		nav = append(nav, button("下一页", fmt.Sprintf("group:%s:%d", key, page+1)))
	}
	if len(nav) > 0 {
		view.rows = append(view.rows, nav)
	}
	listRoute := "list:0"
	if m.watched {
		listRoute = "watched:0"
	}
	view.rows = append(view.rows, []menuButton{button("＋ 添加监听", "add:"+key)}, []menuButton{button(toggle, "group-toggle:"+key), button("返回群组列表", listRoute)})
	return view, nil
}

func (m *botMenu) set(chat, anchor int64, page int) (menuView, error) {
	group, err := m.store.Group(chat)
	if err != nil {
		return menuView{}, err
	}
	set, groupPage, err := groupSet(group, anchor)
	if err != nil {
		return menuView{}, err
	}
	page, start, end := pageBounds(page, len(set.rules))
	state, toggle, add := "已开启", "暂停这项监听", "＋ 添加关键词"
	if set.paused {
		state, toggle = "已暂停", "恢复这项监听"
	} else if !set.active() {
		state = "所有条件已暂停"
	}
	if set.key.kind == "s" {
		add = "＋ 添加发送者"
	}
	view := menuView{text: fmt.Sprintf("%s\n%s\n状态：%s\n共 %d 个条件 · 第 %d 页", truncateText(group.Title, 120), set.heading(), state, len(set.rules), page+1)}
	enabled, err := m.store.Enabled()
	if err != nil {
		return menuView{}, err
	}
	if !enabled {
		view.text += "\n当前不提醒：全部监听已暂停。"
	} else if !group.Enabled {
		view.text += "\n当前不提醒：本群组已暂停。"
	}
	key := setKey(group, set)
	for i := start; i < end; i++ {
		rule := set.rules[i]
		mark := "✅"
		if !rule.Enabled {
			mark = "⏸"
		}
		value := rule.Keyword
		if set.key.kind == "s" {
			value = senderLabel(rule.Sender)
		}
		view.rows = append(view.rows, []menuButton{button(fmt.Sprintf("%s %d. %s", mark, i+1, truncateText(value, 48)), "item:"+groupKey(group)+":"+strconv.FormatInt(rule.ID, 36))})
	}
	var nav []menuButton
	if page > 0 {
		nav = append(nav, button("上一页", fmt.Sprintf("set:%s:%d", key, page-1)))
	}
	if end < len(set.rules) {
		nav = append(nav, button("下一页", fmt.Sprintf("set:%s:%d", key, page+1)))
	}
	if len(nav) > 0 {
		view.rows = append(view.rows, nav)
	}
	view.rows = append(view.rows, []menuButton{button(add, "set-add:"+key)}, []menuButton{button(toggle, "set-toggle:"+key), button("删除这项监听", "set-remove:"+key)}, []menuButton{backToGroup(group, groupPage)})
	return view, nil
}

func (m *botMenu) item(chat, id int64) (menuView, error) {
	group, err := m.store.Group(chat)
	if err != nil {
		return menuView{}, err
	}
	rule, page, err := groupRule(group, id)
	if err != nil {
		return menuView{}, err
	}
	set, _, err := groupSet(group, id)
	if err != nil {
		return menuView{}, err
	}
	state, toggle := "已开启", "暂停此条件"
	if !rule.Enabled {
		state, toggle = "已暂停", "恢复此条件"
	}
	key := groupKey(group) + ":" + strconv.FormatInt(id, 36)
	view := menuView{text: fmt.Sprintf("%s\n\n%s\n\n状态：%s", truncateText(group.Title, 120), rule.description(), state)}
	enabled, err := m.store.Enabled()
	if err != nil {
		return menuView{}, err
	}
	if reason := pauseReason(group, rule, enabled); reason != "" {
		view.text += "\n当前不提醒：" + reason + "。"
	}
	if rule.Keyword != "" {
		view.rows = append(view.rows, []menuButton{button("修改关键词", "edit:"+key+":k")})
	}
	if rule.Sender.ID != 0 {
		view.text += fmt.Sprintf("\n发送者 ID：%d", rule.Sender.ID)
		view.rows = append(view.rows, []menuButton{button("修改发送者", "edit:"+key+":s")})
	}
	view.rows = append(view.rows, []menuButton{button(toggle, "item-toggle:"+key), button("删除此条件", "remove:"+key)}, []menuButton{backToSet(group, set, page)})
	return view, nil
}

func addMenu(group WatchGroup) menuView {
	key := groupKey(group)
	return menuView{fmt.Sprintf("%s\n添加监听\n\n你想关注什么？", truncateText(group.Title, 120)), [][]menuButton{
		{button("含某个关键词的消息", "new:"+key+":k")},
		{button("某个人的全部消息", "new:"+key+":s")},
		{button("某个人发出的含关键词消息", "new:"+key+":b")},
		{backToGroup(group, 0)},
	}}
}

func splitBatchInput(text string, limit int) ([]string, error) {
	parts := strings.FieldsFunc(text, func(ch rune) bool {
		return strings.ContainsRune(",，、;；\r\n", ch)
	})
	var values []string
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if utf8.RuneCountInString(value) > 80 {
			return nil, fmt.Errorf("第 %d 项超过 80 字符，请缩短后重新发送整批内容。", len(values)+1)
		}
		values = append(values, value)
	}
	if len(values) == 0 || len(values) > limit {
		return nil, fmt.Errorf("一次填写 1～%d 项，可用逗号、分号、顿号或换行分隔。", limit)
	}
	return values, nil
}

func inputView(group WatchGroup, input *menuInput) menuView {
	title, content := "关键词", "发送要监听的关键词，例如：补货, 优惠, 活动。\n按普通文字匹配，不支持正则表达式。"
	if input.field == "s" {
		title, content = "发送者", "发送 @用户名或数字 ID，例如：@xiaowang, @xiaoli。"
	}
	action := "添加"
	limit := fmt.Sprintf("一次 1～%d 项，每项最多 80 字符。\n可用逗号、分号、顿号或换行分隔，按输入顺序添加。", ruleBatchLimit)
	if input.rule.ID != 0 {
		action = "修改"
		limit = "只修改当前这一项，最多 80 字符，不支持换行。"
		if input.field == "k" {
			content = "发送新的关键词。按普通文字匹配，不支持正则表达式。"
		} else {
			content = "发送新的 @用户名或数字 ID。"
		}
	} else if input.kind == "b" && input.field == "s" {
		content = "发送一个 @用户名或数字 ID，下一步填写关键词。\n也可一次发送：@xiaowang, 补货, 优惠。"
		limit = fmt.Sprintf("第一个值是发送者，其余是关键词（最多 %d 个）。每项最多 80 字符。\n可用逗号、分号、顿号或换行分隔。", ruleBatchLimit)
	}
	text := fmt.Sprintf("%s\n%s%s\n\n%s\n%s\n/cancel 取消。", truncateText(group.Title, 120), action, title, content, limit)
	if input.field == "k" && input.rule.Sender.ID != 0 {
		text += "\n\n发送者：" + truncateText(senderLabel(input.rule.Sender), 160)
	}
	return menuView{text, [][]menuButton{{backToGroup(group, 0)}}}
}

func confirmInput(group WatchGroup, input *menuInput, page int) menuView {
	// A batch belongs to one entry. Split only long summaries, never repeat an
	// entire listener row per keyword. All pages share the same draft token.
	set := groupedRules(input.rules)[0]
	var pages []string
	chunk := ""
	for _, value := range set.values() {
		candidate := value
		if chunk != "" {
			candidate = chunk + "、" + value
		}
		if truncateText(candidate, 1800) != candidate {
			pages = append(pages, chunk)
			chunk = value
		} else {
			chunk = candidate
		}
	}
	pages = append(pages, chunk)
	page = max(0, min(page, len(pages)-1))
	text := fmt.Sprintf("确认添加 %d 个%s\n群组：%s\n\n%s：%s", len(input.rules), batchNoun(input.kind), truncateText(group.Title, 120), set.heading(), pages[page])
	if len(pages) > 1 {
		text += fmt.Sprintf("\n第 %d / %d 页", page+1, len(pages))
	}
	if input.skipped > 0 {
		text += fmt.Sprintf("\n\n已跳过 %d 个重复或已有项目。", input.skipped)
	}
	text += "\n\n确认后按输入顺序归入同一项监听。"
	for _, old := range group.Rules {
		if old.setKey() == set.key && old.SetPaused {
			text += "该监听项已暂停，新增条件也将在恢复后生效。"
			break
		}
	}
	if !group.Enabled {
		text += "当前群组已暂停，恢复本群组后才会提醒。"
	}
	if input.globalPaused {
		text += "全部监听已暂停。"
	}
	text += overlapWarnings(group, input.rules, !input.globalPaused)
	key := groupKey(group)
	var rows [][]menuButton
	var nav []menuButton
	if page > 0 {
		nav = append(nav, button("上一页", fmt.Sprintf("preview:%s:%s:%d", key, input.token, page-1)))
	}
	if page+1 < len(pages) {
		nav = append(nav, button("下一页", fmt.Sprintf("preview:%s:%s:%d", key, input.token, page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []menuButton{button("确认添加", "save:"+key+":"+input.token), button("取消", "add:"+key)})
	return menuView{text, rows}
}

func batchNoun(kind string) string {
	if kind == "s" {
		return "发送者"
	}
	return "关键词"
}

func (m *botMenu) command(name string, awaiting bool) (menuView, error) {
	switch name {
	case "/start":
		return m.home()
	case "/cancel":
		text := "当前没有待完成的监听项输入。"
		if awaiting {
			text = "已取消当前监听项输入。"
		}
		return menuView{text, [][]menuButton{{button("返回首页", "home")}}}, nil
	case "/help":
		text := "使用个人账号监听已加入的群组/频道，匹配后由 Bot 私聊提醒。\n"
		for _, command := range botCommands {
			text += "\n/" + command.Command + " — " + command.Description
		}
		text += "\n\n在首页选择群组/频道，点击“添加监听”。\n关键词集中在一项，指定发送者集中在一项；组合监听每人一项。未添加的类型不显示。\n可用逗号、分号、顿号或换行批量输入，按输入顺序排列。\n组合监听可先填一个发送者，再批量填关键词；也可一次填写“发送者, 关键词, 关键词”。\n点击监听项管理其中的条件，可继续添加、暂停或删除。\n同一条消息命中多项也只提醒一次。\n关键词按普通文字匹配，不支持正则表达式。\n首页可以暂停全部监听、发送验证提醒。"
		return menuView{text, [][]menuButton{{button("群组 / 频道", "list:0")}}}, nil
	default:
		return menuView{text: "未知命令，请发送 /start 打开菜单。", rows: [][]menuButton{{button("首页", "home")}}}, nil
	}
}

var errMenuExpired = errors.New("管理按钮已过期")

type menuHint string

func (e menuHint) Error() string { return string(e) }

func (m *botMenu) callback(ctx context.Context, data string) (view menuView, result error) {
	parts := strings.SplitN(data, ":", 2)
	if m.session == "" || len(parts) != 2 || parts[0] != m.session {
		return menuView{}, errMenuExpired
	}
	pending := m.input
	m.committed = false
	m.input = nil
	defer func() {
		if result != nil && !m.committed && m.input == nil {
			m.input = pending // Rejected/failed clicks cannot discard a newer draft.
		}
	}()
	return m.route(ctx, parts[1], pending)
}

func (m *botMenu) route(ctx context.Context, data string, pending *menuInput) (menuView, error) {
	parts := strings.Split(data, ":")
	switch parts[0] {
	case "home":
		return m.home()
	case "refresh", "watched-refresh":
		if len(parts) != 1 {
			return menuView{}, errMenuExpired
		}
		return m.groups(ctx, 0, true, parts[0] == "watched-refresh")
	case "list", "watched":
		if len(parts) != 2 {
			return menuView{}, errMenuExpired
		}
		page, err := strconv.Atoi(parts[1])
		if err != nil || page < 0 {
			return menuView{}, errMenuExpired
		}
		return m.groups(ctx, page, false, parts[0] == "watched")
	case "pause":
		if len(parts) != 2 || (parts[1] != "0" && parts[1] != "1") {
			return menuView{}, errMenuExpired
		}
		if err := m.store.Pause(ctx, parts[1] == "1"); err != nil {
			return menuView{}, err
		}
		m.committed = true
		return m.home()
	case "notify":
		if err := m.bot.send(ctx, m.owner, "✅ 通知 Bot 可以正常私聊你。", nil); err != nil {
			return menuView{}, err
		}
		return m.home()
	}
	if len(parts) < 3 {
		return menuView{}, errMenuExpired
	}
	chat, err := strconv.ParseInt(parts[1], 36, 64)
	if err != nil || chat == 0 {
		return menuView{}, errMenuExpired
	}
	revision, err := strconv.ParseInt(parts[2], 36, 64)
	if err != nil || revision < -1 {
		return menuView{}, errMenuExpired
	}
	group, err := m.store.Group(chat)
	if err != nil {
		return menuView{}, err
	}
	if group.Revision != revision {
		return menuView{}, errMenuExpired
	}
	if parts[0] == "group" && len(parts) == 4 {
		page, err := strconv.Atoi(parts[3])
		if err != nil || page < 0 {
			return menuView{}, errMenuExpired
		}
		known := false
		for _, dialog := range m.dialogs {
			if dialog.ID == chat {
				known = true
				if err := m.store.SetChat(ctx, chat, dialog.Title); err != nil {
					return menuView{}, err
				}
				break
			}
		}
		if !known {
			return menuView{}, menuHint("当前群组列表中已没有该群组，请刷新列表后重新选择。")
		}
		return m.group(chat, page)
	}
	if group.Revision < 0 {
		return menuView{}, errMenuExpired
	}
	key := groupKey(group)
	switch parts[0] {
	case "group-toggle":
		if len(parts) != 3 {
			return menuView{}, errMenuExpired
		}
		if !group.Enabled {
			if err := m.requireJoined(ctx, chat); err != nil {
				return menuView{}, err
			}
		}
		if err := m.store.SetGroupEnabled(ctx, chat, !group.Enabled, revision); err != nil {
			return menuView{}, err
		}
		m.committed = true
		return m.group(chat, 0)
	case "add":
		if len(parts) == 3 {
			return addMenu(group), nil
		}
	case "new":
		if len(parts) != 4 || (parts[3] != "k" && parts[3] != "s" && parts[3] != "b") {
			return menuView{}, errMenuExpired
		}
		if err := m.requireJoined(ctx, chat); err != nil {
			return menuView{}, err
		}
		field := "k"
		if parts[3] != "k" {
			field = "s"
		}
		input := &menuInput{chat: chat, revision: revision, kind: parts[3], field: field, token: rand.Text()[:12], expires: time.Now().Add(menuInputLifetime)}
		return m.promptInput(ctx, group, input, "")
	case "preview", "save":
		if len(parts) < 4 || pending == nil || !pending.confirming || pending.chat != chat || pending.revision != revision || pending.token != parts[3] || time.Now().After(pending.expires) {
			return menuView{}, errMenuExpired
		}
		if parts[0] == "preview" {
			if len(parts) != 5 {
				return menuView{}, errMenuExpired
			}
			page, err := strconv.Atoi(parts[4])
			if err != nil || page < 0 {
				return menuView{}, errMenuExpired
			}
			m.input = pending
			return m.confirmation(group, pending, page)
		}
		if len(parts) != 4 {
			return menuView{}, errMenuExpired
		}
		if err := m.requireJoined(ctx, chat); err != nil {
			m.input = pending
			return menuView{}, err
		}
		added, skipped, err := m.store.AddRules(ctx, chat, pending.rules, revision)
		if err != nil {
			if !errors.Is(err, ErrRuleChanged) {
				m.input = pending
			}
			return menuView{}, err
		}
		m.committed = true
		current, err := m.store.Group(chat)
		if err != nil {
			return menuView{}, err
		}
		page := 0
		for i, set := range groupedRules(current.Rules) {
			if set.key == pending.rules[0].setKey() {
				page = i / menuPageSize
				break
			}
		}
		view, err := m.group(chat, page)
		message := fmt.Sprintf("已添加 %d 个%s，归入对应监听项。", added, batchNoun(pending.kind))
		if skipped += pending.skipped; skipped > 0 {
			message += fmt.Sprintf("已跳过 %d 个重复或已有项目。", skipped)
		}
		view.text = message + "\n" + view.text
		return view, err
	case "set", "set-add", "set-toggle", "set-remove", "set-delete":
		if len(parts) < 4 {
			return menuView{}, errMenuExpired
		}
		anchor, err := strconv.ParseInt(parts[3], 36, 64)
		if err != nil || anchor <= 0 {
			return menuView{}, errMenuExpired
		}
		set, page, err := groupSet(group, anchor)
		if err != nil {
			return menuView{}, err
		}
		skey := setKey(group, set)
		if parts[0] == "set" {
			if len(parts) != 5 {
				return menuView{}, errMenuExpired
			}
			memberPage, err := strconv.Atoi(parts[4])
			if err != nil || memberPage < 0 {
				return menuView{}, errMenuExpired
			}
			return m.set(chat, anchor, memberPage)
		}
		if len(parts) != 4 {
			return menuView{}, errMenuExpired
		}
		switch parts[0] {
		case "set-add":
			if err := m.requireJoined(ctx, chat); err != nil {
				return menuView{}, err
			}
			field := "k"
			if set.key.kind == "s" {
				field = "s"
			}
			input := &menuInput{chat: chat, revision: revision, kind: set.key.kind, field: field, token: rand.Text()[:12], expires: time.Now().Add(menuInputLifetime)}
			if set.key.kind == "b" {
				input.rule.Sender = set.rules[0].Sender
			}
			return m.promptInput(ctx, group, input, "")
		case "set-toggle":
			if set.paused {
				if err := m.requireJoined(ctx, chat); err != nil {
					return menuView{}, err
				}
			}
			if err := m.store.changeSet(ctx, chat, anchor, revision, false, !set.paused); err != nil {
				return menuView{}, err
			}
			m.committed = true
			return m.set(chat, anchor, 0)
		case "set-remove":
			return menuView{fmt.Sprintf("删除这项监听？\n\n群组：%s\n%s\n\n将删除其中全部 %d 个条件。其他监听项保持不变。", truncateText(group.Title, 120), truncateText(set.description(), 1200), len(set.rules)), [][]menuButton{{button("确认删除", "set-delete:"+skey), button("取消", "set:"+skey+":0")}}}, nil
		case "set-delete":
			if err := m.store.changeSet(ctx, chat, anchor, revision, true, false); err != nil {
				return menuView{}, err
			}
			m.committed = true
			return m.group(chat, page)
		}
	case "item", "item-toggle", "edit", "remove", "delete":
		if len(parts) < 4 {
			return menuView{}, errMenuExpired
		}
		id, err := strconv.ParseInt(parts[3], 36, 64)
		if err != nil || id <= 0 {
			return menuView{}, errMenuExpired
		}
		rule, page, err := groupRule(group, id)
		if err != nil {
			return menuView{}, err
		}
		itemKey := key + ":" + parts[3]
		switch parts[0] {
		case "item":
			if len(parts) == 4 {
				return m.item(chat, id)
			}
		case "item-toggle":
			if len(parts) != 4 {
				return menuView{}, errMenuExpired
			}
			if !rule.Enabled {
				if err := m.requireJoined(ctx, chat); err != nil {
					return menuView{}, err
				}
			}
			if err := m.store.SetRuleEnabled(ctx, chat, id, !rule.Enabled, revision); err != nil {
				return menuView{}, err
			}
			m.committed = true
			return m.item(chat, id)
		case "edit":
			if len(parts) != 5 || (parts[4] != "k" && parts[4] != "s") || parts[4] == "k" && rule.Keyword == "" || parts[4] == "s" && rule.Sender.ID == 0 {
				return menuView{}, errMenuExpired
			}
			input := &menuInput{chat: chat, revision: revision, field: parts[4], rule: rule, token: rand.Text()[:12], expires: time.Now().Add(menuInputLifetime)}
			return m.promptInput(ctx, group, input, "")
		case "remove":
			if len(parts) == 4 {
				return menuView{fmt.Sprintf("删除此条件？\n\n群组：%s\n%s\n\n其他条件保持不变。", truncateText(group.Title, 120), rule.description()), [][]menuButton{{button("确认删除", "delete:"+itemKey), button("取消", "item:"+itemKey)}}}, nil
			}
		case "delete":
			if len(parts) != 4 {
				return menuView{}, errMenuExpired
			}
			if err := m.store.DeleteRule(ctx, chat, id, revision); err != nil {
				return menuView{}, err
			}
			m.committed = true
			current, err := m.store.Group(chat)
			if err != nil {
				return menuView{}, err
			}
			for _, set := range groupedRules(current.Rules) {
				if set.key == rule.setKey() {
					return m.set(chat, set.rules[0].ID, page)
				}
			}
			_, groupPage, _ := groupSet(group, id)
			return m.group(chat, groupPage)
		}
	}
	return menuView{}, errMenuExpired
}

func (m *botMenu) resolveSender(ctx context.Context, value string) (Sender, error) {
	id, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr == nil && (id > 0 || id < -channelIDOffset) {
		return Sender{ID: id, Label: value}, nil
	}
	numeric := strings.TrimLeft(value, "+-")
	if numeric == "" || strings.Trim(numeric, "0123456789") == "" {
		return Sender{}, errors.New("无效发送者 ID")
	}
	sender, err := m.tg.ResolveSender(ctx, strings.TrimPrefix(value, "@"))
	if err != nil {
		return Sender{}, err
	}
	if sender.ID == 0 {
		return Sender{}, errors.New("发送者 ID 无效")
	}
	if sender.Label == "" {
		sender.Label = value
	}
	return sender, nil
}

func (m *botMenu) addInput(ctx context.Context, text string) error {
	current := m.input
	if current == nil {
		return m.bot.send(ctx, m.owner, "请发送 /start 打开菜单。", nil)
	}
	draft := *current
	input := &draft
	group, err := m.store.Group(input.chat)
	if err != nil {
		return err
	}
	retry := func(message string) error {
		_, err := m.promptInput(ctx, group, current, message)
		return err
	}
	if group.Revision != input.revision || time.Now().After(input.expires) {
		m.input = nil
		return m.bot.send(ctx, m.owner, "输入已过期或监听设置已改变，请重新打开当前群组。", nil)
	}
	if input.confirming {
		return m.bot.send(ctx, m.owner, "请点击“确认添加”，或 /cancel 取消。", nil)
	}
	text = strings.TrimSpace(text)
	values := []string{text}
	if input.rule.ID != 0 {
		if text == "" || utf8.RuneCountInString(text) > 80 || strings.ContainsAny(text, "\r\n") {
			return retry("只修改当前这一项，最多 80 字符，不支持换行。请重新填写。")
		}
	} else {
		limit := ruleBatchLimit
		if input.kind == "b" && input.field == "s" {
			limit++ // One sender plus up to ruleBatchLimit keywords.
		}
		values, err = splitBatchInput(text, limit)
		if err != nil {
			return retry(err.Error() + "\n本批尚未添加，请重新填写。")
		}
		if input.kind == "b" && input.field == "s" {
			input.keywords, values = values[1:], values[:1]
		}
	}
	if input.field == "k" {
		if input.rule.ID != 0 {
			input.rule.Keyword = values[0]
		} else {
			input.keywords = values
		}
	} else {
		senders := make([]Sender, 0, len(values))
		for i, value := range values {
			sender, err := m.resolveSender(ctx, value)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logf("解析监听项发送者失败：%v", err)
				return retry(fmt.Sprintf("第 %d 项发送者无法识别，请检查 @用户名或数字 ID。\n本批尚未保存，请重新填写整批内容。", i+1))
			}
			senders = append(senders, sender)
		}
		if input.rule.ID != 0 || input.kind == "b" {
			input.rule.Sender = senders[0]
		} else {
			input.senders = senders
		}
	}
	if input.rule.ID != 0 {
		if err := m.requireJoined(ctx, input.chat); err != nil {
			return err
		}
		if err := m.store.UpdateRule(ctx, input.chat, input.rule, input.revision); err != nil {
			if errors.Is(err, ErrDuplicateRule) {
				return retry("当前群组已存在相同条件，请重新填写。")
			}
			if errors.Is(err, ErrRuleChanged) {
				m.input = nil
				return m.bot.send(ctx, m.owner, "监听设置已改变，请重新打开当前群组。", nil)
			}
			return err
		}
		m.committed = true
		m.input = nil
		view, err := m.item(input.chat, input.rule.ID)
		if err != nil {
			return err
		}
		view.text = "已保存。\n" + view.text
		updated, err := m.store.Group(input.chat)
		if err != nil {
			return err
		}
		rule, _, err := groupRule(updated, input.rule.ID)
		if err != nil {
			return err
		}
		enabled, err := m.store.Enabled()
		if err != nil {
			return err
		}
		view.text += overlapWarnings(updated, []Rule{rule}, enabled)
		return m.display(ctx, view, nil)
	}
	if input.kind == "b" && input.field == "s" {
		input.field = "k"
		if len(input.keywords) == 0 {
			_, err := m.promptInput(ctx, group, input, "")
			return err
		}
	}
	var candidates []Rule
	if input.kind == "s" {
		for _, sender := range input.senders {
			candidates = append(candidates, Rule{Sender: sender})
		}
	} else {
		for _, keyword := range input.keywords {
			candidates = append(candidates, Rule{Keyword: keyword, Sender: input.rule.Sender})
		}
	}
	input.rules, input.skipped = distinctNewRules(group.Rules, candidates)
	if len(input.rules) == 0 {
		m.input = nil
		view, err := m.group(input.chat, 0)
		if err != nil {
			return err
		}
		view.text = "这些监听项均已存在，没有新增；已有项目的开关状态保持不变。\n" + view.text
		return m.display(ctx, view, nil)
	}
	input.confirming = true
	input.token = rand.Text()[:12]
	view, err := m.confirmation(group, input, 0)
	if err != nil {
		return err
	}
	if err := m.display(ctx, view, nil); err != nil {
		return err
	}
	m.input = input
	return nil
}
