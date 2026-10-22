package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func dictionaryValidationEntries(count int, long bool) []DictionaryEntry {
	entries := make([]DictionaryEntry, count)
	for i := range entries {
		entries[i] = DictionaryEntry{Code: fmt.Sprintf("code%d", i), Word: fmt.Sprintf("合成词%d", i), Weight: int64(i)}
		if long {
			entries[i].Code = strings.Repeat("x", 128)
			entries[i].Word = strings.Repeat("字", 200)
		}
	}
	return entries
}

func TestDictionaryValidationBatchAllocationBudget(t *testing.T) {
	service := &Service{}
	for _, count := range []int{1, 50} {
		for _, long := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/long=%v", count, long), func(t *testing.T) {
				entries := dictionaryValidationEntries(count, long)
				var err error
				allocations := testing.AllocsPerRun(100, func() {
					_, err = service.validateDictionary(t.Context(), "english", entries)
				})
				if !errors.Is(err, engine.ErrUnavailable) {
					t.Fatal(err)
				}
				if allocations > 8 {
					t.Fatalf("%d 条词库校验准备分配 %.0f 次，预算 8 次", count, allocations)
				}
			})
		}
	}
	for _, at := range []int{0, 49} {
		t.Run(fmt.Sprintf("invalid/%d", at), func(t *testing.T) {
			entries := dictionaryValidationEntries(50, true)
			entries[at].Weight = -1
			var err error
			allocations := testing.AllocsPerRun(100, func() {
				_, err = service.validateDictionary(t.Context(), "english", entries)
			})
			if !errors.Is(err, engine.ErrInvalid) {
				t.Fatal(err)
			}
			if allocations != 0 {
				t.Fatalf("无效批次准备分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

// 旧谓词用于差分检查，包含两个字段边界拼成完整 UTF-8 编码的旧行为。
func previousDictionaryValidationInvalid(kind string, e DictionaryEntry) bool {
	return e.Weight < 0 || e.Word == "" || len(e.Word) > 2048 || len(e.Code) > 512 || !utf8.ValidString(e.Word+e.Code) || strings.ContainsAny(e.Word+e.Code, "\x00\t\r\n") || strings.TrimSpace(e.Word) == "" || (kind == "quick" && len(utf16.Encode([]rune(e.Word))) > 199)
}

func TestDictionaryValidationBatchCompatibility(t *testing.T) {
	service := &Service{}
	entries := []DictionaryEntry{
		{Word: "你好", Code: "nihao", Weight: 1},
		{Word: " ", Code: "a"},
		{Word: "", Code: "a"},
		{Word: "合成", Weight: -1},
		{Word: "a\tb"},
		{Word: "a", Code: "b\n"},
		{Word: "a\x00"},
		{Word: "a", Code: "\r"},
		{Word: "\xff", Code: "a"},
		{Word: "a", Code: "\xff"},
		{Word: "a\xe4", Code: "\xb8\xad"},
		{Word: strings.Repeat("a", 2048), Code: strings.Repeat("x", 512)},
		{Word: strings.Repeat("a", 2049)},
		{Word: "a", Code: strings.Repeat("x", 513)},
		{Word: strings.Repeat("😀", 99) + "a"},
		{Word: strings.Repeat("😀", 100)},
	}
	random := rand.New(rand.NewSource(704))
	for range 1000 {
		word, code := make([]byte, random.Intn(24)), make([]byte, random.Intn(16))
		random.Read(word)
		random.Read(code)
		entries = append(entries, DictionaryEntry{Word: string(word), Code: string(code), Weight: int64(random.Intn(4) - 1)})
	}
	for _, kind := range dictionaryKinds {
		for _, entry := range entries {
			want := engine.ErrUnavailable
			if previousDictionaryValidationInvalid(kind, entry) {
				want = engine.ErrInvalid
			}
			got, err := service.validateDictionary(t.Context(), kind, []DictionaryEntry{entry})
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("%s 词=%q 编码=%q 权重=%d：结果=%v，错误=%v，期望=%v", kind, entry.Word, entry.Code, entry.Weight, got, err, want)
			}
		}
	}
	// 后续批次的无效词条不能抢先覆盖前一批次的引擎错误。
	laterInvalid := dictionaryValidationEntries(51, false)
	laterInvalid[50].Weight = -1
	if got, err := service.validateDictionary(t.Context(), "english", laterInvalid); got != nil || !errors.Is(err, engine.ErrUnavailable) {
		t.Fatalf("跨批次错误优先级改变：%v/%v", got, err)
	}

	for _, entries := range [][]DictionaryEntry{nil, {}} {
		got, err := service.validateDictionary(t.Context(), "english", entries)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("空输入行为改变：%v/%v", got, err)
		}
	}
}

// 保留原批次准备流程作为配对基准，不计入真实引擎或数据库耗时。
func previousDictionaryValidation(a *Service, ctx context.Context, kind string, entries []DictionaryEntry) ([]DictionaryEntry, error) {
	result := []DictionaryEntry{}
	for start := 0; start < len(entries); start += 50 {
		end := min(start+50, len(entries))
		batch := []map[string]any{}
		for _, e := range entries[start:end] {
			if e.Weight < 0 || e.Word == "" || len(e.Word) > 2048 || len(e.Code) > 512 || !utf8.ValidString(e.Word+e.Code) || strings.ContainsAny(e.Word+e.Code, "\x00\t\r\n") || strings.TrimSpace(e.Word) == "" || (kind == "quick" && len(utf16.Encode([]rune(e.Word))) > 199) {
				return nil, engine.ErrInvalid
			}
			batch = append(batch, map[string]any{"kind": kind, "code": e.Code, "text": e.Word, "weight": e.Weight})
		}
		raw, err := a.engine.Query(ctx, map[string]any{"operation": "validate_dictionary_batch", "entries": batch})
		if err != nil {
			return nil, err
		}
		var response struct {
			Entries []DictionaryEntry `json:"entries"`
		}
		if json.Unmarshal(raw, &response) != nil || len(response.Entries) != len(batch) {
			return nil, engine.ErrFailure
		}
		result = append(result, response.Entries...)
	}
	return result, nil
}

// 引擎未配置时仍执行真实的批次校验及请求准备，排除子进程与数据库开销。
func BenchmarkDictionaryValidationPrepare(b *testing.B) {
	service := &Service{}
	for _, count := range []int{1, 50} {
		for _, long := range []bool{false, true} {
			for _, previous := range []bool{true, false} {
				name := "Current"
				if previous {
					name = "Previous"
				}
				b.Run(fmt.Sprintf("%d/long=%v/%s", count, long, name), func(b *testing.B) {
					entries := dictionaryValidationEntries(count, long)
					run := service.validateDictionary
					if previous {
						run = func(ctx context.Context, kind string, entries []DictionaryEntry) ([]DictionaryEntry, error) {
							return previousDictionaryValidation(service, ctx, kind, entries)
						}
					}
					b.ReportAllocs()
					for b.Loop() {
						_, err := run(b.Context(), "english", entries)
						if !errors.Is(err, engine.ErrUnavailable) {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
