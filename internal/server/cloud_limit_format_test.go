package server

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func previousJSONNumber(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestJSONNumberIntegerCompatibility(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	numbers := []int{-maximum - 1, -maximum, -100, -10, -1, 0, 1, 9, 10, 99, 100, 999, 1000, maximum}
	rng := rand.New(rand.NewSource(103))
	for i := 0; i < 1000; i++ {
		n := rng.Int()
		numbers = append(numbers, n, -n)
	}
	for _, n := range numbers {
		if got, want := jsonNumber(n), previousJSONNumber(n); got != want {
			t.Fatalf("整数十进制格式变化：输入=%d，结果=%q，期望=%q", n, got, want)
		}
	}
}

func TestCloudLimitNumberAllocationBudget(t *testing.T) {
	for n := 1; n <= contract.CloudCandidates; n++ {
		var got string
		allocations := testing.AllocsPerRun(50, func() { got = jsonNumber(n) })
		if got != previousJSONNumber(n) {
			t.Fatalf("候选数量格式变化：输入=%d，结果=%q", n, got)
		}
		if allocations != 0 {
			t.Fatalf("候选数量 %d 转成十进制文本分配 %.0f 次，期望无需 JSON 中间缓冲区", n, allocations)
		}
	}
}

func BenchmarkCloudLimitNumber(b *testing.B) {
	for _, n := range []int{1, 5, contract.CloudCandidates, int(^uint(0) >> 1)} {
		for _, impl := range []struct {
			name   string
			format func(int) string
		}{{"previous", previousJSONNumber}, {"current", jsonNumber}} {
			b.Run(previousJSONNumber(n)+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got string
				for b.Loop() {
					got = impl.format(n)
				}
				if got != previousJSONNumber(n) {
					b.Fatal("候选数量格式变化")
				}
			})
		}
	}
}
