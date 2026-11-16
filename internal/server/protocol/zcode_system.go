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

func upstreamSystemBlocks(client []any) ([]any, error) {
	system := append(append([]any{}, zcodeUpstreamSystemPrefix...), client...)
	if len(system) == 3 {
		return system, nil
	}
	texts := []string{}
	var control any
	for _, value := range system[2:] {
		block := shared.Obj(value)
		if shared.Str(block["type"]) != "text" {
			return nil, fault.Invalid("系统前缀之后仅支持普通 text 系统块")
		}
		for key := range block {
			if key != "type" && key != "text" && key != "cache_control" {
				return nil, fault.Invalid("系统块存在无法合并的字段")
			}
		}
		texts = append(texts, shared.Str(block["text"]))
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
	return append(system[:2], last), nil
}
