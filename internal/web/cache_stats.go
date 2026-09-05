package web

import (
	"sync"
	"time"
)

type CacheStats struct {
	mu sync.Mutex

	TotalRequests  int64         `json:"total_requests"`
	CacheHits      int64         `json:"cache_hits"`
	CacheMisses    int64         `json:"cache_misses"`
	TokensSent     int64         `json:"tokens_sent"`
	TokensSaved    int64         `json:"tokens_saved"`
	ActiveSessions int           `json:"active_sessions"`
	MaxSessionAge  time.Duration `json:"max_session_age"`
	HitRate        float64       `json:"hit_rate"`
	SavingsPercent float64       `json:"savings_percent"`

	// 按 API Key 统计
	KeyStats map[string]*KeyStat `json:"key_stats"`
}

// statsSnapshot 是无锁深拷贝快照，避免 GetStats 复制锁值。
type statsSnapshot struct {
	TotalRequests  int64         `json:"total_requests"`
	CacheHits      int64         `json:"cache_hits"`
	CacheMisses    int64         `json:"cache_misses"`
	TokensSent     int64         `json:"tokens_sent"`
	TokensSaved    int64         `json:"tokens_saved"`
	ActiveSessions int           `json:"active_sessions"`
	MaxSessionAge  time.Duration `json:"max_session_age"`
	HitRate        float64       `json:"hit_rate"`
	SavingsPercent float64       `json:"savings_percent"`
	KeyStats       map[string]*KeyStat `json:"key_stats"`
}

type KeyStat struct {
	APIKey        string    `json:"api_key"`
	TotalRequests int64     `json:"total_requests"`
	CacheHits     int64     `json:"cache_hits"`
	CacheMisses   int64     `json:"cache_misses"`
	TokensSent    int64     `json:"tokens_sent"`
	TokensSaved   int64     `json:"tokens_saved"`
	HitRate       float64   `json:"hit_rate"`
	LastUsed      time.Time `json:"last_used"`
}

var cacheStats = &CacheStats{
	KeyStats: make(map[string]*KeyStat),
}

func (s *CacheStats) RecordRequest(apiKey string, hit bool, tokensSent, tokensSaved int64, activeSessions int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalRequests++
	s.TokensSent += tokensSent
	s.TokensSaved += tokensSaved
	s.ActiveSessions = activeSessions

	if hit {
		s.CacheHits++
	} else {
		s.CacheMisses++
	}

	if s.TotalRequests > 0 {
		s.HitRate = float64(s.CacheHits) / float64(s.TotalRequests) * 100
	}
	if s.TokensSent+s.TokensSaved > 0 {
		s.SavingsPercent = float64(s.TokensSaved) / float64(s.TokensSent+s.TokensSaved) * 100
	}

	// 按 API Key 统计
	ks, ok := s.KeyStats[apiKey]
	if !ok {
		ks = &KeyStat{APIKey: apiKey}
		s.KeyStats[apiKey] = ks
	}
	ks.TotalRequests++
	ks.TokensSent += tokensSent
	ks.TokensSaved += tokensSaved
	ks.LastUsed = time.Now()
	if hit {
		ks.CacheHits++
	} else {
		ks.CacheMisses++
	}
	if ks.TotalRequests > 0 {
		ks.HitRate = float64(ks.CacheHits) / float64(ks.TotalRequests) * 100
	}
}

func (s *CacheStats) GetStats() statsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make(map[string]*KeyStat, len(s.KeyStats))
	for k, v := range s.KeyStats {
		cp := *v
		keys[k] = &cp
	}
	return statsSnapshot{
		TotalRequests:  s.TotalRequests,
		CacheHits:      s.CacheHits,
		CacheMisses:    s.CacheMisses,
		TokensSent:     s.TokensSent,
		TokensSaved:    s.TokensSaved,
		ActiveSessions: s.ActiveSessions,
		MaxSessionAge:  s.MaxSessionAge,
		HitRate:        s.HitRate,
		SavingsPercent: s.SavingsPercent,
		KeyStats:       keys,
	}
}

func (s *CacheStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalRequests = 0
	s.CacheHits = 0
	s.CacheMisses = 0
	s.TokensSent = 0
	s.TokensSaved = 0
	s.HitRate = 0
	s.SavingsPercent = 0
	s.KeyStats = make(map[string]*KeyStat)
}

// EstimateTokens counts completion/request text with the embedded tiktoken
// O200k vocabulary; the heuristic character count is only a fallback if the
// codec fails to initialize.
func EstimateTokens(text string) int64 {
	if enc, err := getGPTTokenizer(); err == nil {
		if ids, _, encErr := enc.Encode(text); encErr == nil {
			return int64(len(ids))
		}
	}
	return int64(heuristicTokenCount(text))
}

// truncateToTokens cuts text to at most max tokens, reporting whether any cut
// happened. Used to honor client max_tokens on non-stream completions.
func truncateToTokens(text string, max int64) (string, bool) {
	if max <= 0 {
		return text, false
	}
	enc, err := getGPTTokenizer()
	if err != nil {
		total := int64(heuristicTokenCount(text))
		if total <= max {
			return text, false
		}
		r := []rune(text)
		cut := int(float64(len(r)) * float64(max) / float64(total))
		if cut > len(r) {
			cut = len(r)
		}
		return string(r[:cut]), true
	}
	ids, _, encErr := enc.Encode(text)
	if encErr != nil || int64(len(ids)) <= max {
		return text, false
	}
	out, decErr := enc.Decode(ids[:max])
	if decErr != nil {
		return text, false
	}
	return out, true
}
