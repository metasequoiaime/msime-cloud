package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/docs"
)

// previousAdminGuide 保留按行拆分整篇文档的实现，验证返回内容及顺序不变。
func previousAdminGuide(path string) []string {
	base, _, _ := strings.Cut(path, "{")
	base = strings.TrimSuffix(base, "/")
	var lines []string
	for _, line := range strings.Split(docs.Admin, "\n") {
		if mentions(line, base) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) == 0 {
		lines = []string{"docs/admin.md does not name " + base + "; see the admin-web client in admin-web/src/api"}
	}
	return lines
}

func TestAdminGuideCatalogCompatibility(t *testing.T) {
	operations, err := catalog()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/api/synthetic/missing", "/api/users-archive", "/api/users/", "/api/users/{id}"}
	for _, op := range operations {
		if op.Auth == authAdmin {
			paths = append(paths, op.Path)
		}
	}
	for _, path := range paths {
		if got, want := adminGuide(path), previousAdminGuide(path); !reflect.DeepEqual(got, want) {
			t.Fatalf("接口说明变化：路径=%q，实际=%q，期望=%q", path, got, want)
		}
	}
}

var adminGuideCases = []struct{ name, path string }{
	{"操作说明", "/api/actions"},
	{"参数路径", "/api/users/{id}"},
	{"未收录接口", "/api/synthetic/missing"},
}

func TestAdminGuideAllocationBudget(t *testing.T) {
	for _, tc := range adminGuideCases {
		t.Run(tc.name, func(t *testing.T) {
			want := previousAdminGuide(tc.path)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if got := adminGuide(tc.path); len(got) != len(want) || got[0] != want[0] {
						b.Fatal("接口说明不正确")
					}
				}
			})
			// 允许结果切片增长，但不能为整篇文档创建数百个行引用。
			if got := result.AllocedBytesPerOp(); got > 4096 {
				t.Fatalf("接口说明查找分配 %d B，预算为 4096 B", got)
			}
		})
	}
}

func BenchmarkAdminGuide(b *testing.B) {
	for _, tc := range adminGuideCases {
		b.Run(tc.name, func(b *testing.B) {
			want := previousAdminGuide(tc.path)
			b.ReportAllocs()
			for b.Loop() {
				if got := adminGuide(tc.path); len(got) != len(want) || got[0] != want[0] {
					b.Fatal("接口说明不正确")
				}
			}
		})
	}
}
