package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolateThinkingDefaults(t *testing.T, enabled bool) {
	t.Helper()
	old := thinkingDefaultsEnabled.Swap(enabled)
	t.Cleanup(func() { thinkingDefaultsEnabled.Store(old) })
}

func TestThinkingDefaultsConfigValidation(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"thinkingDefaults":{}}`, `{"thinkingDefaults":{"enabled":false}}`, `{"thinkingDefaults":{"enabled":true}}`,
	} {
		c, err := parseRuntimeConfig([]byte(raw))
		if err != nil || c.ThinkingDefaults.Enabled != strings.Contains(raw, "true") {
			t.Fatalf("valid config rejected: %s %v", raw, err)
		}
	}
	for _, value := range []string{
		`null`, `true`, `[]`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":1}`, `{"enabled":true,"model":"other"}`,
	} {
		if _, err := parseRuntimeConfig([]byte(`{"thinkingDefaults":` + value + `}`)); err == nil {
			t.Fatalf("invalid config accepted: %s", value)
		}
	}
}

func TestThinkingDefaultsProtectsClientSettings(t *testing.T) {
	isolateThinkingDefaults(t, true)
	for _, raw := range []string{
		`{"reasoning_effort":"off"}`, `{"reasoning_effort":"none"}`, `{"reasoning_effort":"disabled"}`,
		`{"reasoning_effort":""}`, `{"reasoning_effort":null}`, `{"reasoning_effort":7}`,
		`{"reasoning_effort":"low"}`, `{"reasoning_effort":"max"}`,
		`{"thinking":{"type":"disabled"}}`, `{"thinking":{}}`, `{"thinking":null}`,
		`{"reasoning":{"effort":"none"}}`, `{"reasoningEffort":"off"}`, `{"reasoning_summary":""}`,
		`{"reasoning_effort":"high","thinking":{"type":"disabled"}}`,
		`{"extra_body":{"thinking":{"type":"disabled"}}}`, `{"extra_body":null}`, `{"enable_thinking":false}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var obj, want map[string]any
			_ = json.Unmarshal([]byte(raw), &obj)
			_ = json.Unmarshal([]byte(raw), &want)
			applyThinkingDefaults(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1, defaultThinkingModel)
			if !reflect.DeepEqual(obj, want) {
				t.Fatalf("client setting changed: %#v", obj)
			}
		})
	}
}

func TestThinkingDefaultsScopeAndRepairOrder(t *testing.T) {
	isolateThinkingDefaults(t, true)
	for _, tc := range []struct {
		path, model string
		enabled     bool
		want        bool
	}{
		{"/v1/chat/completions", defaultThinkingModel, true, true},
		{"/v1/chat/completions", defaultThinkingModel, false, false},
		{"/v1/responses", defaultThinkingModel, true, false},
		{"/v1/messages", defaultThinkingModel, true, false},
		{"/v1/chat/completions", "deepseek-other", true, false},
	} {
		thinkingDefaultsEnabled.Store(tc.enabled)
		assistant := map[string]any{"role": "assistant", "content": "previous answer"}
		obj := map[string]any{"model": tc.model, "messages": []any{map[string]any{"role": "system", "content": "system"}, assistant, map[string]any{"role": "user", "content": "continue"}}}
		prepareUpstreamChatRequest(obj, httptest.NewRequest("POST", tc.path, nil), 2, tc.model)
		if got := obj["reasoning_effort"] == "high"; got != tc.want {
			t.Fatalf("scope %s/%s enabled=%t got=%t", tc.path, tc.model, tc.enabled, got)
		}
		if _, repaired := assistant["reasoning_content"]; repaired != tc.want {
			t.Fatalf("history repair did not follow thinking: %#v", assistant)
		}
		if tc.want && obj["reasoning_summary"] != "auto" {
			t.Fatal("existing thinking normalization did not run after default")
		}
	}
}

func TestThinkingDefaultsLogsOnlyDecision(t *testing.T) {
	isolateThinkingDefaults(t, true)
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	for _, raw := range []string{
		`{"messages":"private-prompt"}`,
		`{"reasoning_effort":"private-invalid-effort"}`,
		`{"thinking":{"type":"disabled","text":"private-reasoning"}}`,
	} {
		var obj map[string]any
		_ = json.Unmarshal([]byte(raw), &obj)
		applyThinkingDefaults(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 3, defaultThinkingModel)
	}
	for _, state := range []string{"default_injected", "client_setting", "client_off"} {
		if !strings.Contains(output.String(), state) {
			t.Fatalf("missing decision %s", state)
		}
	}
	if strings.Contains(output.String(), "private-") {
		t.Fatal("request content leaked")
	}
}

func TestThinkingDefaultsAdminSaveAndConflict(t *testing.T) {
	isolateThinkingDefaults(t, false)
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	const endpoint = "/admin/api/thinking-defaults"
	for _, key := range []string{"", "feature-api-key"} {
		if w := adminRequest(handler, "GET", endpoint, key, ""); w.Code != 401 {
			t.Fatal("settings bypassed admin auth")
		}
	}
	old, _ := os.ReadFile(runtimeConfigFile)
	w := adminRequest(handler, "GET", endpoint, "feature-admin-key", "")
	var view struct {
		Enabled                 bool
		Model, Effort, Revision string
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Enabled || view.Model != defaultThinkingModel || view.Effort != "high" || view.Revision != configRevision(old) {
		t.Fatalf("invalid view: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "feature-") || strings.Contains(w.Body.String(), "visible prompt") {
		t.Fatal("view exposed full config")
	}
	for _, body := range []string{
		`{}`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":true,"unknown":1}`,
	} {
		if w := adminRequest(handler, "POST", endpoint, "feature-admin-key", body); w.Code != 400 {
			t.Fatalf("invalid input accepted: %d", w.Code)
		}
	}
	body, _ := json.Marshal(map[string]any{"enabled": true, "revision": view.Revision})
	w = adminRequest(handler, "POST", endpoint, "feature-admin-key", string(body))
	if w.Code != 200 || !thinkingDefaultsEnabled.Load() {
		t.Fatalf("save did not hot apply: %d %s", w.Code, w.Body.String())
	}
	updated, _ := os.ReadFile(runtimeConfigFile)
	parsed, err := parseRuntimeConfig(updated)
	if err != nil || !parsed.ThinkingDefaults.Enabled || parsed.SystemPrompt.Force != "visible prompt text" || parsed.Gateway.AdminKey != "feature-admin-key" {
		t.Fatal("save lost other fields or did not persist")
	}
	if w := adminRequest(handler, "POST", endpoint, "feature-admin-key", string(body)); w.Code != 409 {
		t.Fatal("stale revision overwrote config")
	}
	body, _ = json.Marshal(map[string]any{"enabled": false, "revision": configRevision(updated)})
	if w := adminRequest(handler, "POST", endpoint, "feature-admin-key", string(body)); w.Code != 200 || thinkingDefaultsEnabled.Load() {
		t.Fatal("disable did not hot apply")
	}
	backups, _ := filepath.Glob("config.json.thinking-*.bak")
	if len(backups) != 2 {
		t.Fatalf("expected two exclusive backups, got %d", len(backups))
	}
	foundOld, foundUpdated := false, false
	for _, name := range backups {
		raw, _ := os.ReadFile(name)
		foundOld = foundOld || bytes.Equal(raw, old)
		foundUpdated = foundUpdated || bytes.Equal(raw, updated)
	}
	if !foundOld || !foundUpdated {
		t.Fatal("backup overwritten or not exact")
	}
}

