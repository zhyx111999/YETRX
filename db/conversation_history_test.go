package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 这些测试对共享库也安全:每个用例用独一无二的标记词隔离自己的会话,并在结束时删除。

type historyFixture struct {
	t    *testing.T
	d    *DB
	mark string // 全小写字母数字,不含 LIKE 通配符,可直接拼进搜索词
}

func newHistoryFixture(t *testing.T) *historyFixture {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return &historyFixture{t: t, d: d, mark: fmt.Sprintf("hx%d", time.Now().UnixNano())}
}

// conv 直接 INSERT 以指定 updated_at(BEFORE UPDATE 触发器会把后续 UPDATE 的 updated_at 改成 now())。
func (f *historyFixture) conv(title string, updatedAt time.Time) int64 {
	f.t.Helper()
	var id int64
	err := f.d.QueryRow(`INSERT INTO conversations(agent_key, title, created_at, updated_at)
VALUES ('mainagent', $1, $2, $2) RETURNING id`, title, updatedAt).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = f.d.DeleteConversation(id) })
	return id
}

func (f *historyFixture) add(conv int64, kind, text string) int64 {
	f.t.Helper()
	tool := ""
	if strings.HasPrefix(kind, "tool_") {
		tool = "Bash"
	}
	id, err := f.d.AppendConvActivity(conv, Activity{Worker: "mainagent", Kind: kind, Tool: tool, Detail: text})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// all 翻完所有页,同时断言游标推进、每页不超过 limit。
func (f *historyFixture) all(q ChatSessionQuery) []ChatSession {
	f.t.Helper()
	var out []ChatSession
	for range 1000 {
		page, err := f.d.ChatSessions(context.Background(), q)
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, page.Sessions...)
		if page.Next == nil {
			return out
		}
		q.After = page.Next
	}
	f.t.Fatal("pagination did not terminate")
	return nil
}

