package publictools

import (
	"strings"
	"sync"
	"testing"
)

func TestHasMatchesDefinitions(t *testing.T) {
	definitions := Definitions()
	for _, tool := range definitions {
		for _, name := range []string{tool.Name, strings.ToUpper(tool.Name), tool.Name + " ", tool.Name + "_unknown"} {
			if got, want := Has(name), name == tool.Name; got != want {
				t.Fatalf("工具名=%q，存在=%t，期望=%t", name, got, want)
			}
		}
	}
	for _, name := range []string{"", "private_tool", "\xff"} {
		if Has(name) {
			t.Fatalf("接受了未知工具名 %q", name)
		}
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 100 {
				for _, tool := range definitions {
					if !Has(tool.Name) || Has("private_tool") {
						t.Error("并发查询的工具名称发生变化")
						return
					}
				}
			}
		})
	}
	workers.Wait()
}

func TestHasIndependentOfReturnedDefinitions(t *testing.T) {
	definitions := Definitions()
	name := definitions[0].Name
	if !Has(name) {
		t.Fatal("未找到定义中的工具")
	}
	definitions[0].Name = "synthetic_mutated_tool"
	definitions[0].InputSchema = nil
	if !Has(name) || Has(definitions[0].Name) {
		t.Fatal("修改返回的定义影响了工具名称查询")
	}
	if fresh := Definitions(); fresh[0].Name != name || fresh[0].InputSchema == nil {
		t.Fatal("工具定义未保持独立")
	}
}

func TestHasAllocationBudget(t *testing.T) {
	for _, tool := range Definitions() {
		t.Run(tool.Name, func(t *testing.T) {
			var got bool
			allocations := testing.AllocsPerRun(100, func() { got = Has(tool.Name) })
			if !got || allocations != 0 {
				t.Fatalf("工具名查询结果=%t，每次分配=%g，期望零分配", got, allocations)
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		var got bool
		allocations := testing.AllocsPerRun(100, func() { got = Has("private_tool") })
		if got || allocations != 0 {
			t.Fatalf("未知工具查询结果=%t，每次分配=%g，期望零分配", got, allocations)
		}
	})
}

func BenchmarkHas(b *testing.B) {
	for _, name := range []string{"lookup_dictionary", "browse_input_catalog", "query_public_data", "private_tool"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = Has(name)
			}
		})
	}
}
