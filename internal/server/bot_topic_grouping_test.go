package server

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func previousQQCountTopics(topics []string) []qqImprovementCounted {
	counts, first := map[string]int{}, map[string]string{}
	var order []string
	for _, topic := range topics {
		key := strings.ToLower(strings.Join(strings.Fields(topic), ""))
		if key == "" {
			key, topic = "（没有概括）", "（没有概括）"
		}
		if _, ok := counts[key]; !ok {
			order, first[key] = append(order, key), topic
		}
		counts[key]++
	}
	out := make([]qqImprovementCounted, 0, len(order))
	for _, key := range order {
		out = append(out, qqImprovementCounted{first[key], counts[key]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

func TestQQTopicGroupingOrderAndOriginalText(t *testing.T) {
	topics := []string{" Windows\t五笔 ", "鸿蒙安装", "WINDOWS五笔", " ", "鸿蒙 安装", "（没有概括）", "单次话题"}
	want := []qqImprovementCounted{{" Windows\t五笔 ", 2}, {"鸿蒙安装", 2}, {"（没有概括）", 2}, {"单次话题", 1}}
	if got := qqCountTopics(topics); !reflect.DeepEqual(got, want) {
		t.Fatalf("必须按次数降序、并列时按首次出现顺序，保留首条原文：结果=%#v，期望=%#v", got, want)
	}
}

func TestQQTopicGroupingMatchesPrevious(t *testing.T) {
	inputs := [][]string{nil, {}, {""}, {"\t\u0085\u3000", "（没有概括）"}, {" （没有概括） ", " "}, {"\xe6 \x8f\x90", "提", "\xff", " \xff "}, {"İ", "i", "K", "K", "Σ", "σ"}}
	random := rand.New(rand.NewSource(668))
	units := []string{"五笔", "切换", "Windows", "WINDOWS", "鸿蒙", "\n", " ", "\t", "\u0085", "\u3000", "\xff", "\xe6", "\x8f", "\x90", "（没有概括）"}
	for range 500 {
		var topics []string
		for range random.Intn(150) {
			if len(topics) > 0 && random.Intn(3) == 0 {
				topics = append(topics, topics[random.Intn(len(topics))])
				continue
			}
			var topic strings.Builder
			for range random.Intn(10) {
				topic.WriteString(units[random.Intn(len(units))])
			}
			topics = append(topics, topic.String())
		}
		inputs = append(inputs, topics)
	}
	for _, topics := range inputs {
		if got, want := qqCountTopics(topics), previousQQCountTopics(topics); !reflect.DeepEqual(got, want) {
			t.Fatalf("话题归并结果变化：输入=%q，结果=%#v，期望=%#v", topics, got, want)
		}
	}
}

func qqTopicGroupingFixture(count, unique int) []string {
	topics := make([]string, count)
	for i := range topics {
		topics[i] = fmt.Sprintf("话题%04d", i%unique)
	}
	return topics
}

func TestQQTopicGroupingSampleRunsMatchPrevious(t *testing.T) {
	units := []string{" \t\u3000", "（没有概括）", " Windows 五笔 ", "windows五笔", "\xe6 \x8f\x90", "提"}
	for _, count := range []int{256, 257, 258, 2048} {
		topics := make([]string, count)
		for i := range topics {
			topics[i] = units[(i/17)%len(units)]
		}
		if got, want := qqCountTopics(topics), previousQQCountTopics(topics); !reflect.DeepEqual(got, want) {
			t.Fatalf("采样边界归并变化：条数=%d，结果=%#v，期望=%#v", count, got, want)
		}
	}
}

func TestQQTopicGroupingSampleOrderMatchesPrevious(t *testing.T) {
	for _, count := range []int{257, 258, 2048} {
		for _, unique := range []int{1, 2, 64, 256, 257} {
			t.Run(fmt.Sprintf("条数%d/不同话题%d", count, unique), func(t *testing.T) {
				topics := qqTopicGroupingFixture(count, unique)
				slices.Reverse(topics)
				for i := range topics {
					if i%3 == 0 {
						topics[i] = " \t" + topics[i] + "\u3000"
					}
				}
				original := slices.Clone(topics)
				if got, want := qqCountTopics(topics), previousQQCountTopics(topics); !reflect.DeepEqual(got, want) {
					t.Fatalf("采样顺序影响了归并和首次出现顺序：结果=%#v，期望=%#v", got, want)
				}
				if !slices.Equal(topics, original) {
					t.Fatal("话题归并修改了输入顺序或原文")
				}
			})
		}
	}
}

func TestQQTopicGroupingAllocationBudget(t *testing.T) {
	topics := qqTopicGroupingFixture(2048, 2048)
	var got []qqImprovementCounted
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqCountTopics(topics)
		}
	})
	if !reflect.DeepEqual(got, previousQQCountTopics(topics)) {
		t.Fatal("大量不同话题的归并结果变化")
	}
	if result.AllocedBytesPerOp() > 320<<10 {
		t.Fatalf("话题元数据不应重复保存在多个映射和切片中：%s", result.MemString())
	}
}

func TestQQTopicGroupingPartialDuplicateAllocationBudget(t *testing.T) {
	topics := qqTopicGroupingFixture(2048, 64)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_ = qqCountTopics(topics)
		}
	})
	if result.AllocedBytesPerOp() > 64<<10 {
		t.Fatalf("部分重复话题归并分配超过预算：%s", result.MemString())
	}
}

func TestQQTopicGroupingPrefixDuplicateAllocationBudget(t *testing.T) {
	topics := make([]string, 2048)
	topics[0], topics[1] = "重复话题", "重复话题"
	for i := 2; i < len(topics); i++ {
		topics[i] = fmt.Sprintf("新话题%04d", i)
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_ = qqCountTopics(topics)
		}
	})
	if result.AllocedBytesPerOp() > 320<<10 {
		t.Fatalf("前缀重复后大量不同话题归并分配超过预算：%s", result.MemString())
	}
}

func BenchmarkQQTopicGrouping(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func([]string) []qqImprovementCounted
	}{{"previous", previousQQCountTopics}, {"current", qqCountTopics}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, tc := range []struct {
				name   string
				topics []string
			}{
				{"少量话题", []string{"Windows 五笔", "windows五笔", "鸿蒙安装", "鸿蒙 安装"}},
				{"大量不同话题", qqTopicGroupingFixture(2048, 2048)},
				{"大量重复话题", qqTopicGroupingFixture(2048, 1)},
				{"部分重复话题", qqTopicGroupingFixture(2048, 64)},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.run(tc.topics)
					}
				})
			}
		})
	}
}
