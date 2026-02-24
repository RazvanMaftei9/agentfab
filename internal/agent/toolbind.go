package agent

import (
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

var shellToolInfo = &schema.ToolInfo{
	Name: "shell",
	Desc: "Execute a shell command. Use for file operations, builds, git, or any CLI tool.",
	ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"command": {Type: schema.String, Desc: "The shell command to execute", Required: true},
	}),
}

var dataTypeMap = map[string]schema.DataType{
	"string":  schema.String,
	"integer": schema.Integer,
	"boolean": schema.Boolean,
	"number":  schema.Number,
}

// BuildToolInfos converts live ToolConfigs into Eino ToolInfo definitions for WithTools.
func BuildToolInfos(tools []runtime.ToolConfig) []*schema.ToolInfo {
	var infos []*schema.ToolInfo
	for _, tc := range tools {
		if !tc.IsLive() {
			continue
		}

		if tc.Name == "shell" {
			infos = append(infos, shellToolInfo)
			continue
		}

		params := make(map[string]*schema.ParameterInfo, len(tc.Parameters))
		for name, p := range tc.Parameters {
			dt, ok := dataTypeMap[p.Type]
			if !ok {
				dt = schema.String // default to string for unknown types
			}
			params[name] = &schema.ParameterInfo{
				Type:     dt,
				Desc:     p.Description,
				Required: p.Required,
			}
		}

		infos = append(infos, &schema.ToolInfo{
			Name:        tc.Name,
			Desc:        tc.Instructions,
			ParamsOneOf: schema.NewParamsOneOfByParams(params),
		})
	}
	return infos
}

func LiveTools(tools []runtime.ToolConfig) map[string]runtime.ToolConfig {
	m := make(map[string]runtime.ToolConfig)
	for _, tc := range tools {
		if tc.IsLive() {
			m[tc.Name] = tc
		}
	}
	return m
}
