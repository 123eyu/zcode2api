package protocol

import (
	_ "embed"
	"strings"
	"zcode2api/internal/server/fault"
	"zcode2api/internal/server/shared"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

// Keep the upstream client template separate from caller instructions. Omitting
// it causes otherwise valid inference requests to be rejected with code 3012.
var zcodeUpstreamSystemPrefix = func() []any {
	blocks := shared.Arr(shared.Parse(string(zcodeSystemJSON), nil))
	if len(blocks) != 3 {
		panic("zcode_system.json must contain exactly three system blocks")
	}
	return blocks
}()

const (
	TemplateMain           = "main"
	TemplateSubagent       = "subagent"
	MaxCustomTemplateBytes = 256 << 10
)

type SystemTemplate struct {
	Mode            string
	Custom          string
	InsertEnabled   bool
	HarnessEnabled  bool
	FilesEnabled    bool
	CommentsEnabled bool
}

func NewSystemTemplate(mode, custom string) (SystemTemplate, error) {
	if mode == "" {
		mode = TemplateMain
	}
	template := SystemTemplate{Mode: mode, Custom: custom}
	switch mode {
	case TemplateMain, TemplateSubagent:
	default:
		return template, fault.Invalid("模板模式仅支持 main 或 subagent")
	}
	if len(custom) > MaxCustomTemplateBytes {
		return template, fault.Invalid("自定义模板不能超过 256 KiB")
	}
	return template, nil
}

func (t SystemTemplate) SessionType() string {
	if t.Mode == TemplateSubagent {
		return "subagent"
	}
	return "main"
}

func (t SystemTemplate) QuerySource() string {
	if t.SessionType() == "subagent" {
		return "subagent"
	}
	return "main_turn"
}

func upstreamSystemBlocks(client []any, template SystemTemplate) ([]any, error) {
	prefix := zcodeUpstreamSystemPrefix
	if template.SessionType() == "subagent" {
		// Identity is fixed; disabling every optional section leaves an empty body.
		prefix = []any{
			shared.M{"type": "text", "text": zcodeSubagentIdentity},
			shared.M{"type": "text", "text": subagentSystem(template)},
		}
	}
	system := append(append([]any{}, prefix...), client...)
	if len(system) == len(prefix) {
		return system, nil
	}
	mergeAt := len(prefix) - 1
	texts := []string{}
	var control any
	for _, value := range system[mergeAt:] {
		block := shared.Obj(value)
		if shared.Str(block["type"]) != "text" {
			return nil, fault.Invalid("系统前缀之后仅支持普通 text 系统块")
		}
		for key := range block {
			if key != "type" && key != "text" && key != "cache_control" {
				return nil, fault.Invalid("系统块存在无法合并的字段")
			}
		}
		if text := shared.Str(block["text"]); template.Mode != TemplateSubagent || text != "" {
			texts = append(texts, text)
		}
		if block["cache_control"] != nil {
			if control != nil && shared.JSON(control) != shared.JSON(block["cache_control"]) {
				return nil, fault.Invalid("不能保留多个不同的 system 缓存策略")
			}
			control = block["cache_control"]
		}
	}
	last := shared.M{"type": "text", "text": strings.Join(texts, "\n\n")}
	if control != nil {
		last["cache_control"] = control
	}
	return append(system[:mergeAt], last), nil
}
