package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const channelIDOffset int64 = 1000000000000

// Session contents are authentication credentials. Never print them or use the
// SDK's plain FileStorage, which does not provide our atomic-write guarantees.
type telegramSession struct {
	path string
	mu   sync.Mutex
}

func (s *telegramSession) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	return b, err
}

func (s *telegramSession) StoreSession(ctx context.Context, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return atomicWrite(s.path, b, 0600)
}

type telegramLoginPrompt struct {
	prompt func(string, bool) (string, error)
}

func (p telegramLoginPrompt) ask(ctx context.Context, label string, secret bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := p.prompt(label, secret)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("登录输入不能为空")
	}
	return value, nil
}

func (p telegramLoginPrompt) Phone(ctx context.Context) (string, error) {
	value, err := p.ask(ctx, "手机号（包含国家码，例如 +86）：", false)
	return strings.TrimSpace(value), err
}

func (p telegramLoginPrompt) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	value, err := p.ask(ctx, "Telegram 验证码：", true)
	return strings.TrimSpace(value), err
}

func (p telegramLoginPrompt) Password(ctx context.Context) (string, error) {
	return p.ask(ctx, "Telegram 两步验证密码：", true)
}

func (p telegramLoginPrompt) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("请先在 Telegram 官方客户端注册个人账号")
}

func (p telegramLoginPrompt) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("不支持在此程序注册 Telegram 账号")
}

func LoginTelegram(ctx context.Context, cfg Config, path string, prompt func(string, bool) (string, error)) (ownerID int64, name string, err error) {
	if cfg.APIID <= 0 || strings.TrimSpace(cfg.APIHash) == "" {
		return 0, "", &ConfigError{Message: "请先配置有效的 Telegram API_ID 和 API_HASH"}
	}
	if prompt == nil {
		return 0, "", errors.New("缺少前台登录输入接口")
	}
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &telegramSession{path: path},
		NoUpdates:      true,
	})
	err = client.Run(ctx, func(ctx context.Context) error {
		// IfNecessary verifies an existing candidate session before asking for
		// credentials, allowing interrupted setup to resume without a new login.
		if err := client.Auth().IfNecessary(ctx, auth.NewFlow(telegramLoginPrompt{prompt: prompt}, auth.SendCodeOptions{})); err != nil {
			return fmt.Errorf("Telegram 登录失败: %w", err)
		}
		me, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("确认 Telegram 登录身份: %w", err)
		}
		if me.Bot {
			return &ConfigError{Message: "监听必须使用个人账号，不能使用 Bot Session"}
		}
		ownerID, name = me.ID, telegramUserName(me)
		return nil
	})
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return ownerID, name, err
}

type telegramPeerInfo struct {
	title    string
	username string
}

type TGClient struct {
	api          *tg.Client
	mu           sync.RWMutex
	peers        map[int64]telegramPeerInfo
	queueWarning atomic.Int64
}

func markedPeerID(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return p.UserID
	case *tg.PeerChat:
		return -p.ChatID
	case *tg.PeerChannel:
		return -(channelIDOffset + p.ChannelID)
	default:
		return 0
	}
}

func telegramUserName(user *tg.User) string {
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name == "" {
		name = user.Username
	}
	if name == "" {
		name = strconv.FormatInt(user.ID, 10)
	}
	return name
}

func (c *TGClient) remember(e tg.Entities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, user := range e.Users {
		c.peers[id] = telegramPeerInfo{title: telegramUserName(user), username: user.Username}
	}
	for id, chat := range e.Chats {
		c.peers[-id] = telegramPeerInfo{title: chat.Title}
	}
	for id, channel := range e.Channels {
		key := -(channelIDOffset + id)
		old := c.peers[key]
		// Min entities may omit fields; retain known metadata in that case.
		if channel.Title != "" {
			old.title = channel.Title
		}
		if !channel.Min || channel.Username != "" {
			old.username = channel.Username
		}
		c.peers[key] = old
	}
}

