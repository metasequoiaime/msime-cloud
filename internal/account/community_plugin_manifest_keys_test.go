package account

import "testing"

var pluginManifestKeysSink bool

func TestPluginManifestKeysCompatibility(t *testing.T) {
	common := []string{"schema_version", "kind", "id", "name", "version", "license", "author", "description", "permissions"}
	for _, tc := range []struct {
		kind string
		keys []string
	}{
		{"sound", []string{"mode", "sounds", "sequence"}},
		{"music", []string{"music"}},
		{"command_table", []string{"commands"}},
		{"effect", []string{"effect"}},
		{"helpcode", []string{"helpcode"}},
		{"symbol_set", []string{"groups"}},
		{"phrase_table", []string{"phrases"}},
		{"wordbook", []string{"wordbook"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			table := map[string]any{}
			for _, key := range common {
				if !pluginManifestKeys(map[string]any{key: true}, tc.kind) {
					t.Fatalf("拒绝公共键 %q", key)
				}
				table[key] = true
			}
			for _, key := range tc.keys {
				if !pluginManifestKeys(map[string]any{key: true}, tc.kind) {
					t.Fatalf("拒绝类型键 %q", key)
				}
				table[key] = true
			}
			if !pluginManifestKeys(table, tc.kind) || !pluginManifestKeys(nil, tc.kind) {
				t.Fatal("拒绝合法键组合或空表")
			}
			for _, key := range []string{"", "Kind", "Name", "name ", "unknown", "commands", "effect", "groups", "sounds", "music", "helpcode", "phrases", "wordbook"} {
				if _, allowed := table[key]; allowed {
					continue
				}
				table[key] = true
				if pluginManifestKeys(table, tc.kind) {
					t.Fatalf("接受未知或其他类型的键 %q", key)
				}
				delete(table, key)
			}
		})
	}
}

func TestPluginManifestKeysAllocationBudget(t *testing.T) {
	for _, kind := range pluginKinds {
		t.Run(kind, func(t *testing.T) {
			table := map[string]any{"schema_version": int64(1), "kind": kind, "id": "synthetic-pack", "name": "合成插件", "version": "1", "license": "MIT"}
			for _, key := range pluginKindKeys[kind] {
				table[key] = nil
			}
			allocs := testing.AllocsPerRun(100, func() { pluginManifestKeysSink = pluginManifestKeys(table, kind) })
			if !pluginManifestKeysSink {
				t.Fatal("合法清单键被拒绝")
			}
			if allocs != 0 {
				t.Fatalf("校验清单键分配 %.0f 次，期望零分配", allocs)
			}
		})
	}
}

func BenchmarkPluginManifestKeys(b *testing.B) {
	for _, kind := range pluginKinds {
		b.Run(kind, func(b *testing.B) {
			table := map[string]any{}
			for _, key := range pluginCommonKeys {
				table[key] = nil
			}
			for _, key := range pluginKindKeys[kind] {
				table[key] = nil
			}
			b.ReportAllocs()
			for b.Loop() {
				pluginManifestKeysSink = pluginManifestKeys(table, kind)
				if !pluginManifestKeysSink {
					b.Fatal("合法清单键被拒绝")
				}
			}
		})
	}
}
