package server

import (
	"context"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 固定原正则及历史查找顺序，防止解析优化改变合并目标。
var previousQQDraftRef = regexp.MustCompile(`(?:编号 |已补充到 |已记下（)D([0-9]{1,9})`)

func previousQQDraftReference(history []account.QQTurn) int64 {
	var id int64
	for i := len(history) - 1; i >= 0 && id == 0; i-- {
		if refs := previousQQDraftRef.FindAllStringSubmatch(history[i].Answer, -1); refs != nil {
			id, _ = strconv.ParseInt(refs[len(refs)-1][1], 10, 64)
		}
	}
	return id
}

// 只记录合并流程实际查询的编号；返回不存在，避免进入后续模型调用。
type qqDraftReferenceStore struct {
	memoryDrafts
	id    int64
	calls int
}

func (s *qqDraftReferenceStore) QQDraft(_ context.Context, id int64) (account.QQDraft, error) {
	s.id, s.calls = id, s.calls+1
	return account.QQDraft{}, account.ErrQQDraftNotFound
}

func TestQQMergeDraftReferenceCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
		want    int64
	}{
		{"Empty", nil, 0},
		{"Prefixes", []string{"编号 D1，已补充到 D2。已记下（D3）"}, 3},
		{"LatestTurn", []string{"已记下（D7）", "no reference", "编号 D8"}, 8},
		{"NineDigits", []string{"已补充到 D1234567890"}, 123456789},
		{"LeadingZeros", []string{"编号 D000000012"}, 12},
		{"ZeroLastMatch", []string{"编号 D7", "编号 D8，已记下（D0）"}, 7},
		{"ZeroNineDigits", []string{"编号 D7", "编号 D0000000001"}, 7},
		{"InvalidTrailingCandidate", []string{"编号 D7，已补充到 Dx 编号 D"}, 7},
		{"ExactSyntax", []string{"编号 d1 编号  D2 编号\tD3 已记下(D4) 编号 D１２"}, 0},
		{"InvalidUTF8", []string{"\xff编号 D7\xfe已记下（D9\xff"}, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := make([]account.QQTurn, len(tc.answers))
			for i, answer := range tc.answers {
				history[i].Answer = answer
			}
			assertQQDraftReference(t, history, tc.want)
		})
	}
	random := rand.New(rand.NewSource(691))
	atoms := []string{"水杉", "D", "d", "编号 ", "已补充到 ", "已记下（", "编号\t", "已记下(", "0", "1", "9999999999", "0000000001", "１２", "）", "\n", "\xff", "\xfe"}
	for range 1000 {
		history := make([]account.QQTurn, random.Intn(5))
		for i := range history {
			var text strings.Builder
			for range random.Intn(80) {
				text.WriteString(atoms[random.Intn(len(atoms))])
			}
			history[i].Answer = text.String()
		}
		assertQQDraftReference(t, history, previousQQDraftReference(history))
	}
}

func assertQQDraftReference(t *testing.T, history []account.QQTurn, want int64) {
	t.Helper()
	store := &qqDraftReferenceStore{}
	s := &Server{qqDrafts: store}
	note, asked, ok := s.qqMergeDraft(context.Background(), "synthetic", "", qqReport{}, history)
	wantCalls := 0
	if want != 0 {
		wantCalls = 1
	}
	if note != "" || asked || ok || store.id != want || store.calls != wantCalls {
		t.Fatalf("历史=%+v，查询编号=%d/次数=%d，期望编号=%d/次数=%d，回复=%q/%v/%v", history, store.id, store.calls, want, wantCalls, note, asked, ok)
	}
}

func TestQQMergeDraftReferenceAvoidsMatchCopies(t *testing.T) {
	s := &Server{qqDrafts: &memoryDrafts{}}
	history := []account.QQTurn{{Answer: strings.Repeat("已记下（D12），管理员确认后转给开发者。\n", 20)}}
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() {
		if note, asked, ok := s.qqMergeDraft(ctx, "synthetic", "", qqReport{}, history); note != "" || asked || ok {
			t.Fatalf("不存在的草稿得到回复=%q/%v/%v", note, asked, ok)
		}
	})
	if allocs != 0 {
		t.Fatalf("查找末个草稿编号分配了 %.0f 次，期望不复制匹配结果", allocs)
	}
}

var qqDraftReferenceResult int64

func BenchmarkQQDraftReference(b *testing.B) {
	for _, tc := range []struct{ name, answer string }{
		{"NoReference", strings.Repeat("合成答复内容。", 100)},
		{"Single", strings.Repeat("合成答复内容。", 100) + "已记下（D12），管理员确认后转给开发者。"},
		{"Multiple", strings.Repeat("已记下（D12），管理员确认后转给开发者。\n", 20)},
		{"TrailingInvalid", "编号 D12。" + strings.Repeat("已补充到 Dx，编号 D。", 50)},
		{"UnrelatedD", strings.Repeat("DirectDownload D123 draft D0\n", 100)},
		{"Zero", "编号 D12。已记下（D0），管理员确认后转给开发者。"},
	} {
		previous := func(answer string) int64 {
			return previousQQDraftReference([]account.QQTurn{{Answer: answer}})
		}
		for _, impl := range []struct {
			name string
			find func(string) int64
		}{{"Previous", previous}, {"Current", qqLastDraftID}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqDraftReferenceResult = impl.find(tc.answer)
				}
			})
		}
	}
}
