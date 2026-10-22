package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func testGroup(t *testing.T, store *Store, chat int64) WatchGroup {
	t.Helper()
	group, err := store.Group(chat)
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func addTestRules(t *testing.T, store *Store, chat int64, rules ...Rule) WatchGroup {
	t.Helper()
	if err := store.SetChat(context.Background(), chat, "测试群组"); err != nil {
		t.Fatal(err)
	}
	group := testGroup(t, store, chat)
	if _, _, err := store.AddRules(context.Background(), chat, rules, group.Revision); err != nil {
		t.Fatal(err)
	}
	return testGroup(t, store, chat)
}

func TestGroupedListeners(t *testing.T) {
	store, _ := testStore(t)
	wang, li := Sender{ID: 11, Label: "@wang"}, Sender{ID: 22, Label: "@li"}
	group := addTestRules(t, store, -123,
		Rule{Sender: li, Keyword: "优惠"}, Rule{Sender: wang},
		Rule{Keyword: "补货"}, Rule{Sender: wang, Keyword: "活动"},
		Rule{Keyword: "优惠"}, Rule{Sender: li}, Rule{Sender: li, Keyword: "补货"})
	sets := groupedRules(group.Rules)
	var got []string
	for _, set := range sets {
		got = append(got, set.description())
	}
	want := []string{"消息包含：补货、优惠", "指定发送者：@wang、@li", "@li 的消息包含：优惠、补货", "@wang 的消息包含：活动"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grouping: got %v, want %v", got, want)
	}
	menu := &botMenu{store: store}
	view, err := menu.group(group.ChatID, 0)
	if err != nil || !strings.Contains(view.text, "共 4 项监听") {
		t.Fatalf("group view: %s, %v", view.text, err)
	}
	if strings.Contains(view.text, "消息包含：") || strings.Contains(view.text, "指定发送者：") {
		t.Fatalf("group body repeats listener buttons: %s", view.text)
	}
	for index, description := range want {
		if got := view.rows[index][0].Text; got != fmt.Sprintf("✅ %d. %s", index+1, description) {
			t.Fatalf("listener button %d: %s", index+1, got)
		}
	}
	conditions, err := menu.set(group.ChatID, sets[0].rules[0].ID, 0)
	if err != nil || strings.Contains(conditions.text, "补货") || conditions.rows[0][0].Text != "✅ 1. 补货" {
		t.Fatalf("condition body/button: %+v, %v", conditions, err)
	}
	if err := store.changeSet(context.Background(), group.ChatID, sets[0].rules[0].ID, group.Revision, true, false); err != nil {
		t.Fatal(err)
	}
	view, err = menu.group(group.ChatID, 0)
	if err != nil || !strings.Contains(view.text, "共 3 项监听") || strings.Contains(view.rows[0][0].Text, "消息包含：") {
		t.Fatalf("empty category must disappear: %s, %v", view.text, err)
	}
}

func TestBatchAtomicityAndOrder(t *testing.T) {
	store, _ := testStore(t)
	values, err := splitBatchInput(" First，第二;first；第三、第二\n第四\r\n ", ruleBatchLimit)
	if err != nil {
		t.Fatal(err)
	}
	var rules []Rule
	for _, value := range values {
		rules = append(rules, Rule{Keyword: value})
	}
	group := addTestRules(t, store, -123, rules...)
	if got := groupedRules(group.Rules)[0].values(); !reflect.DeepEqual(got, []string{"First", "第二", "第三", "第四"}) {
		t.Fatalf("order/dedup: %v", got)
	}
	added, skipped, err := store.AddRules(context.Background(), -123, []Rule{{Keyword: "FIRST"}}, group.Revision)
	if err != nil || added != 0 || skipped != 1 || testGroup(t, store, -123).Revision != group.Revision {
		t.Fatalf("duplicate changed state: added=%d skipped=%d err=%v", added, skipped, err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_bad BEFORE INSERT ON rules WHEN NEW.keyword='bad' BEGIN SELECT RAISE(ABORT,'rejected'); END`); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.AddRules(context.Background(), -123, []Rule{{Keyword: "before"}, {Keyword: "bad"}}, group.Revision)
	current := testGroup(t, store, -123)
	if err == nil || len(current.Rules) != 4 || current.Revision != group.Revision {
		t.Fatalf("partial write/revision after failed batch: %+v, %v", current, err)
	}
	other := addTestRules(t, store, -456, Rule{Keyword: "First"})
	if len(other.Rules) != 1 {
		t.Fatal("groups must not share duplicate filters")
	}
}

func TestSetPauseKeepsIndividualState(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "a"}, Rule{Keyword: "b"})
	id := group.Rules[0].ID
	if err := store.SetRuleEnabled(context.Background(), -123, id, false, group.Revision); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	if err := store.changeSet(context.Background(), -123, id, group.Revision, false, true); err != nil {
		t.Fatal(err)
	}
	group = addTestRules(t, store, -123, Rule{Keyword: "c"})
	if len(telegramMatch(group, "abc", 11)) != 0 {
		t.Fatal("new member reactivated a paused set")
	}
	if err := store.changeSet(context.Background(), -123, id, group.Revision, false, false); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	if group.Rules[0].Enabled || len(telegramMatch(group, "abc", 11)) != 2 {
		t.Fatal("set resume overwrote the individual pause")
	}
	// Moving a condition out of a paused combined entry must preserve its pause.
	group = addTestRules(t, store, -123, Rule{Keyword: "x", Sender: Sender{ID: 11}})
	rule := group.Rules[len(group.Rules)-1]
	if err := store.changeSet(context.Background(), -123, rule.ID, group.Revision, false, true); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	rule.Sender.ID = 22
	if err := store.UpdateRule(context.Background(), -123, rule, group.Revision); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	if group.Rules[len(group.Rules)-1].Enabled {
		t.Fatal("moving a paused condition activated it")
	}
}

func TestConcurrentRuleEdits(t *testing.T) {
	store, path := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "first"})
	second, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, current := range []*Store{store, second} {
		go func() {
			<-start
			_, _, err := current.AddRules(context.Background(), -123, []Rule{{Keyword: fmt.Sprint(i)}}, group.Revision)
			results <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	if !(a == nil && errors.Is(b, ErrRuleChanged) || b == nil && errors.Is(a, ErrRuleChanged)) {
		t.Fatalf("expected one winner and one stale edit, got %v / %v", a, b)
	}
	if len(testGroup(t, store, -123).Rules) != 2 {
		t.Fatal("lost edit or duplicate revision accepted")
	}
}

func TestConcurrentMultiMatchDeduplication(t *testing.T) {
	store, path := testStore(t)
	chat := -(channelIDOffset + 123)
	addTestRules(t, store, chat, Rule{Keyword: "Sale"}, Rule{Sender: Sender{ID: 11, Label: "@wang"}}, Rule{Keyword: "sale", Sender: Sender{ID: 11, Label: "@wang"}})
	second, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	client := &TGClient{peers: map[int64]telegramPeerInfo{}}
	message := &tg.Message{ID: 7, Date: int(time.Now().Unix()), PeerID: &tg.PeerChannel{ChannelID: 123}, FromID: &tg.PeerUser{UserID: 11}, Message: "SALE now"}
	var wg sync.WaitGroup
	results := make(chan error, 24)
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			current := []*Store{store, second}[i%2]
			results <- client.enqueueMessage(current, 99, 0, tg.Entities{}, message)
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.db.QueryRow("SELECT count(*) FROM notices").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate enqueue: count=%d err=%v", count, err)
	}
	notice, err := store.NextNotice(time.Now())
	if err != nil || notice == nil {
		t.Fatalf("missing notice: %v", err)
	}
	for _, reason := range []string{"消息包含「Sale」", "@wang 的全部消息", "@wang 发出的含「sale」的消息"} {
		if !strings.Contains(notice.Body, reason) {
			t.Errorf("missing match reason %q: %s", reason, notice.Body)
		}
	}
	if err := store.Sent(chat, 7); err != nil {
		t.Fatal(err)
	}
	if err := client.enqueueMessage(store, 99, 0, tg.Entities{}, message); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := store.db.QueryRow("SELECT body FROM notices").Scan(&body); err != nil || body != "" {
		t.Fatalf("sent body retained or re-enqueued: %q %v", body, err)
	}
	addTestRules(t, store, -456, Rule{Keyword: "sale"})
	other := *message
	other.PeerID = &tg.PeerChat{ChatID: 456}
	if err := client.enqueueMessage(store, 99, 0, tg.Entities{}, &other); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT count(*) FROM notices").Scan(&count); err != nil || count != 2 {
		t.Fatalf("same message ID in different groups suppressed: %d %v", count, err)
	}
}

func TestInlineButtonKeywordMatching(t *testing.T) {
	buttons := &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
		{Buttons: []tg.KeyboardInlineButton{{Text: "一键参与抽奖", Type: &tg.InlineButtonTypeCallback{Data: []byte("callback-only")}}}},
		{},
		{Buttons: []tg.KeyboardInlineButton{{Text: "SALE", Type: &tg.InlineButtonTypeURL{URL: "https://example.com/url-only"}}, {Text: ""}}},
	}}
	sender := Sender{ID: 11, Label: "小王"}
	cases := []struct {
		name    string
		text    string
		markup  tg.ReplyMarkupClass
		rules   []Rule
		reasons []string
		summary string
	}{
		{"media_buttons", "", buttons,
			[]Rule{{Keyword: "抽奖"}, {Keyword: "sale"}, {Sender: sender, Keyword: "抽奖"}, {Sender: Sender{ID: 22}, Keyword: "抽奖"}, {Keyword: "callback-only"}, {Keyword: "url-only"}},
			[]string{"消息包含「抽奖」", "消息包含「sale」", "小王 发出的含「抽奖」的消息"}, "一键参与抽奖\nSALE"},
		{"caption_and_buttons", "补货", buttons,
			[]Rule{{Keyword: "补货"}, {Sender: sender, Keyword: "SALE"}},
			[]string{"消息包含「补货」", "小王 发出的含「SALE」的消息"}, "补货\n一键参与抽奖\nSALE"},
		{"wrong_sender", "", buttons, []Rule{{Sender: Sender{ID: 22}, Keyword: "抽奖"}}, nil, ""},
		{"hidden_button_data", "", buttons, []Rule{{Keyword: "callback-only"}, {Keyword: "url-only"}}, nil, ""},
		{"separate_fields", "抽", &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{{Buttons: []tg.KeyboardInlineButton{{Text: "奖"}}}}},
			[]Rule{{Keyword: "抽奖"}}, nil, ""},
		{"plain_message", "SALE", nil, []Rule{{Keyword: "sale"}}, []string{"消息包含「sale」"}, "SALE"},
		{"media_without_text", "", nil, []Rule{{Sender: sender}}, []string{"小王 的全部消息"}, "（媒体消息，无文字说明）"},
		{"empty_buttons", "", &tg.ReplyInlineMarkup{}, []Rule{{Keyword: "抽奖"}}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := testStore(t)
			chat := -(channelIDOffset + 123)
			addTestRules(t, store, chat, tc.rules...)
			client := &TGClient{peers: map[int64]telegramPeerInfo{}}
			message := &tg.Message{ID: 7, Date: int(time.Now().Unix()), PeerID: &tg.PeerChannel{ChannelID: 123}, FromID: &tg.PeerUser{UserID: 11}, Message: tc.text, ReplyMarkup: tc.markup}
			for range 2 {
				if err := client.enqueueMessage(store, 99, 0, tg.Entities{}, message); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := store.db.QueryRow("SELECT count(*) FROM notices").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if len(tc.reasons) == 0 {
				if count != 0 {
					t.Fatalf("unexpected notice count: %d", count)
				}
				return
			}
			if count != 1 {
				t.Fatalf("expected one notice after repeated delivery, got %d", count)
			}
			notice, err := store.NextNotice(time.Now())
			if err != nil || notice == nil {
				t.Fatalf("missing notice: %v", err)
			}
			want := "\n\n命中：" + formatNoticeReasons(tc.reasons) + "\n\n" + tc.summary
			if !strings.HasSuffix(notice.Body, want) {
				t.Fatalf("notice body %q does not end with %q", notice.Body, want)
			}
		})
	}
}

func TestEnqueueUsesCurrentRulesAndPause(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "sale"})
	if err := store.DeleteRule(context.Background(), -123, group.Rules[0].ID, group.Revision); err != nil {
		t.Fatal(err)
	}
	match := func(current WatchGroup) string { return strings.Join(telegramMatch(current, "SALE", 11), "、") }
	if inserted, err := store.Enqueue(Notice{ChatID: -123, MessageID: 1, OwnerID: 99}, match); err != nil || inserted {
		t.Fatalf("enqueue used stale rules: %t %v", inserted, err)
	}
	addTestRules(t, store, -123, Rule{Keyword: "sale"})
	if err := store.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if inserted, err := store.Enqueue(Notice{ChatID: -123, MessageID: 2, OwnerID: 99}, match); err != nil || inserted {
		t.Fatalf("enqueue ignored pause: %t %v", inserted, err)
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type menuTelegram struct{ joined bool }

func (f menuTelegram) Dialogs(context.Context) ([]Chat, error) {
	if !f.joined {
		return nil, nil
	}
	return []Chat{{ID: -123, Title: "测试群组"}}, nil
}
func (menuTelegram) ResolveSender(_ context.Context, username string) (Sender, error) {
	if username == "missing" {
		return Sender{}, errors.New("unknown sender")
	}
	return Sender{ID: 11, Label: "@wang"}, nil
}

func testMenu(t *testing.T, store *Store) *botMenu {
	t.Helper()
	bot := NewBot("test-token")
	var nextMessage atomic.Int64
	bot.client.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if text, ok := payload["text"].(string); ok && len(utf16.Encode([]rune(text))) > 4096 {
			return nil, errors.New("message too long")
		}
		if markup, ok := payload["reply_markup"].(map[string]any); ok && markup["force_reply"] == true && markup["selective"] == true {
			return nil, errors.New("private ForceReply must not require an absent mention/reply target")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, nextMessage.Add(1))))}, nil
	})
	return &botMenu{store: store, bot: bot, tg: menuTelegram{joined: true}, owner: 99}
}

func testCallback(menu *botMenu, ctx context.Context, data string) (menuView, error) {
	return menu.callback(ctx, menu.callbackData(data))
}

func TestBatchMenuConfirmationAndCancellation(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "existing"})
	menu := testMenu(t, store)
	ctx := context.Background()
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":k"); err != nil {
		t.Fatal(err)
	}
	if err := menu.addInput(ctx, "补货，活动，补货，EXISTING"); err != nil {
		t.Fatal(err)
	}
	if len(testGroup(t, store, -123).Rules) != 1 || !menu.input.confirming {
		t.Fatal("batch saved before confirmation")
	}
	preview := confirmInput(group, menu.input, 0)
	if !strings.Contains(preview.text, "消息包含：补货、活动") || menu.input.skipped != 2 {
		t.Fatalf("not a merged preview: %s", preview.text)
	}
	save := "save:" + groupKey(group) + ":" + menu.input.token
	if _, err := testCallback(menu, ctx, save); err != nil {
		t.Fatal(err)
	}
	if _, err := testCallback(menu, ctx, save); !errors.Is(err, errMenuExpired) {
		t.Fatalf("old confirmation accepted: %v", err)
	}
	group = testGroup(t, store, -123)
	if got := groupedRules(group.Rules)[0].values(); !reflect.DeepEqual(got, []string{"existing", "补货", "活动"}) {
		t.Fatalf("saved batch: %v", got)
	}
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":b"); err != nil {
		t.Fatal(err)
	}
	if err := menu.addInput(ctx, "11, 22"); err != nil || !menu.input.confirming || len(menu.input.rules) != 1 || menu.input.rules[0].Sender.ID != 11 || menu.input.rules[0].Keyword != "22" {
		t.Fatalf("inline sender and keyword batch: %v", err)
	}
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":b"); err != nil {
		t.Fatal(err)
	}
	if err := menu.addInput(ctx, "@wang"); err != nil || menu.input.field != "k" {
		t.Fatalf("sender step: %v", err)
	}
	if err := menu.addInput(ctx, "优惠;开奖"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(confirmInput(group, menu.input, 0).text, "@wang 的消息包含：优惠、开奖") {
		t.Fatal("combined preview not grouped by sender")
	}
	if _, err := testCallback(menu, ctx, "home"); err != nil || menu.input != nil {
		t.Fatalf("navigation failed to cancel draft: %v", err)
	}
	if len(testGroup(t, store, -123).Rules) != 3 {
		t.Fatal("cancelled draft saved")
	}
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":s"); err != nil {
		t.Fatal(err)
	}
	if err := menu.addInput(ctx, "@wang, @missing"); err != nil || menu.input.confirming || len(testGroup(t, store, -123).Rules) != 3 {
		t.Fatalf("partially accepted invalid sender batch: %v", err)
	}
	menu.tg = menuTelegram{joined: false}
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":k"); err == nil {
		t.Fatal("added listener for inaccessible group")
	}
}

func TestLongBatchPreviewAndCallbackLimits(t *testing.T) {
	group := WatchGroup{ChatID: -9223372036854775807, Revision: 9223372036854775807, Enabled: true, Title: strings.Repeat("群组", 120)}
	input := &menuInput{kind: "k", token: "abcdefghijkl", confirming: true}
	for i := range 30 {
		input.rules = append(input.rules, Rule{Keyword: fmt.Sprintf("%02d", i) + strings.Repeat("😀", 78)})
	}
	var combined strings.Builder
	for page := 0; ; page++ {
		view := confirmInput(group, input, page)
		if len(utf16.Encode([]rune(view.text))) > 4096 {
			t.Fatal("preview exceeds Telegram text limit")
		}
		combined.WriteString(view.text)
		next := false
		for _, row := range view.rows {
			for _, button := range row {
				if len(button.Data) > 64 {
					t.Fatalf("callback exceeds Telegram limit: %s", button.Data)
				}
				next = next || button.Text == "下一页"
			}
		}
		if !next {
			break
		}
	}
	for _, rule := range input.rules {
		if !strings.Contains(combined.String(), rule.Keyword) {
			t.Fatal("long preview lost a keyword")
		}
	}
}

func TestSetMenuActions(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "first"}, Rule{Sender: Sender{ID: 11, Label: "@wang"}}, Rule{Keyword: "sale", Sender: Sender{ID: 22, Label: "@li"}})
	menu := testMenu(t, store)
	ctx := context.Background()
	set := groupedRules(group.Rules)[0]
	view, err := testCallback(menu, ctx, "set:"+setKey(group, set)+":0")
	if err != nil || strings.Contains(view.text, "first") || view.rows[0][0].Text != "✅ 1. first" {
		t.Fatalf("open set: %s %v", view.text, err)
	}
	toggle := "set-toggle:" + setKey(group, set)
	if _, err := testCallback(menu, ctx, toggle); err != nil {
		t.Fatal(err)
	}
	if _, err := testCallback(menu, ctx, toggle); !errors.Is(err, errMenuExpired) {
		t.Fatalf("stale set toggle accepted: %v", err)
	}
	group = testGroup(t, store, -123)
	set = groupedRules(group.Rules)[0]
	if _, err := testCallback(menu, ctx, "set-add:"+setKey(group, set)); err != nil {
		t.Fatal(err)
	}
	if err := menu.addInput(ctx, "second, third"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(confirmInput(group, menu.input, 0).text, "该监听项已暂停") {
		t.Fatal("preview omitted existing set pause")
	}
	if _, err := testCallback(menu, ctx, "save:"+groupKey(group)+":"+menu.input.token); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	set = groupedRules(group.Rules)[0]
	if !set.paused || len(set.rules) != 3 {
		t.Fatal("adding from paused entry lost pause or members")
	}
	if _, err := testCallback(menu, ctx, "delete:"+groupKey(group)+":"+strconv.FormatInt(set.rules[0].ID, 36)); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	set = groupedRules(group.Rules)[0]
	if set.rules[0].Keyword != "second" {
		t.Fatal("deleting anchor changed the remaining order")
	}
	if _, err := testCallback(menu, ctx, "set-remove:"+setKey(group, set)); err != nil {
		t.Fatal(err)
	}
	if _, err := testCallback(menu, ctx, "set-delete:"+setKey(group, set)); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	sets := groupedRules(group.Rules)
	if len(sets) != 2 || sets[0].key.kind != "s" {
		t.Fatal("deleted entry still shown, or unrelated entry deleted")
	}
	if _, err := testCallback(menu, ctx, "set-add:"+setKey(group, sets[1])); err != nil {
		t.Fatal(err)
	}
	if menu.input.field != "k" || menu.input.rule.Sender.ID != 22 {
		t.Fatal("adding to combined entry asked for sender again")
	}
	if err := menu.addInput(ctx, "news, sale"); err != nil {
		t.Fatal(err)
	}
	if _, err := testCallback(menu, ctx, "save:"+groupKey(group)+":"+menu.input.token); err != nil {
		t.Fatal(err)
	}
	sets = groupedRules(testGroup(t, store, -123).Rules)
	if len(sets) != 2 || !reflect.DeepEqual(sets[1].values(), []string{"sale", "news"}) {
		t.Fatal("combined keywords were not appended to the same sender")
	}
}

func TestCoverageWarnings(t *testing.T) {
	sender := Sender{ID: 11, Label: "@wang"}
	word := Rule{ID: 1, Enabled: true, Keyword: "货"}
	person := Rule{ID: 2, Enabled: true, Sender: sender}
	combined := Rule{ID: 3, Enabled: true, Keyword: "补货", Sender: sender}
	for _, tc := range []struct {
		name     string
		old, add Rule
		want     string
	}{
		{"keyword covers combined", word, combined, "不会收窄原范围"},
		{"sender covers combined", person, combined, "不会收窄原范围"},
		{"new keyword covers combined", combined, word, "按较大范围提醒"},
		{"new sender covers combined", combined, person, "按较大范围提醒"},
		{"substring and case", Rule{ID: 4, Enabled: true, Keyword: "SALE"}, Rule{Keyword: "sale today", Sender: sender}, "不会收窄原范围"},
		{"alias uses ID", person, Rule{Sender: Sender{ID: 11, Label: "renamed"}, Keyword: "sale"}, "不会收窄原范围"},
		{"different sender", person, Rule{Sender: Sender{ID: 22}, Keyword: "货"}, ""},
		{"incidental intersection", word, person, ""},
		{"duplicate is not coverage", word, Rule{Keyword: "货"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.add.ID = 0
			group := WatchGroup{Enabled: true, Rules: []Rule{tc.old}}
			got := overlapWarnings(group, []Rule{tc.add}, true)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("warning: %q, want %q", got, tc.want)
			}
		})
	}
	group := WatchGroup{Enabled: true, Rules: []Rule{person}}
	group.Rules[0].Enabled = false
	if got := overlapWarnings(group, []Rule{combined}, true); !strings.Contains(got, "此条件已暂停") {
		t.Fatalf("paused condition omitted: %s", got)
	}
	if got := overlapWarnings(group, []Rule{combined}, false); !strings.Contains(got, "全部监听已暂停") {
		t.Fatalf("global pause omitted: %s", got)
	}
	if got := overlapWarnings(WatchGroup{Enabled: true}, []Rule{word, combined}, true); !strings.Contains(got, "本批其他") {
		t.Fatalf("batch inclusion omitted: %s", got)
	}
	var many []Rule
	for i := range 8 {
		many = append(many, Rule{Keyword: fmt.Sprint(i), Sender: Sender{ID: int64(i + 1)}})
		group.Rules = append(group.Rules, Rule{ID: int64(i + 10), Enabled: true, Sender: Sender{ID: int64(i + 1)}})
	}
	got := overlapWarnings(group, many, true)
	if strings.Count(got, "\n• ") != 3 || !strings.Contains(got, "另有") || len(utf16.Encode([]rune(got))) > 1000 {
		t.Fatalf("unbounded warnings: %s", got)
	}
}

func TestNoticeReasonOrderAndLimit(t *testing.T) {
	group := WatchGroup{Enabled: true, Rules: []Rule{
		{Enabled: true, Sender: Sender{ID: 11}, Keyword: "Sale"},
		{Enabled: true, Sender: Sender{ID: 11}},
		{Enabled: true, Keyword: "SALE"},
	}}
	reasons := telegramMatch(group, "sale", 11)
	if len(reasons) != 3 || reasons[0] != "消息包含「SALE」" || reasons[1] != "11 的全部消息" {
		t.Fatalf("notice does not follow panel order: %v", reasons)
	}
	formatted := formatNoticeReasons(reasons)
	if strings.Count(formatted, "\n• ") != 3 {
		t.Fatalf("reasons are not separate lines: %s", formatted)
	}
	formatted = formatNoticeReasons([]string{strings.Repeat("😀", 100), strings.Repeat("甲", 100), strings.Repeat("乙", 250), "last"})
	if len(utf16.Encode([]rune(formatted))) > 500 || !strings.Contains(formatted, "另有") {
		t.Fatalf("reason limit/count: %s", formatted)
	}
}

type telegramFunctions struct {
	dialogs func(context.Context) ([]Chat, error)
	resolve func(context.Context, string) (Sender, error)
}

func (f telegramFunctions) Dialogs(ctx context.Context) ([]Chat, error) { return f.dialogs(ctx) }
func (f telegramFunctions) ResolveSender(ctx context.Context, value string) (Sender, error) {
	if f.resolve != nil {
		return f.resolve(ctx, value)
	}
	return menuTelegram{}.ResolveSender(ctx, value)
}

func replyUpdate(promptID int64, text string) botUpdate {
	message := &botMessage{ID: 100, From: &botUser{ID: 99}, Text: text}
	message.Chat.ID, message.Chat.Type = 99, "private"
	if promptID > 0 {
		message.ReplyTo = &struct {
			ID int64 `json:"message_id"`
		}{ID: promptID}
	}
	return botUpdate{Message: message}
}

func callbackUpdate(data string) botUpdate {
	message := &botMessage{ID: 90}
	message.Chat.ID, message.Chat.Type = 99, "private"
	return botUpdate{Callback: &botCallback{ID: "query", From: botUser{ID: 99}, Message: message, Data: data}}
}

func TestPromptOwnershipAndRestart(t *testing.T) {
	store, _ := testStore(t)
	for _, chat := range []int64{-123, -456} {
		if err := store.SetChat(context.Background(), chat, "测试群组"); err != nil {
			t.Fatal(err)
		}
	}
	menu := testMenu(t, store)
	menu.tg = telegramFunctions{dialogs: func(context.Context) ([]Chat, error) {
		return []Chat{{ID: -123, Title: "测试群组"}, {ID: -456, Title: "测试群组"}}, nil
	}}
	ctx := context.Background()
	a, b := testGroup(t, store, -123), testGroup(t, store, -456)
	if _, err := testCallback(menu, ctx, "new:"+groupKey(a)+":k"); err != nil {
		t.Fatal(err)
	}
	oldPrompt := menu.input.promptID
	if _, err := testCallback(menu, ctx, "new:"+groupKey(b)+":k"); err != nil {
		t.Fatal(err)
	}
	current := menu.input
	for _, update := range []botUpdate{replyUpdate(oldPrompt, "wrong group"), replyUpdate(0, "unbound")} {
		if err := menu.handle(ctx, update); err != nil {
			t.Fatal(err)
		}
		if menu.input != current || menu.input.confirming {
			t.Fatal("old/plain text changed current draft")
		}
	}
	if err := menu.handle(ctx, replyUpdate(current.promptID, "sale, news")); err != nil {
		t.Fatal(err)
	}
	if !menu.input.confirming || menu.input.chat != -456 {
		t.Fatal("valid reply not bound to selected group")
	}
	oldSave := menu.callbackData("save:" + groupKey(b) + ":" + menu.input.token)
	restarted := testMenu(t, store)
	restarted.bot, restarted.tg = menu.bot, menu.tg // Telegram message IDs survive process restarts.
	if _, err := testCallback(restarted, ctx, "new:"+groupKey(a)+":k"); err != nil {
		t.Fatal(err)
	}
	newDraft := restarted.input
	if _, err := restarted.callback(ctx, oldSave); !errors.Is(err, errMenuExpired) {
		t.Fatalf("old process button accepted: %v", err)
	}
	if err := restarted.handle(ctx, replyUpdate(current.promptID, "old input")); err != nil {
		t.Fatal(err)
	}
	if restarted.input != newDraft || len(testGroup(t, store, -456).Rules) != 0 {
		t.Fatal("old process input affected new session")
	}
	newDraft.expires = time.Now().Add(-time.Second)
	if err := restarted.handle(ctx, replyUpdate(newDraft.promptID, "expired")); err != nil {
		t.Fatal(err)
	}
	if restarted.input != nil || len(testGroup(t, store, -123).Rules) != 0 {
		t.Fatal("expired input saved")
	}
}

func TestRefreshFailureAndStaleClicksPreserveDraft(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "old"})
	menu := testMenu(t, store)
	ctx := context.Background()
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":k"); err != nil {
		t.Fatal(err)
	}
	pending := menu.input
	for _, data := range []string{"unknown", "save:" + groupKey(group) + ":wrong", "group:" + strconv.FormatInt(group.ChatID, 36) + ":zzz:0"} {
		if _, err := testCallback(menu, ctx, data); err == nil {
			t.Fatal("invalid callback accepted")
		}
		if menu.input != pending {
			t.Fatal("invalid button discarded current input")
		}
	}
	oldDialogs := append([]Chat(nil), menu.dialogs...)
	menu.tg = telegramFunctions{dialogs: func(context.Context) ([]Chat, error) {
		return []Chat{{ID: -999, Title: "partial"}}, errors.New("refresh failed")
	}}
	if _, err := testCallback(menu, ctx, "refresh"); err == nil {
		t.Fatal("expected refresh failure")
	}
	if menu.input != pending || !reflect.DeepEqual(menu.dialogs, oldDialogs) {
		t.Fatal("failed refresh changed snapshot/draft")
	}
	menu.tg = telegramFunctions{dialogs: func(ctx context.Context) ([]Chat, error) { <-ctx.Done(); return nil, ctx.Err() }}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := testCallback(menu, short, "refresh"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout not propagated: %v", err)
	}
	if menu.input != pending {
		t.Fatal("timeout discarded draft")
	}
	menu.tg = telegramFunctions{dialogs: func(context.Context) ([]Chat, error) { return []Chat{{ID: -456, Title: "new group"}}, nil }}
	if _, err := testCallback(menu, ctx, "refresh"); err != nil {
		t.Fatal(err)
	}
	if menu.input != nil || menu.dialogs[0].ID != -456 {
		t.Fatal("successful navigation did not switch snapshot")
	}
	if _, err := testCallback(menu, ctx, "group:"+groupKey(group)+":0"); err == nil {
		t.Fatal("removed group opened through old button")
	}
}

func TestSaveResponseFailureDoesNotResubmit(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Sender: Sender{ID: 11, Label: "@wang"}})
	menu := testMenu(t, store)
	ctx := context.Background()
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":b"); err != nil {
		t.Fatal(err)
	}
	if err := menu.handle(ctx, replyUpdate(menu.input.promptID, "11")); err != nil {
		t.Fatal(err)
	}
	if err := menu.handle(ctx, replyUpdate(menu.input.promptID, "sale")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(confirmInput(group, menu.input, 0).text, "不会收窄原范围") {
		t.Fatal("missing non-blocking warning")
	}
	save := menu.callbackData("save:" + groupKey(group) + ":" + menu.input.token)
	original := menu.bot.client.Transport
	var feedback []string
	menu.bot.client.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/editMessageText") {
			return nil, errors.New("reply lost")
		}
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		var payload map[string]any
		_ = json.Unmarshal(data, &payload)
		if text, ok := payload["text"].(string); ok {
			feedback = append(feedback, text)
		}
		return original.RoundTrip(req)
	})
	if err := menu.handle(ctx, callbackUpdate(save)); err != nil {
		t.Fatal(err)
	}
	if menu.input != nil || len(testGroup(t, store, -123).Rules) != 2 {
		t.Fatal("save failed or draft reactivated")
	}
	if !strings.Contains(strings.Join(feedback, "\n"), "操作已保存") {
		t.Fatalf("unknown committed result: %v", feedback)
	}
	if err := menu.handle(ctx, callbackUpdate(save)); err != nil {
		t.Fatal(err)
	}
	if len(testGroup(t, store, -123).Rules) != 2 {
		t.Fatal("old confirmation duplicated write")
	}
}

func TestPromptFailureAndStepBinding(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "old"})
	menu := testMenu(t, store)
	ctx := context.Background()
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":k"); err != nil {
		t.Fatal(err)
	}
	old := menu.input
	original := menu.bot.client.Transport
	menu.bot.client.Transport = testTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("prompt failed") })
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":s"); err == nil {
		t.Fatal("expected prompt failure")
	}
	if menu.input != old {
		t.Fatal("failed prompt activated a hidden step")
	}
	menu.bot.client.Transport = original
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":b"); err != nil {
		t.Fatal(err)
	}
	senderPrompt := menu.input.promptID
	if err := menu.handle(ctx, replyUpdate(senderPrompt, "11")); err != nil {
		t.Fatal(err)
	}
	keywordPrompt := menu.input.promptID
	if keywordPrompt == senderPrompt || menu.input.field != "k" {
		t.Fatal("step prompt not changed")
	}
	if err := menu.handle(ctx, replyUpdate(senderPrompt, "bad")); err != nil {
		t.Fatal(err)
	}
	if menu.input.confirming {
		t.Fatal("old step accepted as keyword")
	}
	if err := menu.handle(ctx, replyUpdate(keywordPrompt, strings.Repeat("字", 81))); err != nil {
		t.Fatal(err)
	}
	if menu.input.promptID == keywordPrompt || menu.input.field != "k" {
		t.Fatal("validation failure did not renew bound prompt")
	}
}

func TestCanceledStoreWriteDoesNotLeakLock(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "first"})
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, _, err = store.AddRules(ctx, -123, []Rule{{Keyword: "second"}}, group.Revision)
	cancel()
	_ = tx.Rollback()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation lost: %v", err)
	}
	current := testGroup(t, store, -123)
	if current.Revision != group.Revision || len(current.Rules) != 1 {
		t.Fatal("cancelled write changed data")
	}
	if _, _, err := store.AddRules(context.Background(), -123, []Rule{{Keyword: "retry"}}, current.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestDialogSnapshotAndFinalMembership(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "existing"})
	menu := testMenu(t, store)
	ctx := context.Background()
	var calls int
	dialogs := []Chat{{ID: -123, Title: "Z original"}, {ID: -456, Title: "A other"}}
	menu.tg = telegramFunctions{dialogs: func(context.Context) ([]Chat, error) {
		calls++
		return append([]Chat(nil), dialogs...), nil
	}}
	if _, err := menu.groups(ctx, 0, true, false); err != nil {
		t.Fatal(err)
	}
	selected := "group:" + groupKey(group) + ":0"
	if _, err := menu.groups(ctx, 1, false, false); err != nil || calls != 1 {
		t.Fatalf("pagination unexpectedly fetched dialogs: %d %v", calls, err)
	}
	dialogs[0].Title = "A renamed"
	dialogs[1].Title = "Z other"
	if _, err := testCallback(menu, ctx, "refresh"); err != nil {
		t.Fatal(err)
	}
	view, err := testCallback(menu, ctx, selected)
	if err != nil || !strings.Contains(view.text, "A renamed") || testGroup(t, store, -456).Revision >= 0 {
		t.Fatalf("selection drifted after reorder/rename: %s %v", view.text, err)
	}
	group = testGroup(t, store, -123)
	if _, err := testCallback(menu, ctx, "new:"+groupKey(group)+":k"); err != nil {
		t.Fatal(err)
	}
	if err := menu.addInput(ctx, "candidate"); err != nil {
		t.Fatal(err)
	}
	pending := menu.input
	dialogs = dialogs[1:]
	if _, err := testCallback(menu, ctx, "save:"+groupKey(group)+":"+pending.token); err == nil {
		t.Fatal("saved after leaving target group")
	}
	if menu.input != pending || len(testGroup(t, store, -123).Rules) != 1 {
		t.Fatal("rejected save changed draft or rules")
	}
	if err := store.SetGroupEnabled(ctx, -123, false, group.Revision); err != nil {
		t.Fatal(err)
	}
	group = testGroup(t, store, -123)
	if _, err := testCallback(menu, ctx, "group-toggle:"+groupKey(group)); err == nil || testGroup(t, store, -123).Enabled {
		t.Fatalf("resumed an inaccessible group: %v", err)
	}
}

func TestBatchQueriesShareDeadline(t *testing.T) {
	store, _ := testStore(t)
	group := addTestRules(t, store, -123, Rule{Keyword: "existing"})
	menu := testMenu(t, store)
	if _, err := testCallback(menu, context.Background(), "new:"+groupKey(group)+":s"); err != nil {
		t.Fatal(err)
	}
	pending := menu.input
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var calls int
	menu.tg = telegramFunctions{resolve: func(query context.Context, value string) (Sender, error) {
		calls++
		got, ok := query.Deadline()
		if !ok || !got.Equal(deadline) {
			t.Fatal("lookup received a renewed deadline")
		}
		if calls == 1 {
			return Sender{ID: 11, Label: value}, nil
		}
		<-query.Done()
		return Sender{}, query.Err()
	}}
	if err := menu.addInput(ctx, "@first, @second"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query cancellation not propagated: %v", err)
	}
	if calls != 2 || menu.input != pending || pending.confirming || len(testGroup(t, store, -123).Rules) != 1 {
		t.Fatal("timed out batch was partially accepted")
	}
}

func TestBotShutdownDrainsWorkers(t *testing.T) {
	for _, readyFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(readyFailure), func(t *testing.T) {
			store, _ := testStore(t)
			bot := NewBot("test-token")
			var active, sends atomic.Int64
			var updates int
			polling := make(chan struct{}, 1)
			bot.client.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
				active.Add(1)
				defer active.Add(-1)
				body := `{"ok":true,"result":true}`
				switch {
				case strings.HasSuffix(req.URL.Path, "/getMe"):
					body = `{"ok":true,"result":{"id":1,"is_bot":true}}`
				case strings.HasSuffix(req.URL.Path, "/getUpdates"):
					var payload struct {
						Offset int64 `json:"offset"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						return nil, err
					}
					updates++
					if updates == 1 {
						if payload.Offset != -1 {
							return nil, errors.New("old updates were not flushed")
						}
						body = `{"ok":true,"result":[{"update_id":99,"message":{"message_id":1,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"/start"}}]}`
					} else if updates == 2 {
						if payload.Offset != 100 {
							return nil, errors.New("old update was not acknowledged")
						}
						body = `{"ok":true,"result":[]}`
					} else {
						polling <- struct{}{}
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
				case strings.HasSuffix(req.URL.Path, "/sendMessage"):
					sends.Add(1)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			readyErr := errors.New("ready failed")
			result := make(chan error, 1)
			go func() {
				result <- bot.Run(ctx, store, menuTelegram{joined: true}, 99, func() error {
					if readyFailure {
						return readyErr
					}
					return nil
				})
			}()
			if !readyFailure {
				select {
				case <-polling:
					cancel()
				case <-ctx.Done():
					t.Fatal("polling did not start")
				}
			}
			select {
			case err := <-result:
				if readyFailure && !errors.Is(err, readyErr) || !readyFailure && !errors.Is(err, context.Canceled) {
					t.Fatalf("shutdown result: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("workers did not terminate after cancellation or ready failure")
			}
			if active.Load() != 0 || sends.Load() != 0 {
				t.Fatal("in-flight request leaked or stale update replayed")
			}
		})
	}
}