func (c *TGClient) peerInfo(id int64) telegramPeerInfo {
	c.mu.RLock()
	info := c.peers[id]
	c.mu.RUnlock()
	if info.title == "" {
		info.title = strconv.FormatInt(id, 10)
	}
	return info
}

func (c *TGClient) Dialogs(ctx context.Context) ([]Chat, error) {
	iter := query.NewQuery(c.api).Dialogs().GetDialogs().BatchSize(100).Iter()
	chats := make(map[int64]Chat)
	for iter.Next(ctx) {
		elem := iter.Value()
		c.remember(tg.Entities{Users: elem.Entities.Users(), Chats: elem.Entities.Chats(), Channels: elem.Entities.Channels()})
		switch p := elem.Peer.(type) {
		case *tg.InputPeerChat:
			chat := elem.Entities.Chats()[p.ChatID]
			if chat == nil || chat.Left || chat.Deactivated || chat.MigratedTo != nil {
				continue
			}
			id := -p.ChatID
			chats[id] = Chat{ID: id, Title: chat.Title}
		case *tg.InputPeerChannel:
			channel := elem.Entities.Channels()[p.ChannelID]
			if channel == nil || channel.Left {
				continue
			}
			id := -(channelIDOffset + p.ChannelID)
			chats[id] = Chat{ID: id, Title: channel.Title}
		}
	}
	if err := iter.Err(); err != nil {
		return nil, telegramRuntimeError(err)
	}
	result := make([]Chat, 0, len(chats))
	for _, chat := range chats {
		result = append(result, chat)
	}
	sort.Slice(result, func(i, j int) bool {
		if fold(result[i].Title) == fold(result[j].Title) {
			return result[i].ID < result[j].ID
		}
		return fold(result[i].Title) < fold(result[j].Title)
	})
	return result, nil
}

func (c *TGClient) ResolveSender(ctx context.Context, value string) (Sender, error) {
	value = strings.TrimSpace(value)
	if id, err := strconv.ParseInt(value, 10, 64); err == nil {
		if id <= 0 && id >= -channelIDOffset {
			return Sender{}, errors.New("发送者 ID 必须是个人正数 ID 或频道的 -100… 格式 ID")
		}
		return Sender{ID: id, Label: c.peerInfo(id).title}, nil
	}
	value = strings.TrimPrefix(value, "https://t.me/")
	value = strings.TrimPrefix(value, "http://t.me/")
	value = strings.TrimPrefix(value, "@")
	if value == "" || strings.ContainsAny(value, "/? &#\t\r\n") {
		return Sender{}, errors.New("请输入 @用户名、t.me/用户名或数字 ID")
	}
	resolved, err := c.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: value})
	if err != nil {
		return Sender{}, telegramRuntimeError(err)
	}
	c.remember(tg.Entities{Users: resolved.MapUsers().UserToMap(), Chats: resolved.MapChats().ChatToMap(), Channels: resolved.MapChats().ChannelToMap()})
	if _, ok := resolved.Peer.(*tg.PeerUser); !ok {
		if _, ok := resolved.Peer.(*tg.PeerChannel); !ok {
			return Sender{}, errors.New("用户名必须属于个人账号或频道")
		}
	}
	id := markedPeerID(resolved.Peer)
	return Sender{ID: id, Label: c.peerInfo(id).title}, nil
}

func telegramMatch(group WatchGroup, text string, senderID int64) []string {
	if !group.Enabled {
		return nil
	}
	text = fold(text)
	var reasons []string
	for _, set := range groupedRules(group.Rules) {
		for _, rule := range set.rules {
			if !rule.Enabled || rule.SetPaused || rule.Keyword == "" && rule.Sender.ID == 0 {
				continue
			}
			if rule.Keyword != "" && !strings.Contains(text, fold(rule.Keyword)) {
				continue
			}
			if rule.Sender.ID != 0 && rule.Sender.ID != senderID {
				continue
			}
			reasons = append(reasons, rule.description())
		}
	}
	return reasons
}

