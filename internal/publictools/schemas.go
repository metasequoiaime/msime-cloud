package publictools

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cachedToolSchemas 为每个工具目录独立缓存校验规则，编译时复制定义，避免外部修改影响校验。
func cachedToolSchemas(definitions func() []*mcp.Tool) func() map[string]*jsonschema.Resolved {
	return sync.OnceValue(func() map[string]*jsonschema.Resolved {
		out := make(map[string]*jsonschema.Resolved)
		for _, tool := range definitions() {
			raw, _ := json.Marshal(tool.InputSchema)
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				panic(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				panic(err)
			}
			out[tool.Name] = resolved
		}
		return out
	})
}

// validateToolArguments 按入口自己的目录校验参数，只返回固定错误码，不暴露参数或 Schema 细节。
func validateToolArguments(catalog map[string]*jsonschema.Resolved, name string, arguments json.RawMessage) error {
	schema, ok := catalog[name]
	if !ok {
		return errors.New("unknown_tool")
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if len(arguments) > MaxArguments {
		return errors.New("invalid_tool_arguments")
	}
	var value any
	if json.Unmarshal(arguments, &value) != nil || schema.Validate(value) != nil {
		return errors.New("invalid_tool_arguments")
	}
	return nil
}
