package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// 会话历史的只读查询,给模型的 list/search/read 工具用。权威来源只有
// conversations + conversation_activities,不触碰任何配置/凭据表。所有输出都有硬上限,
// 工具结果不可能整库倾倒;分页一律用键集游标,翻页期间有新消息也不会重复或错位。

const (
	HistoryDefaultSessions = 20
	HistoryMaxSessions     = 50
	HistoryDefaultRecords  = 20
	HistoryMaxRecords      = 50
	HistoryDefaultChars    = 6000
	HistoryMinChars        = 200
	HistoryMaxChars        = 12000
	HistoryMaxQueryChars   = 200

	historyTitleChars   = 200 // 输出标题字符上限(用户可改名成任意长度)
	historyLeadChars    = 40  // 命中片段里,命中位置之前保留的字符数
	historyContextChars = 120 // 命中片段在查询文本之外再多取的字符数
)

// 正文 = 用户消息 + 助手说明(text) + 助手最终回答(result)。最终答案在 agent/capture.go
// 里只落成 result 行(尾部同文 text 会被丢弃去重),所以 result 必须算正文。thinking、
// usage、file 等其余种类从不对外。工具调用/输出需调用方显式要求。
var (
	historyMessageKinds = []string{"user", "text", "result"}
	historyToolKinds    = []string{"tool_use", "tool_result"}
)

func historyKinds(includeTools bool) []string {
	if includeTools {
		return slices.Concat(historyMessageKinds, historyToolKinds)
	}
	return historyMessageKinds
}

// ErrHistoryQuery 表示搜索文本无法安全使用(过长、含 NUL 或非法 UTF-8)。
var ErrHistoryQuery = fmt.Errorf("搜索文本无效：不能含 NUL 或非法 UTF-8，且不超过 %d 个字符", HistoryMaxQueryChars)

// HistoryCursor 是 (updated_at DESC, id DESC) 排序下的键集游标:下一页严格排在它之后。
type HistoryCursor struct {
	UpdatedAt time.Time
	ID        int64
}

// ChatHit 是搜索命中:标题命中时 ActivityID 为 0、Snippet 为标题;正文命中取该会话
// 最近一条命中的记录。
type ChatHit struct {
	MatchedIn  string // "title" | "content"
	ActivityID int64
	Kind       string
	At         time.Time
	Snippet    string
}

// ChatSession 是一个会话的摘要。MessageCount 只统计查询所选种类的记录。
type ChatSession struct {
	ID           int64
	AgentKey     string
	Title        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	MessageCount int
	Hit          *ChatHit // 仅搜索结果有
}

// ChatSessionQuery 描述一页会话。Text 为空=列出全部;否则按字面量(大小写不敏感)
// 搜索标题和正文,%、_、\ 都按普通字符处理。
type ChatSessionQuery struct {
	Text         string
	IncludeTools bool
	After        *HistoryCursor
	Limit        int
}

// ChatSessionPage 是一页结果;Next 非 nil 表示还有下一页。
type ChatSessionPage struct {
	Sessions []ChatSession
	Next     *HistoryCursor
}

