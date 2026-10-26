package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func previousQQSyncFAQSources(sources, oldFAQ, newFAQ string) (string, bool) {
	var record struct {
		Files map[string]struct {
			SourceSha256 string `json:"sourceSha256"`
		} `json:"files"`
	}
	oldSum, newSum := sha256.Sum256([]byte(oldFAQ)), sha256.Sum256([]byte(newFAQ))
	oldHash, newHash := hex.EncodeToString(oldSum[:]), hex.EncodeToString(newSum[:])
	if json.Unmarshal([]byte(sources), &record) != nil || record.Files["faq.md"].SourceSha256 != oldHash || strings.Count(sources, `"`+oldHash+`"`) != 1 {
		return "", false
	}
	return strings.Replace(sources, `"`+oldHash+`"`, `"`+newHash+`"`, 1), true
}

func TestQQSyncFAQSourcesMatchesPrevious(t *testing.T) {
	documents := []string{"", "合成 FAQ\n", "\r\n\x00\xff\xe4\xb8", strings.Repeat("合成正文\n", 300)}
	random := rand.New(rand.NewSource(648))
	for range 100 {
		raw := make([]byte, random.Intn(2048))
		if _, err := random.Read(raw); err != nil {
			t.Fatal(err)
		}
		documents = append(documents, string(raw))
	}
	for i, oldFAQ := range documents {
		newFAQ := documents[(i+1)%len(documents)]
		sum := sha256.Sum256([]byte(oldFAQ))
		hash := hex.EncodeToString(sum[:])
		escaped := `\u00` + fmt.Sprintf("%02x", hash[0]) + hash[1:]
		for _, sources := range []string{
			qqTestFAQSources(oldFAQ),
			" \r\n" + qqTestFAQSources(oldFAQ) + "\t\n",
			qqTestFAQSources(newFAQ),
			"not json", "", "null", "[]", `{}`, `{"files":null}`, `{"files":{}}`,
			`{"files":{"faq.md":{"sourceSha256":null}}}`,
			`{"files":{"faq.md":{"sourceSha256":123}}}`,
			fmt.Sprintf(`{"files":{"faq.md":{"sourceSha256":"%s"}},"other":"%s"}`, hash, hash),
			fmt.Sprintf(`{"files":{"faq.md":{"sourceSha256":"%s"}},"other":"%s"}`, escaped, hash),
			fmt.Sprintf(`{"files":{"faq.md":{"sourceSha256":"%s"}}}`, escaped),
			fmt.Sprintf(`{"files":{"faq.md":{"sourceSha256":"%s"}}}`, strings.ToUpper(hash)),
			fmt.Sprintf(`{"files":{"faq.md":{"sourceSha256":"wrong","sourceSha256":"%s"}}}`, hash),
			fmt.Sprintf(`{"files":{"faq.md":{"sourceSha256":"%s","sourceSha256":"wrong"}}}`, hash),
		} {
			got, ok := qqSyncFAQSources(sources, oldFAQ, newFAQ)
			want, found := previousQQSyncFAQSources(sources, oldFAQ, newFAQ)
			if got != want || ok != found {
				t.Fatalf("原文序号=%d，记录=%q，结果=%q/%t，期望=%q/%t", i, sources, got, ok, want, found)
			}
		}
	}
}

func TestQQSyncFAQSourcesAllocationBudget(t *testing.T) {
	oldFAQ := strings.Repeat("合成旧正文\n", 1<<17)
	newFAQ := strings.Repeat("合成新正文\n", 1<<17)
	for _, tc := range []struct {
		name, sources string
	}{
		{"同步", qqTestFAQSources(oldFAQ)},
		{"格式错误", "not json"},
		{"原文不同步", qqTestFAQSources("不同步的合成正文")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, found := previousQQSyncFAQSources(tc.sources, oldFAQ, newFAQ)
			var got string
			var ok bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, ok = qqSyncFAQSources(tc.sources, oldFAQ, newFAQ)
				}
			})
			if got != want || ok != found || result.AllocedBytesPerOp() > 1700 {
				t.Fatalf("输出一致=%t，同步=%t/%t，分配=%s，期望不超过 1700 字节", got == want, ok, found, result.MemString())
			}
		})
	}
}

func BenchmarkQQSyncFAQSources(b *testing.B) {
	oldFAQ := strings.Repeat("合成旧正文\n", 1<<17)
	newFAQ := strings.Repeat("合成新正文\n", 1<<17)
	for _, implementation := range []struct {
		name string
		sync func(string, string, string) (string, bool)
	}{
		{"previous", previousQQSyncFAQSources},
		{"current", qqSyncFAQSources},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			for _, tc := range []struct {
				name, sources string
			}{
				{"同步", qqTestFAQSources(oldFAQ)},
				{"格式错误", "not json"},
				{"原文不同步", qqTestFAQSources("不同步的合成正文")},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_, _ = implementation.sync(tc.sources, oldFAQ, newFAQ)
					}
				})
			}
		})
	}
}