func formatNoticeReasons(reasons []string) string {
	text := ""
	for i, reason := range reasons {
		line := "\n• " + reason
		tail := ""
		if i+1 < len(reasons) {
			tail = fmt.Sprintf("\n…另有 %d 项命中", len(reasons)-i-1)
		}
		if truncateText(text+line+tail, 500) != text+line+tail {
			if text == "" {
				// Unusually long stored labels still leave room for the count.
				text = truncateText(line, 450)
				if len(reasons) > 1 {
					text += fmt.Sprintf("\n…另有 %d 项命中", len(reasons)-1)
				}
				return text
			}
			return text + fmt.Sprintf("\n…另有 %d 项命中", len(reasons)-i)
		}
		text += line
	}
	return text
}

func (c *TGClient) enqueueMessage(store *Store, ownerID int64, started int64, entities tg.Entities, message tg.MessageClass) error {
	c.remember(entities)
	m, ok := message.(*tg.Message)
	if !ok || int64(m.Date) < started || m.ID <= 0 {
		return nil
	}
	switch m.PeerID.(type) {
	case *tg.PeerChat, *tg.PeerChannel:
	default:
		return nil
	}
	chatID := markedPeerID(m.PeerID)
	senderID := markedPeerID(m.FromID)
	// Channel posts can omit from_id. Their sender is the channel itself.
	if senderID == 0 && m.Post {
		senderID = chatID
	}
	chat := c.peerInfo(chatID)
	sender := c.peerInfo(senderID).title
	if senderID == 0 {
		sender = "未知发送者"
	}
	var link string
	switch p := m.PeerID.(type) {
	case *tg.PeerChannel:
		if chat.username != "" {
			link = fmt.Sprintf("https://t.me/%s/%d", chat.username, m.ID)
		} else {
			link = fmt.Sprintf("https://t.me/c/%d/%d", p.ChannelID, m.ID)
		}
	case *tg.PeerChat:
		link = fmt.Sprintf("tg://openmessage?chat_id=%d&message_id=%d", p.ChatID, m.ID)
	}
	summary := m.Message
	if summary == "" {
		summary = "（媒体消息，无文字说明）"
	}
	now := time.Now()
	_, err := store.Enqueue(Notice{ChatID: chatID, MessageID: int64(m.ID), OwnerID: ownerID, Link: link, Due: now, Created: now}, func(group WatchGroup) string {
		reasons := telegramMatch(group, m.Message, senderID)
		if len(reasons) == 0 {
			return ""
		}
		title := chat.title
		if title == strconv.FormatInt(chatID, 10) && group.Title != "" {
			title = group.Title
		}
		return fmt.Sprintf("来源：%s (%d)\n发送者：%s (%d)\n\n命中：%s\n\n%s", truncateText(title, 160), chatID, truncateText(sender, 160), senderID, formatNoticeReasons(reasons), truncateText(summary, 2200))
	})
	if errors.Is(err, ErrQueueFull) {
		now := time.Now().Unix()
		previous := c.queueWarning.Load()
		if now-previous >= 60 && c.queueWarning.CompareAndSwap(previous, now) {
			logf("通知队列已满，跳过群组 %d 消息 %d", chatID, m.ID)
		}
		return nil
	}
	return err
}

func telegramRuntimeError(err error) error {
	if err != nil && (auth.IsUnauthorized(err) || tgerr.Is(err, "AUTH_KEY_DUPLICATED", "SESSION_REVOKED", "SESSION_EXPIRED", "USER_DEACTIVATED", "USER_DEACTIVATED_BAN")) {
		return &ConfigError{Message: "Telegram 个人账号 Session 已失效，请运行 tgw，选择「1. 首次配置」重新登录"}
	}
	return err
}

