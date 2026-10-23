package publictools

import (
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCachedToolSchemasKeepsCatalogsAndCompiledDefinitionsIndependent(t *testing.T) {
	stringSchema := map[string]any{"type": "string", "enum": []string{"synthetic"}}
	strings := cachedToolSchemas(func() []*mcp.Tool {
		return []*mcp.Tool{{Name: "same_name", InputSchema: stringSchema}}
	})
	numbers := cachedToolSchemas(func() []*mcp.Tool {
		return []*mcp.Tool{{Name: "same_name", InputSchema: map[string]any{"type": "integer"}}}
	})
	for _, tc := range []struct {
		name string
		get  func() bool
	}{
		{"字符串目录", func() bool {
			s := strings()["same_name"]
			return s != nil && s.Validate("synthetic") == nil && s.Validate(float64(1)) != nil
		}},
		{"整数目录", func() bool {
			s := numbers()["same_name"]
			return s != nil && s.Validate(float64(1)) == nil && s.Validate("synthetic") != nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.get() {
				t.Fatal("同名工具的独立目录混用了校验规则")
			}
		})
	}
	stringSchema["enum"] = []string{"changed"}
	if s := strings()["same_name"]; s == nil || s.Validate("synthetic") != nil || s.Validate("changed") == nil {
		t.Fatal("修改源定义影响了已缓存的校验规则")
	}
}

func TestCachedToolSchemasSupportsConcurrentFirstUse(t *testing.T) {
	schemas := cachedToolSchemas(Definitions)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 20 {
				catalog := schemas()
				lookup := catalog["lookup_dictionary"]
				if len(catalog) != 3 || lookup == nil || lookup.Validate(map[string]any{"text": "synthetic"}) != nil || lookup.Validate(map[string]any{}) == nil {
					t.Error("并发初始化丢失了工具目录或必填参数约束")
					return
				}
			}
		})
	}
	workers.Wait()
}

func TestCachedToolSchemasPanicsForBrokenDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema any
	}{
		{"无法解码的定义", 42},
		{"无法解析的引用", map[string]any{"$ref": "#/$defs/missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schemas := cachedToolSchemas(func() []*mcp.Tool {
				return []*mcp.Tool{{Name: "synthetic", InputSchema: tc.schema}}
			})
			for range 2 {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("内部定义错误没有阻止工具目录初始化")
						}
					}()
					schemas()
				}()
			}
		})
	}
}
