package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjzhangq/gpumon/internal/store"
)

// newTestServer 构造一个不依赖配置文件的 Server，调用方负责 Shutdown。
func newTestServer(nodes ...string) *Server {
	s := &Server{
		store: store.New(60, nodes),
		metas: make(map[string]NodeMeta),
		order: nodes,
		agentStore: &agentStore{
			agents: make(map[string]*AgentStatus),
		},
		cleanupDone: make(chan struct{}),
	}
	return s
}

func postAgent(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAgentsDispatch(rec, req)
	return rec
}

func TestAgentPostAndGet(t *testing.T) {
	s := newTestServer()

	if rec := postAgent(t, s, `{"agent":"video","status":"start"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST 应返回 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postAgent(t, s, `{"agent":"audio","status":"end"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST 应返回 200，得到 %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgentsDispatch(rec, req)

	var got struct {
		Agents []AgentStatus `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("应有 2 个 agent，得到 %d", len(got.Agents))
	}
	// 按名称排序：audio 在 video 之前
	if got.Agents[0].Agent != "audio" || got.Agents[1].Agent != "video" {
		t.Fatalf("agent 未按名称排序: %#v", got.Agents)
	}
	if got.Agents[0].UpdatedAt.IsZero() {
		t.Error("updated_at 不应为零值")
	}
}

func TestAgentUpsertKeepsUnique(t *testing.T) {
	s := newTestServer()

	postAgent(t, s, `{"agent":"video","status":"start"}`)
	postAgent(t, s, `{"agent":"video","status":"end"}`)

	agents := s.snapshotAgents()
	if len(agents) != 1 {
		t.Fatalf("同名 agent 应只保留一条，得到 %d", len(agents))
	}
	if agents[0].Status != "end" {
		t.Fatalf("应保留最后一次上报的状态，得到 %q", agents[0].Status)
	}
}

func TestAgentPostValidation(t *testing.T) {
	s := newTestServer()

	cases := []struct {
		name string
		body string
	}{
		{"空 agent 名", `{"agent":"","status":"start"}`},
		{"仅空白的 agent 名", `{"agent":"   ","status":"start"}`},
		{"缺少 agent 字段", `{"status":"start"}`},
		{"非法 status", `{"agent":"video","status":"running"}`},
		{"缺少 status 字段", `{"agent":"video"}`},
		{"非法 JSON", `{oops}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postAgent(t, s, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应返回 400，得到 %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	if n := len(s.snapshotAgents()); n != 0 {
		t.Fatalf("非法请求不应写入数据，当前有 %d 条", n)
	}
}

func TestAgentSweepRemovesExpired(t *testing.T) {
	s := newTestServer()
	now := time.Now()

	s.agentStore.mu.Lock()
	s.agentStore.agents["fresh"] = &AgentStatus{Agent: "fresh", Status: "start", UpdatedAt: now}
	s.agentStore.agents["edge"] = &AgentStatus{Agent: "edge", Status: "start", UpdatedAt: now.Add(-agentTTL)}
	s.agentStore.agents["stale"] = &AgentStatus{Agent: "stale", Status: "end", UpdatedAt: now.Add(-agentTTL - time.Second)}
	s.agentStore.mu.Unlock()

	if removed := s.sweepAgents(now); removed != 1 {
		t.Fatalf("应清理 1 条，实际 %d", removed)
	}

	agents := s.snapshotAgents()
	if len(agents) != 2 {
		t.Fatalf("应剩 2 条，实际 %d: %#v", len(agents), agents)
	}
	for _, a := range agents {
		if a.Agent == "stale" {
			t.Error("过期的 stale 应被删除")
		}
	}
}

func TestMetricsIncludesAgents(t *testing.T) {
	s := newTestServer("node-a")
	postAgent(t, s, `{"agent":"video","status":"start"}`)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，得到 %d", rec.Code)
	}

	var got struct {
		Nodes  []json.RawMessage `json:"nodes"`
		Agents []AgentStatus     `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Agent != "video" {
		t.Fatalf("metrics 应带上 agents 字段: %#v", got.Agents)
	}
}

func TestAgentsEmptyIsArrayNotNull(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgentsDispatch(rec, req)

	if !strings.Contains(rec.Body.String(), `"agents":[]`) {
		t.Fatalf("无 agent 时应返回空数组，得到 %s", rec.Body.String())
	}
}

func TestMiddlewareAllowsPostOnlyForAgents(t *testing.T) {
	s := newTestServer()
	h := s.Handler()
	defer s.Shutdown()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/agents",
		strings.NewReader(`{"agent":"video","status":"start"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/agents 应放行，得到 %d: %s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"/api/v1/metrics", "/api/v1/nodes", "/api/v1/history", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s 应返回 405，得到 %d", path, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/agents", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE 应返回 405，得到 %d", rec.Code)
	}
}

func TestAgentConcurrentAccess(t *testing.T) {
	s := newTestServer()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			postAgent(t, s, `{"agent":"worker","status":"start"}`)
			_ = s.snapshotAgents()
			s.sweepAgents(time.Now())
		}(i)
	}
	wg.Wait()

	if n := len(s.snapshotAgents()); n != 1 {
		t.Fatalf("同名并发上报应只留 1 条，得到 %d", n)
	}
}
