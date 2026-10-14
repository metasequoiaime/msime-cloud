package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

type countedSnapshotQuerier struct {
	snapshotQuerier
	calls int
}

func (q *countedSnapshotQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.calls++
	return q.snapshotQuerier.Query(ctx, sql, args...)
}

func TestDictionaryQuerySnapshotFiltersOverlay(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "query-snapshot@example.test"}).User.ID
	other := complete(t, s, Identity{"email", "query-snapshot-other@example.test"}).User.ID
	for _, kind := range dictionaryKinds {
		for i := range 2 {
			change, err := s.EditDictionary(ctx, user, kind, "", 0, &DictionaryEntry{Code: fmt.Sprintf("code%d", i), Word: fmt.Sprintf("合成词%d", i), Weight: int64(100 + i)})
			if err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				if _, err = s.EditDictionary(ctx, user, kind, change.Replacement.ID, change.Revision, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if _, err := s.EditDictionary(ctx, other, "english", "", 0, &DictionaryEntry{Code: "other", Word: "Other", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{user, other} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO user_candidate_positions(user_id,context,code,word,position) VALUES($1,'ni''hao','ni''hao','合成词',2)`, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,'english:hello','hello','Hello',3)`, owner); err != nil {
			t.Fatal(err)
		}
	}
	var full []json.RawMessage
	if err := s.StreamDictionarySnapshot(ctx, user, func(raw json.RawMessage) error { full = append(full, bytes.Clone(raw)); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(full) != 13 {
		t.Fatal("全量快照遗漏数据", len(full))
	}
	cases := []struct {
		name  string
		query map[string]any
		kind  string
	}{
		{"全拼", map[string]any{"operation": "candidates", "scheme": "pinyin"}, "pinyin"},
		{"双拼", map[string]any{"operation": "candidates", "scheme": "shuangpin"}, "pinyin"},
		{"默认方案", map[string]any{"operation": "candidates"}, "pinyin"},
		{"简拼", map[string]any{"operation": "jianpin"}, "pinyin"},
		{"五笔", map[string]any{"operation": "candidates", "scheme": "wubi"}, "wubi"},
		{"五笔98", map[string]any{"operation": "candidates", "scheme": "wubi98"}, "wubi98"},
		{"英文", map[string]any{"operation": "english"}, "english"},
		{"快捷短语", map[string]any{"operation": "quick"}, "quick"},
		{"未知操作", map[string]any{"operation": "future"}, ""},
		{"未知方案", map[string]any{"operation": "candidates", "scheme": "future"}, ""},
		{"非法方案", map[string]any{"operation": "candidates", "scheme": 1}, ""},
		{"未知词库", map[string]any{"operation": "dictionary", "kind": "future"}, ""},
		{"空查询", nil, ""},
	}
	for _, kind := range dictionaryKinds {
		cases = append(cases, struct {
			name  string
			query map[string]any
			kind  string
		}{"浏览" + kind, map[string]any{"operation": "dictionary", "kind": kind}, kind})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &countedSnapshotQuerier{snapshotQuerier: s.pool}
			var got []json.RawMessage
			if err := streamDictionaryQuerySnapshot(ctx, q, user, tc.query, func(raw json.RawMessage) error { got = append(got, bytes.Clone(raw)); return nil }); err != nil {
				t.Fatal(err)
			}
			var want []json.RawMessage
			for _, raw := range full {
				var record struct{ Previous, Replacement *DictionaryEntry }
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				entry := record.Replacement
				if entry == nil {
					entry = record.Previous
				}
				if entry == nil || tc.kind == "" || entry.Kind == tc.kind {
					want = append(want, raw)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("查询快照包含无关词库或丢失状态：得到 %d 行，期望 %d 行", len(got), len(want))
			}
			if q.calls != 1 {
				t.Fatal("查询快照必须只使用一条 SQL", q.calls)
			}
		})
	}
	sentinel := errors.New("停止发送")
	if err := streamDictionaryQuerySnapshot(ctx, s.pool, user, map[string]any{"operation": "english"}, func(json.RawMessage) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := streamDictionaryQuerySnapshot(cancelled, s.pool, user, map[string]any{"operation": "english"}, func(json.RawMessage) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDictionaryQuerySnapshotNativeParity(t *testing.T) {
	cfg := engine.Config{Binary: os.Getenv("MSIME_ENGINE_TEST_BINARY"), Resources: os.Getenv("MSIME_ENGINE_TEST_RESOURCES")}
	if cfg.Binary == "" || cfg.Resources == "" {
		t.Skip("需要真实引擎和固定发布词库")
	}
	s := testStore(t)
	ctx := t.Context()
	tokens := complete(t, s, Identity{"email", "snapshot-native@example.test"})
	user := tokens.User.ID
	entries := []DictionaryEntry{
		{Kind: "pinyin", Code: "ni'hao", Word: "拟蒿", Weight: 100000000},
		{Kind: "wubi", Code: "abcd", Word: "合成五笔词", Weight: 100000000},
		{Kind: "wubi98", Code: "abcd", Word: "合成九八词", Weight: 100000000},
		{Kind: "english", Code: "zzmsimeexample", Word: "Zzmsimeexample", Weight: 100000000},
		{Kind: "quick", Code: "apitest", Word: "合成短语", Weight: 100000000},
	}
	for _, e := range entries {
		change, err := s.EditDictionary(ctx, user, e.Kind, "", 0, &e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.EditDictionary(ctx, user, e.Kind, change.Replacement.ID, change.Revision, &DictionaryEntry{Code: e.Code, Word: e.Word, Weight: e.Weight + 1}); err != nil {
			t.Fatal(err)
		}
		deleted := e
		deleted.Word += "已删除"
		change, err = s.EditDictionary(ctx, user, e.Kind, "", 0, &deleted)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.EditDictionary(ctx, user, e.Kind, change.Replacement.ID, change.Revision, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_candidate_positions(user_id,context,code,word,position) VALUES($1,'ni''hao','ni''hao','拟蒿',2)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,'ni''hao','ni''hao','拟蒿',1)`, user); err != nil {
		t.Fatal(err)
	}
	queries := []map[string]any{
		{"operation": "candidates", "text": "nihao", "scheme": "pinyin"},
		{"operation": "candidates", "text": "nihc", "scheme": "shuangpin"},
		{"operation": "candidates", "text": "n", "scheme": "pinyin"},
		{"operation": "jianpin", "text": "nh", "scheme": "pinyin"},
		{"operation": "jianpin", "text": "nh", "scheme": "shuangpin"},
		{"operation": "candidates", "text": "abcd", "scheme": "wubi"},
		{"operation": "candidates", "text": "abcd", "scheme": "wubi98"},
		{"operation": "english", "text": "zzmsime"},
		{"operation": "quick", "text": "apitest"},
	}
	for _, e := range entries {
		text := e.Code
		if e.Kind == "pinyin" {
			text = "nihao"
		}
		queries = append(queries, map[string]any{"operation": "dictionary", "kind": e.Kind, "text": text})
	}
	queries = append(queries, map[string]any{"operation": "dictionary", "kind": "quick", "text": ""})
	for i, query := range queries {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			query["limit"] = 20
			query["profile"] = "xiaohe"
			request := map[string]any{"operation": "personal_query", "query": query}
			run := func(filtered bool) json.RawMessage {
				t.Helper()
				out, err := cfg.QuerySnapshot(ctx, request, func(ctx context.Context, w io.Writer) error {
					emit := func(raw json.RawMessage) error { _, err := w.Write(append(raw, '\n')); return err }
					if filtered {
						return streamDictionaryQuerySnapshot(ctx, s.pool, user, query, emit)
					}
					return s.StreamDictionarySnapshot(ctx, user, emit)
				})
				if err != nil {
					t.Fatal(query, err)
				}
				return out
			}
			if full, filtered := run(false), run(true); !bytes.Equal(full, filtered) {
				t.Fatalf("筛选改变引擎结果：全量 %s；筛选 %s", full, filtered)
			}
		})
	}
	// 从实际 HTTP 路径观察 PostgreSQL 返回的快照行数，防止调用方误用全量快照。
	trace := &snapshotRowTrace{}
	poolConfig := s.pool.Config()
	poolConfig.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}, engine: cfg})
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/users/me/dictionary/candidates", `{"kind":"english","text":"zzmsime"}`},
		{"POST", "/v1/users/me/dictionary/candidates", `{"kind":"pinyin","scheme":"shuangpin","text":"nihc"}`},
		{"POST", "/v1/users/me/dictionary/candidates", `{"kind":"jianpin","text":"nh"}`},
		{"POST", "/v1/users/me/dictionary/candidates", `{"kind":"wubi98","text":"abcd"}`},
		{"GET", "/v1/users/me/dictionaries/quick/catalog", ""},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal(tc, response.Code, response.Body.String())
		}
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if !reflect.DeepEqual(trace.rows, []int64{5, 5, 5, 5, 5}) {
		t.Fatal("HTTP 查询发送了无关覆盖记录", trace.rows)
	}

}

type snapshotRowTrace struct {
	mu   sync.Mutex
	rows []int64
}

type snapshotTraceKey struct{}

func (*snapshotRowTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, snapshotTraceKey{}, strings.HasPrefix(data.SQL, "SELECT record FROM ("))
}

func (trace *snapshotRowTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(snapshotTraceKey{}) != true {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.rows = append(trace.rows, data.CommandTag.RowsAffected())
}

func TestDictionaryQuerySnapshotSkipsUnrelatedReplay(t *testing.T) {
	cfg := engine.Config{Binary: os.Getenv("MSIME_ENGINE_TEST_BINARY"), Resources: os.Getenv("MSIME_ENGINE_TEST_RESOURCES")}
	if cfg.Binary == "" || cfg.Resources == "" {
		t.Skip("需要真实引擎和固定发布词库")
	}
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "unrelated-replay@example.test"}).User.ID
	query := map[string]any{"operation": "english", "text": "hello", "limit": 3}
	run := func(filtered bool) (json.RawMessage, int) {
		t.Helper()
		size := 0
		raw, err := cfg.QuerySnapshot(ctx, map[string]any{"operation": "personal_query", "query": query}, func(ctx context.Context, w io.Writer) error {
			emit := func(row json.RawMessage) error {
				size += len(row) + 1
				_, err := w.Write(append(row, '\n'))
				return err
			}
			if filtered {
				return streamDictionaryQuerySnapshot(ctx, s.pool, user, query, emit)
			}
			return s.StreamDictionarySnapshot(ctx, user, emit)
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw, size
	}
	initial, _ := run(false)
	var base struct{ Candidates []struct{ Code, Word string } }
	if err := json.Unmarshal(initial, &base); err != nil || len(base.Candidates) == 0 {
		t.Fatal(string(initial), err)
	}
	candidate := base.Candidates[len(base.Candidates)-1]
	entries := make([]DictionaryEntry, 1000)
	for i := range entries {
		entries[i] = DictionaryEntry{Code: fmt.Sprintf("code%d", i), Word: fmt.Sprintf("合成短语%d", i), Weight: 10}
	}
	if _, err := s.ImportDictionary(ctx, user, "quick", entries); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_candidate_positions(user_id,context,code,word,position) VALUES($1,'english:hello',$2,$3,1)`, user, candidate.Code, candidate.Word); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,'english:hello',$2,$3,2)`, user, candidate.Code, candidate.Word); err != nil {
		t.Fatal(err)
	}
	full, fullBytes := run(false)
	filtered, filteredBytes := run(true)
	if !bytes.Equal(full, filtered) {
		t.Fatalf("跳过无关词库回放改变结果：全量 %s；筛选 %s", full, filtered)
	}
	var result struct {
		Revision   int64
		Candidates []struct {
			Word     string
			Position int `json:"fixed_position"`
		}
	}
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1000 || len(result.Candidates) == 0 || result.Candidates[0].Word != candidate.Word || result.Candidates[0].Position != 1 {
		t.Fatal("无覆盖快照丢失版本或固定位置", string(filtered))
	}
	if fullBytes < 100000 || filteredBytes > 1024 {
		t.Fatal("无关词库仍被发送", fullBytes, filteredBytes)
	}
	t.Logf("1000 条无关快捷短语：快照 %d → %d 字节，真实引擎响应完全相同", fullBytes, filteredBytes)
}
