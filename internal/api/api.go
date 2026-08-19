// Package api 暴露只读的 JSON HTTP 接口。
package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wjzhangq/gpumon/internal/config"
	"github.com/wjzhangq/gpumon/internal/logx"
	"github.com/wjzhangq/gpumon/internal/model"
	"github.com/wjzhangq/gpumon/internal/store"
)

//go:embed dashboard.html
var dashboardHTML string

// NodeMeta 是节点的配置侧信息（不含指标）。
type NodeMeta struct {
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	Target          string  `json:"target,omitempty"`
	IntervalSeconds float64 `json:"interval_seconds"`
}

// NodeStatus 是 /nodes 的返回元素。
type NodeStatus struct {
	NodeMeta
	Online     bool       `json:"online"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	Error      string     `json:"error,omitempty"`
	SampleHost string     `json:"hostname,omitempty"`
}

// HistoryEntry 是 /history 里单个节点的历史序列。
type HistoryEntry struct {
	Node    string           `json:"node"`
	Count   int              `json:"count"`
	Samples []model.Snapshot `json:"samples"`
}

// AgentStatus 表示一个 agent 的上报状态。
type AgentStatus struct {
	Agent     string    `json:"agent"`
	Status    string    `json:"status"`     // "start" 或 "end"
	UpdatedAt time.Time `json:"updated_at"`
}

// agentStore 是 agent 状态的内存存储。
type agentStore struct {
	mu     sync.RWMutex
	agents map[string]*AgentStatus // key: agent 名称
}

// Server 持有 API 依赖。
type Server struct {
	store       *store.Store
	metas       map[string]NodeMeta
	order       []string
	cors        bool
	agentStore  *agentStore
	cleanupDone chan struct{}
}

// New 构造 API 服务。
func New(cfg *config.Config, st *store.Store) *Server {
	s := &Server{
		store: st,
		metas: make(map[string]NodeMeta, len(cfg.Nodes)),
		order: cfg.NodeNames(),
		cors:  cfg.Server.CORSEnabled(),
		agentStore: &agentStore{
			agents: make(map[string]*AgentStatus),
		},
		cleanupDone: make(chan struct{}),
	}
	for _, n := range cfg.Nodes {
		m := NodeMeta{
			Name:            n.Name,
			Type:            n.Type,
			IntervalSeconds: n.Interval.Seconds(),
		}
		if n.SSH != nil {
			m.Target = n.SSH.User + "@" + n.SSH.Addr()
		}
		s.metas[n.Name] = m
	}

	go s.runAgentCleanup()

	return s
}

// agentTTL 是 agent 状态的存活时长，超过这个时间没更新就被清理。
const agentTTL = time.Hour

// runAgentCleanup 周期性清理过期的 agent 状态。
func (s *Server) runAgentCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.sweepAgents(time.Now())
		case <-s.cleanupDone:
			return
		}
	}
}

// sweepAgents 删除在 now 时刻已超过 TTL 的 agent，返回删除数量。
func (s *Server) sweepAgents(now time.Time) int {
	s.agentStore.mu.Lock()
	defer s.agentStore.mu.Unlock()

	removed := 0
	for name, agent := range s.agentStore.agents {
		if now.Sub(agent.UpdatedAt) > agentTTL {
			delete(s.agentStore.agents, name)
			removed++
			logx.Debugf("agent %q 已清理（超过 %s 未更新）", name, agentTTL)
		}
	}
	return removed
}

// Shutdown 停止后台清理 goroutine。
func (s *Server) Shutdown() {
	close(s.cleanupDone)
}

// snapshotAgents 返回当前所有 agent 状态，按名称排序。
func (s *Server) snapshotAgents() []AgentStatus {
	s.agentStore.mu.RLock()
	agents := make([]AgentStatus, 0, len(s.agentStore.agents))
	for _, a := range s.agentStore.agents {
		agents = append(agents, *a)
	}
	s.agentStore.mu.RUnlock()

	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Agent < agents[j].Agent
	})
	return agents
}

// Handler 返回配置好的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/v1/nodes", s.handleNodes)
	mux.HandleFunc("/api/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/api/v1/history", s.handleHistory)
	mux.HandleFunc("/api/v1/agents", s.handleAgentsDispatch)
	mux.HandleFunc("/", s.handleDashboard)
	return s.withMiddleware(mux)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cors {
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = "*"
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// 除 /api/v1/agents 之外的接口一律只读。
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost:
			if r.URL.Path != "/api/v1/agents" {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
				return
			}
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
			return
		}

		if logx.Verbose() {
			start := time.Now()
			next.ServeHTTP(w, r)
			logx.Debugf("%s %s %s %dms", r.RemoteAddr, r.Method, r.URL.Path,
				time.Since(start).Milliseconds())
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"time":         time.Now(),
		"nodes":        len(s.order),
		"history_size": s.store.Size(),
	})
}

// handleDashboard 返回内嵌的单文件 Web 看板。
// 非 "/" 路径落到这里说明是未知路由，返回 JSON 404。
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboardHTML))
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	out := make([]NodeStatus, 0, len(s.order))
	for _, name := range s.order {
		st := NodeStatus{NodeMeta: s.metas[name]}
		if st.Name == "" {
			st.Name = name
		}
		if snap, ok := s.store.Latest(name); ok {
			ts := snap.Timestamp
			st.Online = snap.Online
			st.LastSeen = &ts
			st.Error = snap.Error
			st.SampleHost = snap.Host.Hostname
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now(),
		"nodes":        out,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	names, err := s.resolveNodes(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	out := make([]model.Snapshot, 0, len(names))
	for _, n := range names {
		if snap, ok := s.store.Latest(n); ok {
			out = append(out, snap)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now(),
		"nodes":        out,
		"agents":       s.snapshotAgents(),
	})
}

// handleAgentsDispatch 按方法分发：POST 上报状态，GET 查询状态。
func (s *Server) handleAgentsDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleAgentsPost(w, r)
		return
	}
	s.handleAgentsGet(w, r)
}

func (s *Server) handleAgentsPost(w http.ResponseWriter, r *http.Request) {
	var req AgentStatus
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}

	req.Agent = strings.TrimSpace(req.Agent)
	if req.Agent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent 字段必填"})
		return
	}
	if req.Status != "start" && req.Status != "end" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status 必须为 start 或 end"})
		return
	}

	req.UpdatedAt = time.Now()

	s.agentStore.mu.Lock()
	s.agentStore.agents[req.Agent] = &req
	s.agentStore.mu.Unlock()

	logx.Debugf("agent %q 上报状态 %s", req.Agent, req.Status)
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleAgentsGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now(),
		"agents":       s.snapshotAgents(),
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	names, err := s.resolveNodes(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	limit := s.store.Size()
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		v, convErr := strconv.Atoi(raw)
		if convErr != nil || v < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit 必须是正整数"})
			return
		}
		if v < limit {
			limit = v
		}
	}

	out := make([]HistoryEntry, 0, len(names))
	for _, n := range names {
		samples, ok := s.store.History(n, limit)
		if !ok {
			continue
		}
		out = append(out, HistoryEntry{Node: n, Count: len(samples), Samples: samples})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now(),
		"history_size": s.store.Size(),
		"limit":        limit,
		"nodes":        out,
	})
}

// resolveNodes 解析 ?node=a,b（可重复传参），留空表示全部节点。
func (s *Server) resolveNodes(r *http.Request) ([]string, error) {
	raw := r.URL.Query()["node"]
	if len(raw) == 0 {
		return s.order, nil
	}

	var names []string
	seen := make(map[string]bool)
	for _, group := range raw {
		for _, part := range strings.Split(group, ",") {
			name := strings.TrimSpace(part)
			if name == "" || seen[name] {
				continue
			}
			if !s.store.Has(name) {
				return nil, &unknownNodeError{name}
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return s.order, nil
	}
	return names, nil
}

type unknownNodeError struct{ name string }

func (e *unknownNodeError) Error() string { return "未知节点: " + e.name }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}