// 命中的片段在 SQL 里按字符截取(substr 按字符而非字节,中文/emoji 不会被切坏),
// 因此长工具输出不会整段拉回 Go。分页先在 page CTE 里定页,再只给本页算消息数。
const chatSessionsSQL = `
WITH page AS (
  SELECT c.id, c.agent_key, left(c.title, $9) AS title, c.created_at, c.updated_at,
         h.id AS hit_id, h.kind AS hit_kind, h.created_at AS hit_at,
         h.start AS hit_start, h.len AS hit_len, h.snippet AS hit_snippet
  FROM conversations c
  LEFT JOIN LATERAL (
    SELECT a.id, a.kind, a.created_at, p.start, char_length(a.body) AS len, substr(a.body, p.start, $8) AS snippet
    FROM (SELECT id, kind, created_at, COALESCE(NULLIF(detail,''), summary, '') AS body
          FROM conversation_activities
          WHERE conversation_id = c.id AND kind = ANY($3::text[])) a
    CROSS JOIN LATERAL (SELECT GREATEST(strpos(lower(a.body), lower($1::text)) - $7::int, 1) AS start) p
    WHERE $1::text <> '' AND a.body ILIKE $2::text ESCAPE E'\\'
    ORDER BY a.id DESC LIMIT 1
  ) h ON true
  WHERE ($1::text = '' OR h.id IS NOT NULL OR c.title ILIKE $2::text ESCAPE E'\\')
    AND ($4::timestamptz IS NULL OR (c.updated_at, c.id) < ($4::timestamptz, $5::bigint))
  ORDER BY c.updated_at DESC, c.id DESC
  LIMIT $6
)
SELECT p.id, p.agent_key, p.title, p.created_at, p.updated_at, m.n,
       p.hit_id, p.hit_kind, p.hit_at, p.hit_start, p.hit_len, p.hit_snippet
FROM page p
LEFT JOIN LATERAL (
  SELECT count(*) AS n FROM conversation_activities
  WHERE conversation_id = p.id AND kind = ANY($3::text[])
) m ON true
ORDER BY p.updated_at DESC, p.id DESC`

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ChatSessions 返回一页会话,按最近更新时间稳定排序(相同时间按 id 降序)。
func (d *DB) ChatSessions(ctx context.Context, q ChatSessionQuery) (ChatSessionPage, error) {
	if n := utf8.RuneCountInString(q.Text); n > HistoryMaxQueryChars || !utf8.ValidString(q.Text) || strings.ContainsRune(q.Text, 0) {
		return ChatSessionPage{}, ErrHistoryQuery
	}
	limit := clampHistory(q.Limit, HistoryDefaultSessions, HistoryMaxSessions)
	var afterAt any // nil → SQL NULL → 首页
	var afterID int64
	if q.After != nil {
		afterAt, afterID = q.After.UpdatedAt, q.After.ID
	}
	window := historyContextChars + utf8.RuneCountInString(q.Text)
	rows, err := d.QueryContext(ctx, chatSessionsSQL, q.Text, "%"+likeEscaper.Replace(q.Text)+"%", historyKinds(q.IncludeTools),
		afterAt, afterID, limit+1, historyLeadChars, window, historyTitleChars)
	if err != nil {
		return ChatSessionPage{}, err
	}
	defer rows.Close()

	var page ChatSessionPage
	for rows.Next() {
		var s ChatSession
		var hitID, hitStart, hitLen sql.NullInt64
		var hitKind, hitSnippet sql.NullString
		var hitAt sql.NullTime
		if err := rows.Scan(&s.ID, &s.AgentKey, &s.Title, &s.CreatedAt, &s.UpdatedAt, &s.MessageCount,
			&hitID, &hitKind, &hitAt, &hitStart, &hitLen, &hitSnippet); err != nil {
			return ChatSessionPage{}, err
		}
		switch {
		case hitID.Valid:
			s.Hit = &ChatHit{MatchedIn: "content", ActivityID: hitID.Int64, Kind: hitKind.String, At: hitAt.Time,
				Snippet: snippetText(hitSnippet.String, int(hitStart.Int64), int(hitLen.Int64))}
		case q.Text != "":
			s.Hit = &ChatHit{MatchedIn: "title", Snippet: s.Title}
		}
		page.Sessions = append(page.Sessions, s)
	}
	if err := rows.Err(); err != nil {
		return ChatSessionPage{}, err
	}
	if len(page.Sessions) > limit {
		page.Sessions = page.Sessions[:limit]
		last := page.Sessions[limit-1]
		page.Next = &HistoryCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	}
	return page, nil
}

// snippetText 压平空白,并在片段两端不是原文边界时补省略号。start 是片段在原文中的
// 起始字符位置(从 1 计),total 是原文字符数。
func snippetText(raw string, start, total int) string {
	s := strings.Join(strings.Fields(raw), " ")
	if start > 1 {
		s = "…" + s
	}
	if start-1+utf8.RuneCountInString(raw) < total {
		s += "…"
	}
	return s
}

// HistoryPos 指向一条记录内的位置:从记录 ID 起读,并跳过其正文的前 Offset 个字符。
type HistoryPos struct {
	ID     int64
	Offset int
}

// ChatRecord 是一条可读记录。Content 最多是记录的一段:Offset 为本段起点,Chars 为整条
// 记录的字符数,Truncated 表示本段之后还有内容(从 Transcript.Next 继续)。
type ChatRecord struct {
	ID        int64
	Kind      string
	Tool      string
	IsError   bool
	At        time.Time
	Offset    int
	Chars     int
	Content   string
	Truncated bool
}

