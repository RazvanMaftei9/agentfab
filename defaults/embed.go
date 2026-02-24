package defaults

import "embed"

//go:embed agents/*.yaml agents/*.md
var AgentFS embed.FS

//go:embed templates/*.yaml
var TemplateFS embed.FS
