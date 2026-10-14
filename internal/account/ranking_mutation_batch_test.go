package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func rankingMutationState(t *testing.T, db *Store, user string) []byte {
	t.Helper()
	var raw []byte
	if err := db.pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'entries',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM user_dictionary_entries e WHERE user_id=$1),
 'overlay',(SELECT jsonb_agg(to_jsonb(o) ORDER BY kind,code,word) FROM user_dictionary_overlay o WHERE user_id=$1),
 'selections',(SELECT jsonb_agg(to_jsonb(s) ORDER BY context,code,word) FROM user_candidate_selections s WHERE user_id=$1),
 'state',(SELECT to_jsonb(s) FROM user_dictionary_state s WHERE user_id=$1),
 'changes',(SELECT jsonb_agg(to_jsonb(c) ORDER BY revision) FROM user_dictionary_changes c WHERE user_id=$1)
)`, user).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCandidateMutationsBatchSelectionDeletionAndChangeWrites(t *testing.T) {
	for _, action := range []string{"增加选择计数", "清除选择计数", "删除候选词"} {
		for _, failChange := range []bool{false, true} {
			name := action + "/成功"
			if failChange {
				name = action + "/末条失败回滚"
			}
			t.Run(name, func(t *testing.T) {
				db := testStore(t)
				ctx := t.Context()
				user := complete(t, db, Identity{"email", "ranking-mutation@example.test"}).User.ID
				original, err := db.EditDictionary(ctx, user, "pinyin", "", 0, &DictionaryEntry{Code: "ni", Word: "你好", Weight: 50})
				if err != nil {
					t.Fatal(err)
				}
				selection := CandidateSelection{Context: "ranking-context", Code: "ni", Word: "你好", Count: 4}
				if _, err := db.pool.Exec(ctx, "INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,$2,$3,$4,3)", user, selection.Context, selection.Code, selection.Word); err != nil {
					t.Fatal(err)
				}
				response := RankingResult{Changed: true, Selection: selection, Updates: []DictionaryEntry{
					{ID: "engine-id", Kind: "pinyin", Code: "ni", Word: "你好", Weight: 100},
					{ID: "base-id", Kind: "pinyin", Code: "hao", Word: "好", Weight: 200},
				}}
				operation, wantWrites := "personal_rank", 3
				switch action {
				case "清除选择计数":
					response.Selection.Count = 0
				case "删除候选词":
					operation, wantWrites = "personal_delete", 4
					response.Deleted = original.Replacement
					response.Updates = response.Updates[1:]
				}
				raw, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				engine := fakeRankingEngineWithResponse(t, string(raw))
				before := rankingMutationState(t, db, user)
				if failChange {
					if _, err := db.pool.Exec(ctx, `CREATE FUNCTION reject_ranking_tail_change() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''合成排名变更写入失败''; END';
 CREATE TRIGGER reject_ranking_tail_change BEFORE INSERT ON user_dictionary_changes FOR EACH ROW EXECUTE FUNCTION reject_ranking_tail_change()`); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if _, err := db.pool.Exec(context.Background(), `DROP TRIGGER reject_ranking_tail_change ON user_dictionary_changes; DROP FUNCTION reject_ranking_tail_change()`); err != nil {
							t.Error(err)
						}
					})
				}
				trace := &positionMutationTrace{}
				cfg := db.pool.Config()
				cfg.ConnConfig.Tracer = trace
				cfg.MaxConns = 1
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				result, err := (&Store{pool: pool}).mutateCandidate(ctx, user, original.Revision, engine, map[string]any{"operation": "candidates", "text": "nihao", "limit": 20}, RankingAction{Code: "ni", Word: "你好"}, operation)
				if failChange {
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || pgErr.Message != "合成排名变更写入失败" {
						t.Fatalf("未命中最后一条写入的故障：%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				trace.mu.Lock()
				batches, writes := trace.batches, trace.writes
				trace.mu.Unlock()
				if batches != 1 || writes != wantWrites {
					t.Errorf("后续写入使用了 %d 次批量请求、批内 %d 条语句，期望 1、%d", batches, writes, wantWrites)
				}
				if failChange {
					if !bytes.Equal(rankingMutationState(t, db, user), before) {
						t.Fatal("失败操作没有回滚词条、覆盖层、选择计数、版本或变更记录")
					}
					return
				}
				if result.Revision != original.Revision+1 {
					t.Fatalf("版本=%d，期望=%d", result.Revision, original.Revision+1)
				}
				wantChange := DictionaryChange{Revision: result.Revision, Ranking: result.Updates, Previous: result.Deleted}
				if result.Deleted == nil {
					wantChange.Selection = &result.Selection
					if len(result.Updates) != 2 || result.Updates[0].ID != original.Replacement.ID {
						t.Fatal("调频没有保留个人词条的真实编号")
					}
					if count := clipboardTestCount(t, db, "SELECT COALESCE((SELECT count FROM user_candidate_selections WHERE user_id=$1 AND context=$2 AND code=$3 AND word=$4),0)", user, selection.Context, selection.Code, selection.Word); count != response.Selection.Count {
						t.Fatalf("选择计数=%d，期望=%d", count, response.Selection.Count)
					}
				} else {
					if count := clipboardTestCount(t, db, "SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1 AND id=$2", user, original.Replacement.ID); count != 0 {
						t.Fatal("候选词未从个人词条删除")
					}
					var deleted bool
					var entry DictionaryEntry
					if err := db.pool.QueryRow(ctx, "SELECT deleted,entry FROM user_dictionary_overlay WHERE user_id=$1 AND kind='pinyin' AND code='ni' AND word='你好'", user).Scan(&deleted, &raw); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &entry); err != nil || !deleted || !reflect.DeepEqual(entry, *result.Deleted) {
						t.Fatal("删除覆盖层与返回的候选词不一致")
					}
				}
				changes, _, err := db.DictionaryChanges(ctx, user, original.Revision, 20)
				if err != nil || len(changes) != 1 || !reflect.DeepEqual(changes[0], wantChange) {
					t.Fatalf("变更记录与返回结果不一致：%v", err)
				}
				if revision := clipboardTestCount(t, db, "SELECT revision FROM user_dictionary_state WHERE user_id=$1", user); int64(revision) != result.Revision {
					t.Fatalf("保存版本=%d，期望=%d", revision, result.Revision)
				}
			})
		}
	}
}
