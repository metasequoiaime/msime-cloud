package server

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func previousQQCountTopicKeys(topics []string) []qqImprovementCounted {
	indexes := map[string]int{}
	out := []qqImprovementCounted{}
	for _, topic := range topics {
		key := strings.ToLower(strings.Join(strings.Fields(topic), ""))
		if key == "" {
			key, topic = "（没有概括）", "（没有概括）"
		}
		if index, ok := indexes[key]; ok {
			out[index].count++
		} else {
			indexes[key] = len(out)
			out = append(out, qqImprovementCounted{topic, 1})
		}
	}
	slices.SortStableFunc(out, func(a, b qqImprovementCounted) int { return cmp.Compare(b.count, a.count) })
	return out
}

func TestQQTopicKeysAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		topics []string
		budget int64
	}{
		{"重复短话题", qqTopicGroupingFixture(2048, 1), 4 << 10},
		{"长度上限内的密集空白", slices.Repeat([]string{strings.Repeat("文\u3000 ", 60)}, 32), 16 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []qqImprovementCounted
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqCountTopics(tc.topics)
				}
			})
			if !reflect.DeepEqual(got, previousQQCountTopicKeys(tc.topics)) || result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("归并结果变化或话题键仍建立字段切片：分配=%s，预算=%d", result.MemString(), tc.budget)
			}
		})
	}
}

func TestQQTopicKeysKeepExistingDraftQuota(t *testing.T) {
	for _, occupied := range []bool{true, false} {
		name := "额度空闲"
		if occupied {
			name = "旧键额度已占用"
		}
		t.Run(name, func(t *testing.T) {
			s, fake, limiter := qqServer(t)
			topic := " W I N D O W S\u3000五笔 "
			s.qqImprovements = &memoryImprovements{events: []account.QQImprovementEvent{
				{Kind: "unknown", Topic: topic, Detail: "docs"},
				{Kind: "unknown", Topic: "windows五笔", Detail: "docs"},
			}}
			ctx := context.Background()
			oldKey := strings.ToLower(strings.Join(strings.Fields(topic), ""))
			if occupied {
				if err := s.limits.RateLimit(ctx, "qq-bot-docs-draft", oldKey, 1, qqDocsDraftWindow); err != nil {
					t.Fatal(err)
				}
			}
			fake.answer = `{"type":"unknown","topic":"Windows 五笔"}`
			from := time.Date(2026, 10, 5, 0, 0, 0, 0, qqStatsZone)
			if pulls := s.qqDraftDocsForGaps(ctx, from, from.AddDate(0, 0, 7)); len(pulls) != 0 {
				t.Fatal("答不上时不能起草 PR", pulls)
			}
			if occupied {
				if len(fake.chatCalls) != 0 || len(limiter.released) != 0 {
					t.Fatal("旧键已占用的草稿额度必须继续生效", fake.chatCalls, limiter.released)
				}
			} else {
				if len(fake.chatCalls) == 0 || !slices.Equal(limiter.released, []string{"qq-bot-docs-draft|" + oldKey}) {
					t.Fatal("起草未答上时必须按同一个键退回额度", fake.chatCalls, limiter.released)
				}
				if err := s.limits.RateLimit(ctx, "qq-bot-docs-draft", oldKey, 1, qqDocsDraftWindow); err != nil {
					t.Fatalf("退回旧键额度后必须能重新尝试起草：%v", err)
				}
			}
		})
	}
}

func BenchmarkQQTopicKeys(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func([]string) []qqImprovementCounted
	}{{"previous", previousQQCountTopicKeys}, {"current", qqCountTopics}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, tc := range []struct {
				name   string
				topics []string
			}{
				{"少量话题", []string{"Windows 五笔", "windows五笔", "鸿蒙安装", "鸿蒙 安装"}},
				{"大量不同话题", qqTopicGroupingFixture(2048, 2048)},
				{"大量重复话题", qqTopicGroupingFixture(2048, 1)},
				{"部分重复话题", qqTopicGroupingFixture(2048, 64)},
				{"长度上限内的密集空白", slices.Repeat([]string{strings.Repeat("文\u3000 ", 60)}, 32)},
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
