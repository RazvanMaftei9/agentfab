package agent

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestBuildToolInfosConvertsParameters(t *testing.T) {
	tools := []runtime.ToolConfig{
		{
			Name:         "search",
			Instructions: "Search the web",
			Parameters: map[string]runtime.ToolParam{
				"query": {Type: "string", Description: "Search query", Required: true},
				"limit": {Type: "integer", Description: "Max results"},
			},
		},
	}

	infos := BuildToolInfos(tools)
	if len(infos) != 1 {
		t.Fatalf("expected 1 ToolInfo, got %d", len(infos))
	}

	info := infos[0]
	if info.Name != "search" {
		t.Errorf("name: got %q", info.Name)
	}
	if info.Desc != "Search the web" {
		t.Errorf("desc: got %q", info.Desc)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("ParamsOneOf should not be nil")
	}
}

func TestBuildToolInfosSkipsPostProcess(t *testing.T) {
	tools := []runtime.ToolConfig{
		{
			Name:         "chrome",
			Instructions: "Take screenshots",
			Command:      "chromium --headless",
			Config:       map[string]string{"match": "*.html"},
			// No Parameters → post-process only.
		},
		{
			Name:         "shell",
			Instructions: "Run commands",
			Parameters: map[string]runtime.ToolParam{
				"command": {Type: "string", Description: "Command to run", Required: true},
			},
		},
	}

	infos := BuildToolInfos(tools)
	if len(infos) != 1 {
		t.Fatalf("expected 1 ToolInfo (shell only), got %d", len(infos))
	}
	if infos[0].Name != "shell" {
		t.Errorf("expected shell tool, got %q", infos[0].Name)
	}
}

func TestShellToolInfo(t *testing.T) {
	tools := []runtime.ToolConfig{
		{
			Name:         "shell",
			Instructions: "Run commands",
			Command:      "$TOOL_ARG_COMMAND",
			Parameters: map[string]runtime.ToolParam{
				"command": {Type: "string", Description: "Command to run", Required: true},
			},
		},
	}

	infos := BuildToolInfos(tools)
	if len(infos) != 1 {
		t.Fatalf("expected 1 ToolInfo, got %d", len(infos))
	}

	// Shell tool should use the built-in definition.
	if infos[0] != shellToolInfo {
		t.Error("shell tool should use the built-in shellToolInfo")
	}
	if infos[0].Name != "shell" {
		t.Errorf("name: got %q", infos[0].Name)
	}
}

func TestBuildToolInfosMultipleTypes(t *testing.T) {
	tools := []runtime.ToolConfig{
		{
			Name:         "config",
			Instructions: "Configure settings",
			Parameters: map[string]runtime.ToolParam{
				"name":    {Type: "string", Description: "Setting name", Required: true},
				"value":   {Type: "number", Description: "Numeric value"},
				"count":   {Type: "integer", Description: "Count"},
				"enabled": {Type: "boolean", Description: "Enable flag"},
			},
		},
	}

	infos := BuildToolInfos(tools)
	if len(infos) != 1 {
		t.Fatalf("expected 1 ToolInfo, got %d", len(infos))
	}

	// Verify the schema was generated (we can't easily inspect ParamsOneOf internals
	// without calling ToJSONSchema, but we verify it's not nil).
	if infos[0].ParamsOneOf == nil {
		t.Error("expected ParamsOneOf to be set")
	}
}

func TestBuildToolInfosUnknownTypeDefaultsToString(t *testing.T) {
	tools := []runtime.ToolConfig{
		{
			Name:         "custom",
			Instructions: "Custom tool",
			Parameters: map[string]runtime.ToolParam{
				"data": {Type: "unknown_type", Description: "Some data", Required: true},
			},
		},
	}

	infos := BuildToolInfos(tools)
	if len(infos) != 1 {
		t.Fatalf("expected 1 ToolInfo, got %d", len(infos))
	}

	// Should not panic — unknown types default to string.
	// Verify the schema can be generated without error.
	js, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	if js.Properties == nil {
		t.Fatal("expected properties in JSON schema")
	}
	// Use ordered map Get method.
	prop, ok := js.Properties.Get("data")
	if !ok || prop == nil {
		t.Error("expected 'data' property in JSON schema")
	} else if prop.Type != string(schema.String) {
		t.Errorf("unknown type should default to string, got %q", prop.Type)
	}
}

func TestLiveTools(t *testing.T) {
	tools := []runtime.ToolConfig{
		{Name: "shell", Parameters: map[string]runtime.ToolParam{"command": {Type: "string"}}},
		{Name: "chrome", Command: "chromium", Config: map[string]string{"match": "*.html"}},
		{Name: "search", Parameters: map[string]runtime.ToolParam{"query": {Type: "string"}}},
	}

	live := LiveTools(tools)
	if len(live) != 2 {
		t.Fatalf("expected 2 live tools, got %d", len(live))
	}
	if _, ok := live["shell"]; !ok {
		t.Error("missing shell")
	}
	if _, ok := live["search"]; !ok {
		t.Error("missing search")
	}
	if _, ok := live["chrome"]; ok {
		t.Error("chrome should not be a live tool (no parameters)")
	}
}
