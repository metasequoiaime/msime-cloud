package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// StreamFullDictionarySnapshot 从同一条 SQL 的快照读取数据记录，不包含来源账户或凭据。
// 下载格式的校验尾记录由 streamFullDictionarySnapshotWithFooter 补齐。
func (s *Store) StreamFullDictionarySnapshot(ctx context.Context, user string, emit func(json.RawMessage) error) error {
	rows, err := s.pool.Query(ctx, `SELECT record FROM (
 SELECT 0 AS category,'' AS sort1,'' AS sort2,'' AS sort3,jsonb_build_object('type','header','format','msime-dictionary-snapshot','version',1,'revision',COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)) AS record
 UNION ALL SELECT 1,kind,code,word,jsonb_build_object('type','entry','data',jsonb_build_object('id',id,'kind',kind,'code',code,'word',word,'weight',weight,'revision',revision,'updated_at',updated_at)) FROM user_dictionary_entries WHERE user_id=$1
 UNION ALL SELECT 2,kind,code,word,jsonb_build_object('type','overlay','deleted',deleted,'data',entry) FROM user_dictionary_overlay WHERE user_id=$1
 UNION ALL SELECT 3,context,code,word,jsonb_build_object('type','position','data',jsonb_build_object('context',context,'code',code,'word',word,'position',position)) FROM user_candidate_positions WHERE user_id=$1
 UNION ALL SELECT 4,context,code,word,jsonb_build_object('type','selection','data',jsonb_build_object('context',context,'code',code,'word',word,'count',count)) FROM user_candidate_selections WHERE user_id=$1
 ) snapshot ORDER BY category,sort1,sort2,sort3`, user)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = emit(raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

// streamFullDictionarySnapshotWithFooter 为全部记录补上数量和校验和，统一独立下载与 ZIP 导出格式。
func (s *Store) streamFullDictionarySnapshotWithFooter(ctx context.Context, user string, emit func(json.RawMessage) error) error {
	hash := sha256.New()
	records := 0
	err := s.StreamFullDictionarySnapshot(ctx, user, func(raw json.RawMessage) error {
		hash.Write(raw)
		hash.Write([]byte{'\n'})
		records++
		return emit(raw)
	})
	if err != nil {
		return err
	}
	footer, err := json.Marshal(map[string]any{"type": "footer", "records": records, "sha256": hex.EncodeToString(hash.Sum(nil))})
	if err != nil {
		return err
	}
	return emit(footer)
}

func (a *Service) dictionarySnapshot(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	// 完整快照可能较大，连接写期限必须覆盖路由允许的整个传输时间。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(dictionaryDownloadTimeout + 5*time.Second))
	started := false
	writeLine := ndjsonWriter(w)
	err := a.store.streamFullDictionarySnapshotWithFooter(r.Context(), p.UserID, func(raw json.RawMessage) error {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Content-Disposition", `attachment; filename="msime-dictionary-snapshot.ndjson"`)
			w.Header().Set("Cache-Control", "no-store")
			started = true
		}
		return writeLine(raw)
	})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		a.dictionaryError(w, err)
		return
	}
}