func RunRuntime(ctx context.Context, cfg Config, dataDir string) (result error) {
	logf("正在启动消息监听与通知 Bot")
	defer logf("监听服务已停止")
	started := time.Now().Unix()
	if cfg.APIID <= 0 || cfg.APIHash == "" || cfg.OwnerID <= 0 || cfg.BotToken == "" {
		return &ConfigError{Message: "配置不完整，请运行 tgw，选择「1. 首次配置」"}
	}
	storage := &telegramSession{path: filepath.Join(dataDir, "session.json")}
	data, err := (&session.Loader{Storage: storage}).Load(ctx)
	if err != nil || data == nil || len(data.AuthKey) != 256 || len(data.AuthKeyID) != 8 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ConfigError{Message: "Telegram Session 缺失或损坏，请运行 tgw，选择「1. 首次配置」"}
	}
	store, err := OpenStore(filepath.Join(dataDir, "state.db"))
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.Close()) }()
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tgClient := &TGClient{peers: make(map[int64]telegramPeerInfo)}
	dispatcher := tg.NewUpdateDispatcher()
	var accepting atomic.Bool
	handle := func(ctx context.Context, entities tg.Entities, message tg.MessageClass) error {
		if !accepting.Load() {
			return nil
		}
		if err := tgClient.enqueueMessage(store, cfg.OwnerID, started, entities, message); err != nil {
			cancel(fmt.Errorf("处理 Telegram 消息: %w", err))
			return err
		}
		return nil
	}
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		return handle(ctx, e, u.Message)
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		return handle(ctx, e, u.Message)
	})
	gaps := updates.New(updates.Config{Handler: dispatcher, MaxChannelDifferenceConcurrency: 4})
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
		UpdateHandler:  gaps,
		// The update manager logs some gap-recovery RPC errors internally.
		// Observe authorization failures here so those cannot leave the Bot
		// running while the listening account has silently lost authorization.
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
			return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
				err := next.Invoke(ctx, input, output)
				var configErr *ConfigError
				if converted := telegramRuntimeError(err); errors.As(converted, &configErr) {
					cancel(converted)
				}
				return err
			}
		})},
		OnDead: func(err error) {
			var configErr *ConfigError
			if converted := telegramRuntimeError(err); errors.As(converted, &configErr) {
				cancel(converted)
			}
		},
	})
	tgClient.api = client.API()
	err = client.Run(runCtx, func(clientCtx context.Context) error {
		status, err := client.Auth().Status(clientCtx)
		if err != nil {
			return telegramRuntimeError(err)
		}
		if !status.Authorized || status.User == nil || status.User.Bot {
			return &ConfigError{Message: "Telegram 个人账号尚未授权，请运行 tgw，选择「1. 首次配置」"}
		}
		if status.User.ID != cfg.OwnerID {
			return &ConfigError{Message: "Telegram Session 身份与配置的管理员不一致，请运行 tgw，选择「1. 首次配置」"}
		}
		workerCtx, stopWorkers := context.WithCancel(clientCtx)
		var wg sync.WaitGroup
		defer func() { stopWorkers(); wg.Wait() }()
		errorsCh := make(chan error, 2)
		start := func(worker func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := worker()
				if err == nil && workerCtx.Err() == nil {
					err = errors.New("后台工作进程意外停止")
				}
				errorsCh <- err
			}()
		}
		ready := make(chan struct{})
		start(func() error {
			return gaps.Run(workerCtx, client.API(), status.User.ID, updates.AuthOptions{Forget: true, OnStart: func(context.Context) {
				accepting.Store(true)
				close(ready)
			}})
		})
		select {
		case <-ready:
		case err := <-errorsCh:
			return err
		case <-workerCtx.Done():
			return workerCtx.Err()
		}
		bot := NewBot(cfg.BotToken)
		start(func() error {
			return bot.Run(workerCtx, store, tgClient, cfg.OwnerID, func() error {
				if err := notifyReady(filepath.Dir(dataDir)); err != nil {
					return err
				}
				logf("消息监听与通知 Bot 已就绪")
				return nil
			})
		})
		select {
		case err := <-errorsCh:
			return err
		case <-workerCtx.Done():
			return workerCtx.Err()
		}
	})
	if cause := context.Cause(runCtx); cause != nil && ctx.Err() == nil {
		return telegramRuntimeError(cause)
	}
	return telegramRuntimeError(err)
}
