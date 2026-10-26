package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type responsesToolKind string

const (
	responsesToolFunction  responsesToolKind = "function"
	responsesToolNamespace responsesToolKind = "namespace"
	responsesToolCustom    responsesToolKind = "custom"
	responsesToolWebSearch responsesToolKind = "web_search"
)

// responsesToolMeta records how an upstream flat function name maps back to the
// Responses item shape expected by the client.
type responsesToolMeta struct {
	UpstreamName string
	Name         string
	Namespace    string
	Kind         responsesToolKind
}

type responsesToolMapContextKey struct{}

func withResponsesToolMap(r *http.Request, tools map[string]responsesToolMeta) *http.Request {
	if r == nil || len(tools) == 0 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), responsesToolMapContextKey{}, tools))
}

func responsesToolMapFor(r *http.Request) map[string]responsesToolMeta {
	if r == nil {
		return nil
	}
	tools, _ := r.Context().Value(responsesToolMapContextKey{}).(map[string]responsesToolMeta)
	return tools
}

func responsesToolMetaFor(r *http.Request, upstreamName string) (responsesToolMeta, bool) {
	tools := responsesToolMapFor(r)
	if tools == nil {
		return responsesToolMeta{}, false
	}
	meta, ok := tools[upstreamName]
	return meta, ok
}

func flattenNamespaceToolName(namespace, name string) string {
	if strings.TrimSpace(namespace) == "" {
		return name
	}
	return namespace + "__" + name
}

func customToolSchema(description string) map[string]any {
	props := map[string]any{
		"input": map[string]any{
			"type":        "string",
			"description": "Raw text input for this custom tool.",
		},
	}
	if description != "" {
		props["input"].(map[string]any)["description"] = description
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             []any{"input"},
		"additionalProperties": false,
	}
}

func functionTool(name, description string, parameters any, strict any) map[string]any {
	fn := map[string]any{"name": name}
	if description != "" {
		fn["description"] = description
	}
	if parameters != nil {
		fn["parameters"] = parameters
	}
	if strict != nil {
		fn["strict"] = strict
	}
	return map[string]any{"type": "function", "function": fn}
}

// convertResponsesToolsDetailed flattens Responses namespace/custom tools into
// Chat function tools and returns the reverse map used to restore the client
// shape in streaming and aggregate responses.
func convertResponsesToolsDetailed(toolsAny any) ([]any, map[string]responsesToolMeta) {
	arr, ok := toolsAny.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]any, 0, len(arr))
	tools := make(map[string]responsesToolMeta)
	seen := make(map[string]bool)
	add := func(tool map[string]any, meta responsesToolMeta) {
		if meta.UpstreamName == "" || seen[meta.UpstreamName] {
			return
		}
		seen[meta.UpstreamName] = true
		tools[meta.UpstreamName] = meta
		out = append(out, tool)
	}
	addFunction := func(namespace, name, description string, parameters, strict any) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		upstream := flattenNamespaceToolName(namespace, name)
		kind := responsesToolFunction
		if namespace != "" {
			kind = responsesToolNamespace
		}
		add(functionTool(upstream, description, parameters, strict), responsesToolMeta{
			UpstreamName: upstream, Name: name, Namespace: namespace, Kind: kind,
		})
	}
	addCustom := func(namespace, name, description string, format any) {
		name = strings.TrimSpace(name)
		if name == "" {
			name = "custom_tool"
		}
		upstream := flattenNamespaceToolName(namespace, name)
		desc := description
		if format != nil {
			if b, err := json.Marshal(format); err == nil {
				if desc != "" {
					desc += "\n"
				}
				desc += "Input format: " + string(b)
			}
		}
		add(functionTool(upstream, desc, customToolSchema("Raw input for "+name+"."), nil), responsesToolMeta{
			UpstreamName: upstream, Name: name, Namespace: namespace, Kind: responsesToolCustom,
		})
	}
	addWebSearch := func() {
		name := "web_search"
		add(functionTool(name, "Search the web and return results. Provide a concise query.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query."},
			},
			"required": []any{"query"},
		}, nil), responsesToolMeta{UpstreamName: name, Name: name, Kind: responsesToolWebSearch})
	}
	for _, raw := range arr {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tool["type"].(string)
		switch typ {
		case "", "function":
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			if name == "" {
				if fn, ok := tool["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
					desc, _ = fn["description"].(string)
					addFunction("", name, desc, fn["parameters"], fn["strict"])
					continue
				}
			}
			addFunction("", name, desc, tool["parameters"], tool["strict"])
		case "namespace":
			namespace, _ := tool["name"].(string)
			namespace = strings.TrimSpace(namespace)
			if namespace == "" {
				continue
			}
			children, _ := tool["tools"].([]any)
			for _, childAny := range children {
				child, ok := childAny.(map[string]any)
				if !ok {
					continue
				}
				childType, _ := child["type"].(string)
				name, _ := child["name"].(string)
				desc, _ := child["description"].(string)
				if childType == "custom" {
					addCustom(namespace, name, desc, child["format"])
					continue
				}
				addFunction(namespace, name, desc, child["parameters"], child["strict"])
			}
		case "custom":
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			addCustom("", name, desc, tool["format"])
		case "web_search":
			addWebSearch()
		}
	}
	return out, tools
}

func convertResponsesTools(toolsAny any) []any {
	tools, _ := convertResponsesToolsDetailed(toolsAny)
	return tools
}

func customToolInputFromArguments(arguments string) string {
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) == nil {
		if input, ok := args["input"].(string); ok {
			return input
		}
	}
	return arguments
}

func customToolArguments(input string) string {
	b, _ := json.Marshal(map[string]any{"input": input})
	return string(b)
}

// responsesToolCallItem restores the client-facing item shape after the upstream
// returns a flat Chat function call.
func responsesToolCallItem(callID, upstreamName, arguments string, tools map[string]responsesToolMeta) map[string]any {
	meta, ok := tools[upstreamName]
	if ok && meta.Kind == responsesToolCustom {
		return map[string]any{
			"id":      "ctc_" + compactUUID(),
			"type":    "custom_tool_call",
			"status":  "completed",
			"call_id": ifEmpty(callID, "call_"+compactUUID()),
			"name":    meta.Name,
			"input":   customToolInputFromArguments(arguments),
		}
	}
	item := map[string]any{
		"id":        "fc_" + compactUUID(),
		"type":      "function_call",
		"status":    "completed",
		"call_id":   ifEmpty(callID, "call_"+compactUUID()),
		"name":      upstreamName,
		"arguments": arguments,
	}
	if ok && meta.Namespace != "" {
		item["name"] = meta.Name
		item["namespace"] = meta.Namespace
	}
	return item
}
