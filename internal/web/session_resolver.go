package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionBinding 记录一次内容键复用的会话。身份字段（IP/user）仅作
// 诊断元数据保留，匹配判定只依赖上下文内容，见 Resolve 的内容键逻辑。
type sessionBinding struct {
	SessionID      string    `json:"sessionId"`
	ConversationID string    `json:"conversationId"`
	AccountID      string    `json:"accountId"`
	CreatedAt      time.Time `json:"createdAt"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
	IPFingerprint  string    `json:"ipFingerprint,omitempty"`
	UserField      string    `json:"userField,omitempty"`
	// ContextHistory 持久化保存最近一次协议请求的完整消息，供重启后继续做
	// 内容前缀匹配，避免进程重启导致所有会话键全部失效。
	ContextHistory []oaiMsg `json:"contextHistory,omitempty"`
}

type sessionResolver struct {
	mu          sync.Mutex
	path        string
	sessions    map[string]sessionBinding
	ttl         time.Duration
	contextTTL  time.Duration
	maxSessions int
	persist     *persistStore
}

const defaultMaxSessions = 1000

// envDurationMinutes parses a minutes-based env var. Plain numbers mean
// minutes ("120" → 120m); Go duration strings ("2h", "90m") pass through.
// The old v+"m" concatenation silently broke unit-bearing values ("2h" →
// "2hm" failed to parse and fell back to the default).
func envDurationMinutes(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return def
}

func openSessionResolver() *sessionResolver {
	// 闲置 2 小时即视为过期（用户 2 小时不活跃已经算久了）。会话过期后
	// 从 sessions.json 剔除，云端对话交给 auto_cleanup 按同一窗口回收。
	ttl := envDurationMinutes(os.Getenv("M365_SESSION_TTL_MINUTES"), 2*time.Hour)
	contextTTL := envDurationMinutes(os.Getenv("M365_CONTEXT_TTL_MINUTES"), 2*time.Hour)
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = "sessions.json"
	}
	sr := &sessionResolver{
		path:        path,
		sessions:    map[string]sessionBinding{},
		ttl:         ttl,
		contextTTL:  contextTTL,
		maxSessions: defaultMaxSessions,
	}
	sr.persist = &persistStore{flush: sr.flush}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	if b, err := os.ReadFile(sr.path); err == nil {
		var list []sessionBinding
		if err := json.Unmarshal(b, &list); err == nil {
			now := time.Now().UTC()
			for _, s := range list {
				if now.Sub(s.LastUsedAt) > sr.ttl {
					continue
				}
				sr.reindexLocked(s)
			}
		}
	}
}

// flush 在锁内生成快照，锁外写盘。
func (sr *sessionResolver) flush() error {
	sr.mu.Lock()
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	sr.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(sr.path, b, 0o600)
}

func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	// Resolve matches by linear scan (IP fingerprint filter + content prefix),
	// so no secondary index is maintained: every candidate index in the
	// history of this file was write-only dead weight.
	sr.sessions[s.SessionID] = s
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			sr.dropLocked(id)
		}
	}
	if len(sr.sessions) > sr.maxSessions {
		// Bound memory by dropping the least recently used sessions.
		ids := make([]string, 0, len(sr.sessions))
		last := make(map[string]time.Time, len(sr.sessions))
		for id, s := range sr.sessions {
			ids = append(ids, id)
			last[id] = s.LastUsedAt
		}
		sort.Slice(ids, func(i, j int) bool { return last[ids[i]].Before(last[ids[j]]) })
		for _, id := range ids[:len(sr.sessions)-sr.maxSessions] {
			sr.dropLocked(id)
		}
	}
}

func (sr *sessionResolver) dropLocked(id string) {
	delete(sr.sessions, id)
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
	// HistoryLen 是复用命中时"云端对话已包含的消息条数"，
	// 即增量发送的起点下标（body.Messages[HistoryLen:] 只发新增部分）。
	HistoryLen int
}

func clientIPFingerprint(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ua := r.Header.Get("User-Agent")
	data := host + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextSimilarity(a, b []oaiMsg) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// 全历史相似：把双方所有消息文本拼起来做 Jaccard，比只比最后一条
	// 更能代表整段上下文，避免两条短消息（如"继续"）就误判高度相似。
	var ta, tb strings.Builder
	for _, m := range a {
		ta.WriteString(m.Role + ":" + contentToString(m.Content) + "\n")
	}
	for _, m := range b {
		tb.WriteString(m.Role + ":" + contentToString(m.Content) + "\n")
	}
	return jaccardSimilarity(ta.String(), tb.String())
}

func jaccardSimilarity(a, b string) float64 {
	setA := tokenize(a)
	setB := tokenize(b)
	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	intersection := 0
	for k := range setA {
		if setB[k] {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func tokenize(s string) map[string]bool {
	tokens := map[string]bool{}
	words := strings.Fields(strings.ToLower(s))
	for _, w := range words {
		tokens[w] = true
	}
	return tokens
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	explicitID := r.Header.Get("X-M365-Session-Id")

	// 客户端显式指定的会话 ID 是最高优先级的续接语义：不参与任何身份判定，
	// 由调用方主动决定要继续哪个云端对话。
	if explicitID != "" {
		// Explicit IDs address the session store directly; no fingerprint or
		// secondary index participates.
		if sess, ok := sr.sessions[explicitID]; ok {
			sess.LastUsedAt = time.Now().UTC()
			sr.sessions[explicitID] = sess
			sr.persist.markDirty()
			return ResolveResult{
				SessionID:      sess.SessionID,
				ConversationID: sess.ConversationID,
				AccountID:      sess.AccountID,
				MatchedBy:      "explicit",
				IsNew:          false,
				HistoryLen:     len(sess.ContextHistory),
			}
		}
	}

	// 内容键：协议消息序列严格等于某个已记录会话的历史时直接复用这个
	// 云端对话，但只在同一 IP/UA 指纹下，避免短消息在不同用户间互串。
	// HistoryLen 返回该前缀长度，上层据此只发送 messages[HistoryLen:] 增量。
	ipFinger := clientIPFingerprint(r)
	if bestID, n := sr.matchContextLocked(ipFinger, body.Messages); bestID != "" {
		sess := sr.sessions[bestID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_prefix_%d", n),
			IsNew:          false,
			HistoryLen:     n,
		}
	}

	// 弱约束兜底：内容不构成严格前缀，但与某个历史高度相似（如客户端
	// 本地截断了历史），仍复用该会话。此时增量边界未知，上层发送全量。
	threshold := 0.6
	if v := os.Getenv("M365_CONTEXT_SIMILARITY"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			threshold = f
		}
	}
	bestMatchID := ""
	bestSimilarity := 0.0
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		// 兜底同样受身份约束：只有同一 IP/UA 才可能复用。
		if sess.IPFingerprint != ipFinger {
			continue
		}
		// 兜底也需要至少两轮历史，单条短消息（如"继续"）不触发。
		if len(sess.ContextHistory) < 2 {
			continue
		}
		sim := contextSimilarity(sess.ContextHistory, body.Messages)
		if sim > bestSimilarity {
			bestSimilarity = sim
			bestMatchID = id
		}
	}
	if bestMatchID != "" && bestSimilarity >= threshold {
		sess := sr.sessions[bestMatchID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestMatchID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_similar_%.2f", bestSimilarity),
			IsNew:          false,
		}
	}

	return ResolveResult{IsNew: true}
}

// matchContextLocked 从全部会话中找到其 contextHistory 严格作为消息前缀的
// 那个会话；只选前缀最长的一个，避免短前缀在不同会话间互撞。返回
// (sessionID, 匹配到的消息条数)。
func (sr *sessionResolver) matchContextLocked(ipFinger string, messages []oaiMsg) (string, int) {
	if len(messages) == 0 {
		return "", 0
	}
	bestID := ""
	bestN := 0
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		n := contextPrefixLen(sess.ContextHistory, messages)
		// 门槛：至少 2 条消息的前缀才算复用，避免不同用户以同一条
		// 短消息（如"继续"）互相串会话。
		if n > 1 && n > bestN {
			bestN = n
			bestID = id
		}
	}
	return bestID, bestN
}

// contextPrefixLen 返回 hist 是否严格是 msgs 的前缀。hist 为空或不是前缀
// 时返回 0；命中时返回 len(hist)，即增量发送起点。
func contextPrefixLen(hist, msgs []oaiMsg) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	for i := range hist {
		if !messagesEqual(hist[i], msgs[i]) {
			return 0
		}
	}
	return len(hist)
}

// messagesEqual 判定两条消息在会话键意义上等价：role 与文本内容一致。
// 忽略 tool_calls 的 ID 细节（会话键只关心内容如何被模型消化）。
func messagesEqual(a, b oaiMsg) bool {
	if a.Role != b.Role {
		return false
	}
	ta := contentToString(a.Content)
	tb := contentToString(b.Content)
	if ta != tb {
		return false
	}
	if (a.ToolCalls == nil) != (b.ToolCalls == nil) {
		return false
	}
	for i := range a.ToolCalls {
		if i >= len(b.ToolCalls) {
			return false
		}
		if toolCallEqual(a.ToolCalls[i], b.ToolCalls[i]) {
			continue
		}
		return false
	}
	return len(a.ToolCalls) == len(b.ToolCalls)
}

// toolCallEqual 比较 name 与 arguments，忽略 ID：同一段工具调用重放时
// ID 由客户端重新生成，不应影响会话键。
func toolCallEqual(x, y map[string]any) bool {
	xFunc, _ := x["function"].(map[string]any)
	yFunc, _ := y["function"].(map[string]any)
	xn, _ := xFunc["name"].(string)
	yn, _ := yFunc["name"].(string)
	if xn != yn {
		return false
	}
	xa, _ := xFunc["arguments"].(string)
	ya, _ := yFunc["arguments"].(string)
	return xa == ya
}

func (sr *sessionResolver) Bind(sessionID, conversationID, accountID string, body *oaiReq, r *http.Request) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	now := time.Now().UTC()
	explicitID := r.Header.Get("X-M365-Session-Id")
	if explicitID != "" && sessionID == "" {
		sessionID = explicitID
	}
	// 同一云端对话只保留一条记录：内容键命中后增量轮次更新已存在会话，
	// 而不是每次 Bind 都新建一条，避免 sessions.json 膨胀。
	if sessionID != "" {
		if sess, ok := sr.sessions[sessionID]; ok {
			sess.ConversationID = conversationID
			sess.AccountID = accountID
			sess.LastUsedAt = now
			sess.UserField = body.User
			sess.IPFingerprint = clientIPFingerprint(r)
			sess.ContextHistory = cloneMessages(body.Messages)
			sr.reindexLocked(sess)
			sr.persist.markDirty()
			return
		}
	}
	if sessionID == "" {
		for _, sess := range sr.sessions {
			if sess.ConversationID == conversationID {
				sess.LastUsedAt = now
				sess.AccountID = accountID
				sess.UserField = body.User
				sess.IPFingerprint = clientIPFingerprint(r)
				sess.ContextHistory = cloneMessages(body.Messages)
				sr.reindexLocked(sess)
				sr.persist.markDirty()
				return
			}
		}
		sessionID = uuid.NewString()
	}

	sess := sessionBinding{
		SessionID:      sessionID,
		ConversationID: conversationID,
		AccountID:      accountID,
		CreatedAt:      now,
		LastUsedAt:     now,
		IPFingerprint:  clientIPFingerprint(r),
		UserField:      body.User,
		ContextHistory: cloneMessages(body.Messages),
	}

	sr.reindexLocked(sess)
	sr.persist.markDirty()
}

func (sr *sessionResolver) GetSession(sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	return s, ok
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

func (sr *sessionResolver) DeleteSession(sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if _, ok := sr.sessions[sessionID]; !ok {
		return false
	}
	delete(sr.sessions, sessionID)
	sr.persist.markDirty()
	return true
}

// UnbindByConversation drops every session bound to the given conversation.
// Called after an automatic cleanup deletes the cloud conversation, so the
// anti-CrossID resolver never reuses a dead conversation.
func (sr *sessionResolver) UnbindByConversation(conversationID string) int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	removed := 0
	for sid, s := range sr.sessions {
		if s.ConversationID != conversationID {
			continue
		}
		delete(sr.sessions, sid)
		removed++
	}
	if removed > 0 {
		sr.persist.markDirty()
	}
	return removed
}

func cloneMessages(msgs []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, len(msgs))
	copy(out, msgs)
	return out
}