func ids(sessions []ChatSession) []int64 {
	out := make([]int64, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}

func TestChatSessionsStableOrderAndKeysetPagination(t *testing.T) {
	f := newHistoryFixture(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	// 三个同一时刻(必须靠 id 打破平局) + 一个更新 + 一个更旧。
	old := f.conv(f.mark+" old", base.Add(-time.Minute))
	tie1 := f.conv(f.mark+" tie1", base)
	tie2 := f.conv(f.mark+" tie2", base)
	tie3 := f.conv(f.mark+" tie3", base)
	newest := f.conv(f.mark+" newest", base.Add(time.Minute))
	want := []int64{newest, tie3, tie2, tie1, old}

	for _, limit := range []int{1, 2, 3, 50} {
		got := ids(f.all(ChatSessionQuery{Text: f.mark, Limit: limit}))
		if !slices.Equal(got, want) {
			t.Fatalf("limit=%d order=%v, want %v", limit, got, want)
		}
	}

	// 全库列表(Text 为空):整体严格按 (updated_at, id) 降序,无重复,且我们的会话相对顺序不变。
	list := f.all(ChatSessionQuery{Limit: 7})
	seen := map[int64]bool{}
	var mine []int64
	for i, s := range list {
		if seen[s.ID] {
			t.Fatalf("duplicate session %d across pages", s.ID)
		}
		seen[s.ID] = true
		if i > 0 {
			p := list[i-1]
			if s.UpdatedAt.After(p.UpdatedAt) || (s.UpdatedAt.Equal(p.UpdatedAt) && s.ID > p.ID) {
				t.Fatalf("not sorted at %d: %v/%d after %v/%d", i, s.UpdatedAt, s.ID, p.UpdatedAt, p.ID)
			}
		}
		if slices.Contains(want, s.ID) {
			mine = append(mine, s.ID)
		}
		if s.Hit != nil {
			t.Fatalf("plain list must not carry hits: %+v", s)
		}
	}
	if !slices.Equal(mine, want) {
		t.Fatalf("list relative order=%v, want %v", mine, want)
	}

	// 翻页途中出现更新的会话:游标之后的结果不重复、不丢。
	q := ChatSessionQuery{Text: f.mark, Limit: 2}
	first, err := f.d.ChatSessions(context.Background(), q)
	if err != nil || first.Next == nil || !slices.Equal(ids(first.Sessions), want[:2]) {
		t.Fatalf("first page=%v next=%v err=%v", ids(first.Sessions), first.Next, err)
	}
	f.conv(f.mark+" arrived-mid-pagination", time.Now())
	q.After = first.Next
	rest := f.all(q)
	if !slices.Equal(ids(rest), want[2:]) {
		t.Fatalf("rest after cursor=%v, want %v", ids(rest), want[2:])
	}
}

func TestChatSessionSearchChineseAndLiteralEscapes(t *testing.T) {
	f := newHistoryFixture(t)
	now := time.Now()
	zh := f.conv("支付接口审计", now)
	f.add(zh, "user", f.mark+" 越权漏洞：请检查支付接口")
	// 每个对照会话都能被「错误地当作通配符/转义」的实现命中,但不能被字面量匹配命中。
	pct := f.conv("pct", now)
	f.add(pct, "user", f.mark+" 100%_完成")
	f.add(f.conv("wide", now), "user", f.mark+" 1000 完成")
	f.add(f.conv("wild", now), "user", f.mark+" aXb")
	under := f.conv("under", now)
	f.add(under, "user", f.mark+" a_b")
	backslash := f.conv("backslash", now)
	f.add(backslash, "user", f.mark+` C:\temp`)
	f.add(f.conv("plain", now), "user", f.mark+" C:temp")

	only := func(query string, want ...int64) {
		t.Helper()
		slices.Sort(want)
		got := ids(f.all(ChatSessionQuery{Text: query}))
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("search %q = %v, want %v", query, got, want)
		}
	}
	only(f.mark+" 越权", zh)                    // 中文正文
	only("支付接口审计", zh)                        // 中文标题
	only(strings.ToUpper(f.mark)+" 越权漏洞", zh) // 大小写不敏感
	only(f.mark+" 100%", pct)                 // % 不是通配符:不能命中 "1000"
	only(f.mark+" 100%_", pct)                // % 与 _ 都按字面量
	only(f.mark+" a_b", under)                // _ 不是单字符通配符:不能命中 "aXb"
	only(f.mark+` C:\t`, backslash)           // \ 不是转义符:不能命中 "C:temp"
	only(f.mark+` C:\`, backslash)            // 以反斜杠结尾不能触发 LIKE 转义错误
	only(f.mark + " 不存在的内容")                  // 无命中
	only("x'; DROP TABLE conversations; --")  // 参数化:注入串只是普通文本
	if _, err := f.d.GetConversation(zh); err != nil {
		t.Fatalf("conversations table damaged: %v", err)
	}

	// 全字符匹配 "%" 只命中真有 % 的会话,而不是所有会话。
	for _, s := range f.all(ChatSessionQuery{Text: "%"}) {
		if s.Hit == nil || !strings.Contains(s.Hit.Snippet, "%") {
			t.Fatalf("literal %% matched a session without %%: %+v", s)
		}
	}

	for _, bad := range []string{strings.Repeat("长", HistoryMaxQueryChars+1), "a\x00b", "a\xffb"} {
		if _, err := f.d.ChatSessions(context.Background(), ChatSessionQuery{Text: bad}); err != ErrHistoryQuery {
			t.Fatalf("query %q: err=%v, want ErrHistoryQuery", bad, err)
		}
	}
	if _, err := f.d.ChatSessions(context.Background(), ChatSessionQuery{Text: strings.Repeat("长", HistoryMaxQueryChars)}); err != nil {
		t.Fatalf("query at the limit rejected: %v", err)
	}
}

func TestChatSessionSearchKindSelectionAndHitChoice(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv(f.mark+" title", time.Now())
	f.add(c, "thinking", f.mark+"-think")
	f.add(c, "tool_use", f.mark+"-tooluse")
	f.add(c, "tool_result", f.mark+"-toolout")
	f.add(c, "usage", f.mark+"-usage")
	f.add(c, "result", f.mark+"-answer 最终回答")
	first := f.add(c, "user", f.mark+"-both 第一次")
	f.add(c, "text", f.mark+"-both 说明")
	last := f.add(c, "user", f.mark+"-both 最近一次")
	// 仅有 summary、没有 detail 的老记录同样可搜。
	legacy := f.conv("legacy", time.Now())
	if _, err := f.d.Exec(`INSERT INTO conversation_activities(conversation_id, kind, summary) VALUES ($1,'user',$2)`, legacy, f.mark+"-legacy 只有摘要"); err != nil {
		t.Fatal(err)
	}

	hit := func(query string, tools bool) *ChatHit {
		t.Helper()
		got := f.all(ChatSessionQuery{Text: query, IncludeTools: tools})
		if len(got) == 0 {
			return nil
		}
		if len(got) != 1 {
			t.Fatalf("search %q returned %d sessions", query, len(got))
		}
		return got[0].Hit
	}
	for _, c := range []struct {
		query string
		tools bool
		kind  string // "" = 无命中
	}{
		{f.mark + "-tooluse", false, ""},
		{f.mark + "-toolout", false, ""},
		{f.mark + "-tooluse", true, "tool_use"},
		{f.mark + "-toolout", true, "tool_result"},
		{f.mark + "-think", true, ""},            // thinking 永远不可搜
		{f.mark + "-usage", true, ""},            // usage 行同理
		{f.mark + "-answer", false, "result"},    // 最终回答默认可搜
		{f.mark + "-legacy 只有摘要", false, "user"}, // summary 兜底
	} {
		h := hit(c.query, c.tools)
		switch {
		case c.kind == "" && h != nil:
			t.Errorf("search %q tools=%v unexpectedly hit %+v", c.query, c.tools, h)
		case c.kind != "" && (h == nil || h.Kind != c.kind || h.MatchedIn != "content"):
			t.Errorf("search %q tools=%v hit=%+v, want kind %s", c.query, c.tools, h, c.kind)
		}
	}
	// 同一会话多处命中:取最近一条,可从它的 id 继续读。
	if h := hit(f.mark+"-both", false); h == nil || h.ActivityID != last || h.ActivityID == first {
		t.Fatalf("hit=%+v, want latest activity %d", h, last)
	}
	// 仅标题命中:ActivityID 为 0,片段是标题。
	if h := hit(f.mark+" title", false); h == nil || h.MatchedIn != "title" || h.ActivityID != 0 || h.Snippet != f.mark+" title" {
		t.Fatalf("title hit=%+v", h)
	}
	// message_count 只统计所选种类。
	for tools, want := range map[bool]int{false: 4, true: 6} {
		got := f.all(ChatSessionQuery{Text: f.mark + " title", IncludeTools: tools})
		if len(got) != 1 || got[0].MessageCount != want {
			t.Fatalf("tools=%v message_count=%+v, want %d", tools, got, want)
		}
	}
}

func TestChatSessionSnippetIsBoundedAndRuneSafe(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv("snippet", time.Now())
	body := strings.Repeat("前文😀\n  ", 300) + f.mark + "-needle 命中词" + strings.Repeat("\t后文😀 ", 300)
	f.add(c, "user", body)
	got := f.all(ChatSessionQuery{Text: f.mark + "-needle"})
	if len(got) != 1 || got[0].Hit == nil {
		t.Fatalf("got %+v", got)
	}
	snip := got[0].Hit.Snippet
	if !utf8.ValidString(snip) || !strings.Contains(snip, f.mark+"-needle") {
		t.Fatalf("snippet invalid or missing needle: %q", snip)
	}
	if !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Fatalf("snippet must mark both cut ends: %q", snip)
	}
	if n := utf8.RuneCountInString(snip); n > historyContextChars+utf8.RuneCountInString(f.mark+"-needle")+2 {
		t.Fatalf("snippet has %d chars, not bounded", n)
	}
	if strings.ContainsAny(snip, "\n\t") || strings.Contains(snip, "  ") {
		t.Fatalf("whitespace not collapsed: %q", snip)
	}
	// 命中在开头、整条很短:不加省略号。
	short := f.conv("short", time.Now())
	f.add(short, "user", f.mark+"-short 一句话")
	got = f.all(ChatSessionQuery{Text: f.mark + "-short"})
	if len(got) != 1 || got[0].Hit.Snippet != f.mark+"-short 一句话" {
		t.Fatalf("short snippet=%+v", got)
	}
}

func TestChatSessionsLimitsAreClamped(t *testing.T) {
	f := newHistoryFixture(t)
	for i := range HistoryMaxSessions + 5 {
		f.conv(fmt.Sprintf("%s s%02d", f.mark, i), time.Now())
	}
	page, err := f.d.ChatSessions(context.Background(), ChatSessionQuery{Text: f.mark, Limit: 100000})
	if err != nil || len(page.Sessions) != HistoryMaxSessions || page.Next == nil {
		t.Fatalf("clamped page: n=%d next=%v err=%v", len(page.Sessions), page.Next, err)
	}
	page, err = f.d.ChatSessions(context.Background(), ChatSessionQuery{Text: f.mark})
	if err != nil || len(page.Sessions) != HistoryDefaultSessions {
		t.Fatalf("default page: n=%d err=%v", len(page.Sessions), err)
	}
	// 超长标题按上限截断输出。
	long := f.conv(f.mark+strings.Repeat("题", 500), time.Now())
	for _, s := range f.all(ChatSessionQuery{Text: f.mark + "题"}) {
		if s.ID == long && utf8.RuneCountInString(s.Title) != historyTitleChars {
			t.Fatalf("title has %d chars", utf8.RuneCountInString(s.Title))
		}
	}
}

// readAll 按返回的 Next 一路读完,返回每页。
func (f *historyFixture) readAll(conv int64, tools bool, limit, maxChars int) []*ChatTranscript {
	f.t.Helper()
	var pages []*ChatTranscript
	var pos HistoryPos
	for range 1000 {
		tr, err := f.d.ReadChatSession(context.Background(), conv, pos, tools, limit, maxChars)
		if err != nil || tr == nil {
			f.t.Fatalf("read: %v %v", tr, err)
		}
		pages = append(pages, tr)
		if tr.Next == nil {
			return pages
		}
		pos = *tr.Next
	}
	f.t.Fatal("read did not terminate")
	return nil
}

func TestReadChatSessionPagesInActivityOrder(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv("read", time.Now())
	var want []int64
	for i := range 7 {
		want = append(want, f.add(c, []string{"user", "text", "result"}[i%3], fmt.Sprintf("%s-%d", f.mark, i)))
		f.add(c, "thinking", "不应出现的推理")
	}
	var got []int64
	for _, p := range f.readAll(c, false, 3, 0) {
		if len(p.Records) > 3 {
			t.Fatalf("page has %d records", len(p.Records))
		}
		for _, r := range p.Records {
			got = append(got, r.ID)
			if r.Truncated || r.Offset != 0 || !strings.HasPrefix(r.Content, f.mark) || r.Chars != utf8.RuneCountInString(r.Content) {
				t.Fatalf("record %+v", r)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("read order %v, want %v", got, want)
	}
	// 页满时 Next 指向下一条记录;最后一页没有 Next。
	first, _ := f.d.ReadChatSession(context.Background(), c, HistoryPos{}, false, 3, 0)
	if first.Next == nil || first.Next.ID != want[3] || first.Next.Offset != 0 || first.Session.MessageCount != 7 {
		t.Fatalf("first page: next=%+v count=%d", first.Next, first.Session.MessageCount)
	}
	// 从中间的记录 id 起读(搜索命中后的用法)。
	mid, _ := f.d.ReadChatSession(context.Background(), c, HistoryPos{ID: want[5]}, false, 0, 0)
	if len(mid.Records) != 2 || mid.Records[0].ID != want[5] || mid.Next != nil {
		t.Fatalf("mid read: %+v", mid.Records)
	}
	// 工具记录默认不出现,显式要求才出现,并带上工具名。
	tool := f.add(c, "tool_result", "输出")
	for tools, wantTool := range map[bool]bool{false: false, true: true} {
		pages := f.readAll(c, tools, 50, 0)
		last := pages[len(pages)-1].Records
		hasTool := last[len(last)-1].ID == tool
		if hasTool != wantTool || (hasTool && last[len(last)-1].Tool != "Bash") {
			t.Fatalf("tools=%v last=%+v", tools, last[len(last)-1])
		}
	}
}

func TestReadChatSessionCharBudgetKeepsRecordsWhole(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv("budget", time.Now())
	var idsAdded []int64
	for range 5 {
		idsAdded = append(idsAdded, f.add(c, "user", strings.Repeat("字", 300)))
	}
	pages := f.readAll(c, false, 50, 700)
	if len(pages) != 3 {
		t.Fatalf("pages=%d, want 3 (2+2+1 records)", len(pages))
	}
	for i, p := range pages {
		total := 0
		for _, r := range p.Records {
			total += utf8.RuneCountInString(r.Content)
			if r.Truncated || r.Chars != 300 {
				t.Fatalf("page %d split a record that fit: %+v", i, r)
			}
		}
		if total > 700 {
			t.Fatalf("page %d has %d chars > budget", i, total)
		}
	}
	if next := pages[0].Next; next == nil || next.ID != idsAdded[2] || next.Offset != 0 {
		t.Fatalf("page 1 next=%+v, want start of 3rd record", next)
	}
}

func TestReadChatSessionSplitsOversizedRecordsLosslessly(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv("long", time.Now())
	var long strings.Builder
	for i := range 1000 {
		long.WriteString([]string{"中", "文", "😀", "x"}[i%4]) // 含 4 字节字符,字符偏移必须按字符而非字节
	}
	longID := f.add(c, "result", long.String())
	shortID := f.add(c, "user", strings.Repeat("尾", 50))

	pages := f.readAll(c, false, 50, 300)
	if len(pages) != 4 {
		t.Fatalf("pages=%d, want 4", len(pages))
	}
	var joined strings.Builder
	for i, p := range pages[:3] {
		r := p.Records[0]
		if len(p.Records) != 1 || r.ID != longID || !r.Truncated || r.Offset != i*300 || r.Chars != 1000 || utf8.RuneCountInString(r.Content) != 300 {
			t.Fatalf("page %d: %d records, first=%+v", i, len(p.Records), r)
		}
		if p.Next == nil || p.Next.ID != longID || p.Next.Offset != (i+1)*300 {
			t.Fatalf("page %d next=%+v", i, p.Next)
		}
		joined.WriteString(r.Content)
	}
	// 末段(100 字)读完这条,同页还能装下后面的短记录。
	last := pages[3]
	if len(last.Records) != 2 || last.Next != nil {
		t.Fatalf("last page: %+v next=%v", last.Records, last.Next)
	}
	if r := last.Records[0]; r.ID != longID || r.Truncated || r.Offset != 900 || utf8.RuneCountInString(r.Content) != 100 {
		t.Fatalf("final chunk=%+v", r)
	}
	if r := last.Records[1]; r.ID != shortID || r.Truncated {
		t.Fatalf("trailing record=%+v", r)
	}
	joined.WriteString(last.Records[0].Content)
	if joined.String() != long.String() {
		t.Fatal("reassembled chunks differ from the original record")
	}

	// 续读位置恰好落在记录末尾(陈旧游标):这条视为已读完,直接给下一条。
	tr, err := f.d.ReadChatSession(context.Background(), c, HistoryPos{ID: longID, Offset: 1000}, false, 0, 0)
	if err != nil || len(tr.Records) != 1 || tr.Records[0].ID != shortID {
		t.Fatalf("stale cursor at record end: %+v %v", tr, err)
	}
	// 预算低于下限时按下限处理,保证每次调用都能前进。
	tr, _ = f.d.ReadChatSession(context.Background(), c, HistoryPos{}, false, 0, 1)
	if got := utf8.RuneCountInString(tr.Records[0].Content); got != HistoryMinChars || tr.Next == nil {
		t.Fatalf("min budget chunk=%d next=%v", got, tr.Next)
	}
}

func TestReadChatSessionIsolationAndEmptyCases(t *testing.T) {
	f := newHistoryFixture(t)
	a, b := f.conv(f.mark+" A", time.Now()), f.conv(f.mark+" B", time.Now())
	f.add(a, "user", f.mark+"-only-in-A")
	bID := f.add(b, "user", f.mark+"-only-in-B")
	empty := f.conv("empty", time.Now())

	read := func(conv int64, pos HistoryPos) *ChatTranscript {
		tr, err := f.d.ReadChatSession(context.Background(), conv, pos, true, 50, 0)
		if err != nil || tr == nil {
			t.Fatalf("read %d: %v %v", conv, tr, err)
		}
		return tr
	}
	for _, pos := range []HistoryPos{{}, {ID: bID}, {ID: bID - 1}} { // 即便游标是别的会话的记录 id
		for _, r := range read(a, pos).Records {
			if strings.Contains(r.Content, "only-in-B") {
				t.Fatalf("session A leaked B's record with cursor %+v", pos)
			}
		}
	}
	if recs := read(b, HistoryPos{}).Records; len(recs) != 1 || recs[0].ID != bID {
		t.Fatalf("B records=%+v", recs)
	}
	// 搜索结果也不会把别的会话的命中算到本会话。
	for _, s := range f.all(ChatSessionQuery{Text: f.mark + "-only-in-A"}) {
		if s.ID != a {
			t.Fatalf("search leaked session %d", s.ID)
		}
	}

	tr := read(empty, HistoryPos{})
	if len(tr.Records) != 0 || tr.Next != nil || tr.Session.ID != empty || tr.Session.MessageCount != 0 {
		t.Fatalf("empty session: %+v", tr)
	}
	if tr, err := f.d.ReadChatSession(context.Background(), -1, HistoryPos{}, false, 0, 0); tr != nil || err != nil {
		t.Fatalf("absent session: %v %v", tr, err)
	}
	var none int64
	if err := f.d.QueryRow(`SELECT COALESCE(max(id),0)+1000 FROM conversations`).Scan(&none); err != nil {
		t.Fatal(err)
	}
	if tr, err := f.d.ReadChatSession(context.Background(), none, HistoryPos{}, false, 0, 0); tr != nil || err != nil {
		t.Fatalf("nonexistent session: %v %v", tr, err)
	}
	page, err := f.d.ChatSessions(context.Background(), ChatSessionQuery{Text: f.mark + " 完全不存在"})
	if err != nil || len(page.Sessions) != 0 || page.Next != nil {
		t.Fatalf("no-match search: %+v %v", page, err)
	}
}

func TestChatHistoryQueriesHonourContext(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv("ctx", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 用户中止本轮后,正在跑的历史查询不能继续占着数据库
	if _, err := f.d.ChatSessions(ctx, ChatSessionQuery{Text: f.mark}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatSessions err=%v", err)
	}
	if _, err := f.d.ReadChatSession(ctx, c, HistoryPos{}, false, 0, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadChatSession err=%v", err)
	}
}

func TestReadChatSessionRecordCountIsClamped(t *testing.T) {
	f := newHistoryFixture(t)
	c := f.conv("many", time.Now())
	for i := range HistoryMaxRecords + 5 {
		f.add(c, "user", fmt.Sprint(i))
	}
	tr, err := f.d.ReadChatSession(context.Background(), c, HistoryPos{}, false, 100000, HistoryMaxChars*10)
	if err != nil || len(tr.Records) != HistoryMaxRecords || tr.Next == nil {
		t.Fatalf("n=%d next=%v err=%v", len(tr.Records), tr.Next, err)
	}
	tr, _ = f.d.ReadChatSession(context.Background(), c, HistoryPos{}, false, 0, 0)
	if len(tr.Records) != HistoryDefaultRecords {
		t.Fatalf("default n=%d", len(tr.Records))
	}
}