// ChatTranscript 是一页记录。Next 非 nil 表示还有内容,可直接作为下一次的起点。
type ChatTranscript struct {
	Session ChatSession
	Records []ChatRecord
	Next    *HistoryPos
}

const chatSessionHeaderSQL = `
SELECT c.id, c.agent_key, left(c.title, $2), c.created_at, c.updated_at,
       (SELECT count(*) FROM conversation_activities WHERE conversation_id = c.id AND kind = ANY($3::text[]))
FROM conversations c WHERE c.id = $1`

// 只有起点那条记录需要跳过 Offset 个字符,其后的记录从头取。每条最多取 maxChars 个字符,
// 实际用量由 Go 侧按预算裁定。
const chatRecordsSQL = `
SELECT a.id, a.kind, COALESCE(a.tool,''), a.is_error, a.created_at, char_length(b.body),
       substr(b.body, CASE WHEN a.id = $3 THEN $4::int + 1 ELSE 1 END, $5)
FROM conversation_activities a
CROSS JOIN LATERAL (SELECT COALESCE(NULLIF(a.detail,''), a.summary, '') AS body) b
WHERE a.conversation_id = $1 AND a.kind = ANY($2::text[]) AND a.id >= $3
ORDER BY a.id LIMIT $6`

// ReadChatSession 按活动顺序(id 升序)读取一个会话,只返回该会话自己的记录。
// 一页最多 limit 条且正文总计最多 maxChars 个字符:整条放得下才放,放不下就留到下一页;
// 只有单条记录本身超出预算时才把它切成片段,并用 Next 的 Offset 续读。
// 会话不存在返回 (nil, nil)。
func (d *DB) ReadChatSession(ctx context.Context, sessionID int64, from HistoryPos, includeTools bool, limit, maxChars int) (*ChatTranscript, error) {
	limit = clampHistory(limit, HistoryDefaultRecords, HistoryMaxRecords)
	maxChars = max(clampHistory(maxChars, HistoryDefaultChars, HistoryMaxChars), HistoryMinChars)
	kinds := historyKinds(includeTools)
	from.Offset = min(max(from.Offset, 0), 1<<30) // 单字段上限 1GB;同时保证能放进 SQL 的 int

	t := &ChatTranscript{}
	s := &t.Session
	err := d.QueryRowContext(ctx, chatSessionHeaderSQL, sessionID, historyTitleChars, kinds).
		Scan(&s.ID, &s.AgentKey, &s.Title, &s.CreatedAt, &s.UpdatedAt, &s.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := d.QueryContext(ctx, chatRecordsSQL, sessionID, kinds, from.ID, from.Offset, maxChars, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	budget := maxChars
	for rows.Next() {
		var r ChatRecord
		var chunk string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Tool, &r.IsError, &r.At, &r.Chars, &chunk); err != nil {
			return nil, err
		}
		if r.ID == from.ID {
			r.Offset = min(from.Offset, r.Chars)
		}
		remain := r.Chars - r.Offset
		switch {
		case len(t.Records) == limit:
			t.Next = &HistoryPos{ID: r.ID} // 第 limit+1 条:本页已满
		case len(t.Records) > 0 && remain > budget:
			t.Next = &HistoryPos{ID: r.ID, Offset: r.Offset} // 整条放不下,留到下一页
		case r.Offset > 0 && remain == 0:
			continue // 续读位置恰好落在记录末尾:这条已读完
		default:
			take := min(remain, budget)
			r.Content, r.Truncated = firstChars(chunk, take), take < remain
			budget -= take
			t.Records = append(t.Records, r)
			if r.Truncated {
				t.Next = &HistoryPos{ID: r.ID, Offset: r.Offset + take}
			}
		}
		if t.Next != nil {
			break
		}
	}
	return t, rows.Err()
}

// firstChars 返回 s 的前 n 个字符(不是字节),不会切开多字节字符。
func firstChars(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// clampHistory 把调用方给的数量收进 [1, upper];未给(<=0)用默认值。
func clampHistory(n, def, upper int) int {
	if n <= 0 {
		return def
	}
	return min(n, upper)
}
