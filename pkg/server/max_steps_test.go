package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
)

type configLimitProvider struct{ calls int }

func (*configLimitProvider) Name() string { return "config-limit-stub" }

func (p *configLimitProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	p.calls++
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "call", ToolName: "missing_test_tool"}
	ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call", InputDelta: "{}"}
	close(ch)
	return ch, nil
}

func configLimitRequest(t *testing.T, s *Server, method, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleConfig(w, httptest.NewRequest(method, "/api/config", strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return w.Code, out
}

func TestConfigMaxStepsBoundariesAndReload(t *testing.T) {
	dir := t.TempDir()
	p := &configLimitProvider{}
	d := newTestDepsAtProvider(t, dir, p)
	d.cfg.LLM.APIKey = ""
	s := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)
	before := *d.cfg
	status, view := configLimitRequest(t, s, http.MethodGet, "")
	if status != http.StatusOK || view["max_steps"] != float64(before.Agent.MaxSteps) {
		t.Fatalf("GET status=%d view=%v", status, view)
	}
	for _, limit := range []int{1, 100, 1000, 3} {
		status, out := configLimitRequest(t, s, http.MethodPost, fmt.Sprintf(`{"max_steps":%d}`, limit))
		if status != http.StatusOK || out["config"].(map[string]any)["max_steps"] != float64(limit) {
			t.Fatalf("POST status=%d out=%v", status, out)
		}
		if d.agent.MaxSteps() != limit || d.cfg.Agent.MaxSteps != limit {
			t.Fatal("runtime limit was not updated")
		}
		want := before
		want.Agent.MaxSteps = limit
		if !reflect.DeepEqual(*d.cfg, want) {
			t.Fatal("max_steps changed unrelated configuration")
		}
		loaded, err := config.Load(dir)
		if err != nil || loaded.Agent.MaxSteps != limit {
			t.Fatalf("reload limit failed: %v", err)
		}
		status, view = configLimitRequest(t, s, http.MethodGet, "")
		if status != http.StatusOK || view["max_steps"] != float64(limit) {
			t.Fatalf("GET did not reflect limit: %v", view)
		}
		p.calls = 0
		sess, err := d.agent.History().Create(d.dir, "limit")
		if err != nil {
			t.Fatal(err)
		}
		err = d.agent.Run(context.Background(), sess.ID, "continue", func(ev agent.Event) {
			if ev.Type == agent.EventDone {
				t.Error("exhaustion emitted done")
			}
		})
		if err == nil || p.calls != limit || !strings.Contains(err.Error(), fmt.Sprintf("%d 轮", limit)) {
			t.Fatalf("stub provider or limit changed: calls=%d err=%v", p.calls, err)
		}
	}
}

func TestConfigMaxStepsInvalidIsAtomic(t *testing.T) {
	dir := t.TempDir()
	d := newTestDepsAt(t, dir)
	d.cfg.LLM.APIKey = ""
	s := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)
	path := filepath.Join(dir, "local.yaml")
	if err := d.cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	before := *d.cfg
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bodies := []string{
		`{"max_steps":0}`, `{"max_steps":-1}`,
		`{"max_steps":1.5}`, `{"max_steps":1.0}`, `{"max_steps":"2"}`,
		`{"max_steps":null}`, `{"max_steps":true}`, `{"max_steps":[]}`,
		`{"max_steps":999999999999999999999999999999}`, `{"max_steps":`,
		`{"max_steps":0,"model":"changed","retry_mode":"fixed"}`,
		`{"max_steps":2,"retry_mode":"invalid"}`,
		`{"max_steps":2,"retry_mode":"fixed","retry_max_attempts":16}`,
		`{"max_steps":2,"retry_interval_sec":61}`,
		`{"max_steps":2,"provider":"invalid"}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			status, out := configLimitRequest(t, s, http.MethodPost, body)
			if status != http.StatusBadRequest || out["error"] == nil {
				t.Fatalf("status=%d out=%v", status, out)
			}
			if !reflect.DeepEqual(*d.cfg, before) || d.agent.MaxSteps() != before.Agent.MaxSteps {
				t.Fatal("invalid request partially updated configuration")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(saved) {
				t.Fatal("invalid request changed saved configuration")
			}
		})
	}
}

func TestConfigMaxStepsSaveFailureIsAtomic(t *testing.T) {
	for _, body := range []string{`{"max_steps":2}`, `{"max_steps":2,"model":"changed"}`} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			d := newTestDepsAt(t, dir)
			d.cfg.LLM.APIKey = "test-key"
			s := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)
			before := *d.cfg
			// 让写盘必然失败：state.yaml 的位置放一个目录，
			// os.WriteFile 就会报错（旧实现是把 local.yaml 做成目录）。
			if err := os.Mkdir(config.StatePath(), 0o755); err != nil {
				t.Fatal(err)
			}
			status, out := configLimitRequest(t, s, http.MethodPost, body)
			if status != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(out["error"]), "保存配置失败") {
				t.Fatalf("status=%d out=%v", status, out)
			}
			if !reflect.DeepEqual(*d.cfg, before) || d.agent.MaxSteps() != before.Agent.MaxSteps {
				t.Fatal("save failure partially updated configuration")
			}
		})
	}
}

func TestConfigMaxStepsOmittedPreservesLimit(t *testing.T) {
	dir := t.TempDir()
	d := newTestDepsAt(t, dir)
	d.cfg.LLM.APIKey = "test-key"
	s := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)
	status, out := configLimitRequest(t, s, http.MethodPost, `{"max_steps":7,"retry_mode":"fixed"}`)
	if status != http.StatusOK || d.cfg.LLM.RetryMode != "fixed" {
		t.Fatalf("mixed update status=%d out=%v", status, out)
	}
	for _, body := range []string{`{}`, `{"max_tokens":2048,"temperature":0.25,"retry_interval_sec":3}`} {
		status, out = configLimitRequest(t, s, http.MethodPost, body)
		if status != http.StatusOK || d.cfg.Agent.MaxSteps != 7 || d.agent.MaxSteps() != 7 {
			t.Fatalf("omitted limit status=%d out=%v", status, out)
		}
	}
	loaded, err := config.Load(dir)
	if err != nil || loaded.Agent.MaxSteps != 7 || loaded.LLM.MaxTokens != 2048 || loaded.LLM.Temperature != 0.25 || loaded.LLM.RetryBackoffMs != 3000 {
		t.Fatalf("reload after LLM update failed: %v", err)
	}
}
