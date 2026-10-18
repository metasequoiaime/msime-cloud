package skins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestBuiltinLoadAllocationBudget(t *testing.T) {
	for _, id := range builtinIDs {
		t.Run(id, func(t *testing.T) {
			var got Package
			allocations := testing.AllocsPerRun(5, func() {
				var err error
				got, err = Load("", id)
				if err != nil {
					t.Fatal(err)
				}
			})
			if got.ID != id || len(got.Resources) != 4 {
				t.Fatalf("内置皮肤元数据不完整：%+v", got)
			}
			// 允许独立结果切片的分配，排除再次遍历、读取和哈希全部资源。
			if allocations > 8 {
				t.Fatalf("重复加载内置皮肤分配 %.0f 次，超过 8 次预算", allocations)
			}
		})
	}
}

func TestBuiltinLoadReturnsIndependentSlices(t *testing.T) {
	for _, id := range builtinIDs {
		t.Run(id, func(t *testing.T) {
			p, err := Load("", id)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			p.Supports.Layouts[0] = "changed-layout"
			p.Supports.Themes[0] = "changed-theme"
			p.Resources[0].Path = "changed-resource.css"
			p.Name = "改过的名字"
			p, err = Load("missing-root", id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(p)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("修改返回值影响了后续内置皮肤：结果=%s，预期=%s，错误=%v", got, want, err)
			}
		})
	}
}

func TestBuiltinLoadConcurrentResultsAreIndependent(t *testing.T) {
	var group sync.WaitGroup
	errors := make(chan error, len(builtinIDs)*16)
	for _, id := range builtinIDs {
		p, err := Load("", id)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		for range 16 {
			group.Add(1)
			go func() {
				defer group.Done()
				p, err := Load("", id)
				if err != nil {
					errors <- err
					return
				}
				p.Supports.Layouts[0] = "changed-layout"
				p.Supports.Themes[0] = "changed-theme"
				p.Resources[0].SHA256 = "changed-digest"
				fresh, err := Load("", id)
				if err != nil {
					errors <- err
					return
				}
				got, err := json.Marshal(fresh)
				if err != nil || !bytes.Equal(got, want) {
					errors <- fmt.Errorf("并发加载的内置皮肤受到其他结果影响：%s", id)
				}
			}()
		}
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func BenchmarkBuiltinMetadata(b *testing.B) {
	b.Run("load", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Load("", "fluent"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("catalog", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := Catalog("", "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("resource", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := ReadResource("", "fluent", "horizontal_light.css"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