func TestThinkingDefaultsReloadAndFailurePreserveState(t *testing.T) {
	isolateThinkingDefaults(t, false)
	isolateAdminFeatures(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key"},"thinkingDefaults":{"enabled":true}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil || !thinkingDefaultsEnabled.Load() {
		t.Fatal("file reload did not enable defaults")
	}
	writeRuntimeFixture(t, `{"thinkingDefaults":{"enabled":"bad"}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err == nil || !thinkingDefaultsEnabled.Load() {
		t.Fatal("invalid reload changed active settings")
	}
	writeRuntimeFixture(t, `{}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil || thinkingDefaultsEnabled.Load() {
		t.Fatal("removed config did not restore default off")
	}
}

func TestThinkingDefaultsAdminRejectsStaleKeyAndApplyFailure(t *testing.T) {
	isolateThinkingDefaults(t, false)
	isolateAdminFeatures(t)
	const endpoint = "/admin/api/thinking-defaults"
	r := httptest.NewRequest("GET", endpoint, nil)
	r.Header.Set("Authorization", "Bearer stale-key")
	w := httptest.NewRecorder()
	handleWebUIThinkingDefaults(w, r)
	if w.Code != 401 {
		t.Fatal("direct handler skipped locked auth")
	}
	old, _ := os.ReadFile(runtimeConfigFile)
	changed := bytes.Replace(old, []byte("feature-admin-key"), []byte("new-admin-key"), 1)
	if err := os.WriteFile(runtimeConfigFile, changed, 0600); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIHandler()
	if w := adminRequest(handler, "GET", endpoint, "feature-admin-key", ""); w.Code != 409 {
		t.Fatal("external key change not detected")
	}
	// An external valid edit can still fail hot application (debug log unavailable).
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(old, &doc)
	doc["debug"] = json.RawMessage(`{"enabled":true}`)
	before, _ := json.Marshal(doc)
	if err := os.WriteFile(runtimeConfigFile, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"enabled": true, "revision": configRevision(before)})
	w = adminRequest(handler, "POST", endpoint, "feature-admin-key", string(body))
	after, _ := os.ReadFile(runtimeConfigFile)
	if w.Code != 500 || thinkingDefaultsEnabled.Load() || !bytes.Equal(before, after) {
		t.Fatal("failed apply changed runtime or failed to restore disk")
	}
}

func TestThinkingDefaultsConcurrentReadersAndUpdates(t *testing.T) {
	isolateThinkingDefaults(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				thinkingDefaultsEnabled.Store(n%2 == 0)
				obj := map[string]any{}
				applyThinkingDefaults(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 4, defaultThinkingModel)
				if v, exists := obj["reasoning_effort"]; exists && v != "high" {
					t.Error("invalid concurrent default")
				}
			}
		}()
	}
	wg.Wait()
}
