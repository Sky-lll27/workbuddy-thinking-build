package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

const defaultThinkingModel = "deepseek-v4.1-flash"
const defaultThinkingEffort = "high"

var thinkingDefaultsEnabled atomic.Bool

type thinkingDefaultsFileConfig struct {
	Enabled bool `json:"enabled"`
}

// A missing section defaults off. A supplied section must contain boolean values,
// never coerce strings, numbers or null into an enabled switch.
func (c *thinkingDefaultsFileConfig) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("thinkingDefaults 必须是对象")
	}
	*c = thinkingDefaultsFileConfig{}
	for key, value := range fields {
		if key != "enabled" || (!bytes.Equal(bytes.TrimSpace(value), []byte("true")) && !bytes.Equal(bytes.TrimSpace(value), []byte("false"))) {
			return errors.New("thinkingDefaults.enabled 必须是布尔值，且不支持其他字段")
		}
		c.Enabled = bytes.Equal(bytes.TrimSpace(value), []byte("true"))
	}
	return nil
}

// Presence, not truthiness, protects explicit empty/invalid/conflicting input.
// Inspect aliases and extension containers without changing their old handling.
func clientThinkingSetting(obj map[string]any) string {
	state := ""
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "reasoning_summary", "reasoning", "thinking", "enable_thinking"} {
		value, exists := obj[key]
		if !exists {
			continue
		}
		state = "client_setting"
		if text, ok := value.(string); ok && thinkingOff(text) {
			return "client_off"
		}
		if enabled, ok := value.(bool); ok && !enabled {
			return "client_off"
		}
		if fields, ok := value.(map[string]any); ok {
			for _, subkey := range []string{"type", "effort"} {
				if text, ok := fields[subkey].(string); ok && thinkingOff(text) {
					return "client_off"
				}
			}
		}
	}
	if extra, exists := obj["extra_body"]; exists {
		if fields, ok := extra.(map[string]any); ok {
			if nested := clientThinkingSetting(fields); nested != "" {
				return nested
			}
		} else {
			state = "client_setting" // malformed extension: do not guess intent
		}
	}
	return state
}

func thinkingOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "none", "disabled":
		return true
	}
	return false
}

func applyThinkingDefaults(obj map[string]any, r *http.Request, id uint64, model string) {
	if obj == nil || r == nil || r.URL == nil || r.URL.Path != "/v1/chat/completions" || normalizeModelName(model) != defaultThinkingModel {
		return
	}
	state := clientThinkingSetting(obj)
	if state == "" {
		state = "default_disabled"
		if thinkingDefaultsEnabled.Load() {
			obj["reasoning_effort"] = defaultThinkingEffort
			state = "default_injected"
		}
	}
	// Only fixed labels and IDs: no request fields, prompts or reasoning text.
	log.Printf("[默认思考] traceId=%s requestId=%d model=%s source=%s", debugTraceID(r), id, defaultThinkingModel, state)
}

func thinkingDefaultsView(enabled bool, revision string) map[string]any {
	return map[string]any{"enabled": enabled, "model": defaultThinkingModel, "effort": defaultThinkingEffort, "revision": revision}
}

// Backup is exclusive and mode 0600; do not overwrite or follow an existing .bak.
func backupThinkingConfig(old []byte) error {
	f, err := os.CreateTemp(".", "config.json.thinking-*.bak")
	if err != nil {
		return err
	}
	name := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	if _, err := f.Write(old); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func handleWebUIThinkingDefaults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
		return
	}
	var input struct {
		Enabled  *bool  `json:"enabled"`
		Revision string `json:"revision"`
	}
	if r.Method == http.MethodPost {
		if err := decodeAdminJSON(w, r, &input); err != nil || input.Enabled == nil {
			writeJSONError(w, http.StatusBadRequest, "enabled 必须是布尔值，并提供 revision")
			return
		}
	}
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	key := currentAdminKey()
	if key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "管理 Key 已变化，请重新登录")
		return
	}
	old, err := readConfigEditorDocument()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "无法读取配置")
		return
	}
	c, err := parseRuntimeConfig(old)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "当前配置无效，请先修复配置")
		return
	}
	if c.Gateway.AdminKey != key {
		writeJSONError(w, http.StatusConflict, "管理 Key 已被外部修改，请重新登录")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, thinkingDefaultsView(c.ThinkingDefaults.Enabled, configRevision(old)))
		return
	}
	if input.Revision == "" || input.Revision != configRevision(old) {
		writeJSONError(w, http.StatusConflict, "配置已变化，请重新加载后保存")
		return
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(old, &doc)
	// Remove case aliases accepted by encoding/json so the new value is unambiguous.
	for key := range doc {
		if strings.EqualFold(key, "thinkingDefaults") {
			delete(doc, key)
		}
	}
	doc["thinkingDefaults"], _ = json.Marshal(thinkingDefaultsFileConfig{Enabled: *input.Enabled})
	if err := backupThinkingConfig(old); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "配置备份失败，未保存")
		return
	}
	current, err := readConfigEditorDocument()
	if err != nil || configRevision(current) != input.Revision {
		writeJSONError(w, http.StatusConflict, "磁盘配置已变化，请重新加载后保存")
		return
	}
	if r.Context().Err() != nil {
		writeJSONError(w, http.StatusRequestTimeout, "请求已取消，未保存")
		return
	}
	if err := persistRuntimeDocument(doc, old, debugTraceID(r)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "配置保存或应用失败，未完成更新")
		return
	}
	log.Printf("[默认思考配置] traceId=%s enabled=%t 结果=保存并热生效", debugTraceID(r), *input.Enabled)
	writeJSON(w, http.StatusOK, thinkingDefaultsView(*input.Enabled, hex.EncodeToString(runtimeAppliedHash[:])))
}
