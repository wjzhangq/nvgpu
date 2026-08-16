// Package api 暴露只读的 JSON HTTP 接口。
package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wjzhangq/gpumon/internal/config"
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

// Server 持有 API 依赖。
type Server struct {
	store *store.Store
	metas map[string]NodeMeta
	order []string
	cors  bool
}

// New 构造 API 服务。
func New(cfg *config.Config, st *store.Store) *Server {
	s := &Server{
		store: st,
		metas: make(map[string]NodeMeta, len(cfg.Nodes)),
		order: cfg.NodeNames(),
		cors:  cfg.Server.CORSEnabled(),
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
	return s
}

// Handler 返回配置好的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/v1/nodes", s.handleNodes)
	mux.HandleFunc("/api/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/api/v1/history", s.handleHistory)
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
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
