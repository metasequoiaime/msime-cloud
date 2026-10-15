package account

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

type snapshotCopySource struct {
	scanner *bufio.Scanner
	seq     int64
}

func (s *snapshotCopySource) Next() bool {
	if !s.scanner.Scan() {
		return false
	}
	s.seq++
	return true
}
func (s *snapshotCopySource) Values() ([]any, error) {
	return []any{s.seq, string(s.scanner.Bytes())}, nil
}
func (s *snapshotCopySource) Err() error { return s.scanner.Err() }

// Restore validates on private disk before taking a user write lock. COPY stages
// records in a transaction-local table; no target state changes before validation.
func (s *Store) RestoreDictionarySnapshot(ctx context.Context, user string, expected int64, reader io.Reader, cfg engine.Config) (int64, error) {
	file, err := os.CreateTemp("", "msime-restore-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	entryCount := 0
	err = decodeDictionarySnapshot(reader, func(record snapshotRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Type == "entry" {
			entryCount++
			if entryCount > 100000 {
				return errDictionaryLimit
			}
			var e DictionaryEntry
			if err := json.Unmarshal(record.Data, &e); err != nil {
				return err
			}
			e.ID = randomToken()
			record.Data, err = json.Marshal(e)
			if err != nil {
				return err
			}
		}
		return json.NewEncoder(file).Encode(record)
	})
	if err != nil {
		return 0, err
	}
	_, err = cfg.QuerySnapshot(ctx, map[string]any{"operation": "validate_snapshot"}, func(ctx context.Context, w io.Writer) error {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := io.Copy(w, file)
		return err
	})
	if err != nil {
		return 0, err
	}
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var current int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)`, user).Scan(&current); err != nil {
		return 0, err
	}
	if current != expected {
		return 0, errRevisionConflict
	}
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE msime_restore_stage(seq bigint PRIMARY KEY,data jsonb NOT NULL) ON COMMIT DROP`); err != nil {
		return 0, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 65536)
	source := &snapshotCopySource{scanner: scanner}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"pg_temp", "msime_restore_stage"}, []string{"seq", "data"}, source); err != nil {
		return 0, err
	}
	// Match entries to overlays by indexed keys rather than repeatedly scanning
	// the uploaded JSON records when restoring a large dictionary.
	if _, err = tx.Exec(ctx, `CREATE INDEX ON msime_restore_stage ((data->>'type'),(data->'data'->>'kind'),(data->'data'->>'code'),(data->'data'->>'word')); ANALYZE msime_restore_stage`); err != nil {
		return 0, err
	}
	// A live personal entry must have the same live user-owned overlay. A user-owned
	// live overlay must have a personal entry. Tombstones and base ranks are distinct.
	var invalid bool
	if err = tx.QueryRow(ctx, `WITH e AS (SELECT data->'data' AS v FROM msime_restore_stage WHERE data->>'type'='entry'),o AS (SELECT data->'data' AS v,(data->>'deleted')::boolean AS deleted FROM msime_restore_stage WHERE data->>'type'='overlay') SELECT EXISTS(SELECT 1 FROM e FULL JOIN o ON e.v->>'kind'=o.v->>'kind' AND e.v->>'code'=o.v->>'code' AND e.v->>'word'=o.v->>'word' WHERE (e.v IS NOT NULL AND (o.v IS NULL OR o.deleted OR o.v->>'user_inserted'='false' OR e.v->>'weight'<>o.v->>'weight')) OR (o.v IS NOT NULL AND NOT o.deleted AND COALESCE(o.v->>'user_inserted','true')='true' AND e.v IS NULL))`).Scan(&invalid); err != nil {
		return 0, err
	}
	if invalid {
		return 0, errInvalidSnapshot
	}
	revision := current + 1 + source.seq
	if revision <= current {
		return 0, errInvalidSnapshot
	}
	// 验证通过后按原顺序批量清理和重建词库，各语句仍能读取前序语句写入的新状态；任何写入失败都回滚整个恢复事务。
	batch := &pgx.Batch{}
	batch.Queue(`WITH deleted_entries AS (
 DELETE FROM user_dictionary_entries WHERE user_id=$1
), deleted_overlay AS (
 DELETE FROM user_dictionary_overlay WHERE user_id=$1
), deleted_positions AS (
 DELETE FROM user_candidate_positions WHERE user_id=$1
), deleted_selections AS (
 DELETE FROM user_candidate_selections WHERE user_id=$1
)
SELECT 1`, user)
	// 词条与覆盖记录共用新版本；替换来源 ID 以避免跨账号冲突，客户端收到重置事件后重新加载。
	batch.Queue(`INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision,updated_at)
 SELECT e.data->'data'->>'id',$1,e.data->'data'->>'kind',e.data->'data'->>'code',e.data->'data'->>'word',(e.data->'data'->>'weight')::bigint,$2::bigint+1+o.seq,now()
 FROM msime_restore_stage e JOIN msime_restore_stage o ON o.data->>'type'='overlay' AND e.data->'data'->>'kind'=o.data->'data'->>'kind' AND e.data->'data'->>'code'=o.data->'data'->>'code' AND e.data->'data'->>'word'=o.data->'data'->>'word' WHERE e.data->>'type'='entry'`, user, current)
	batch.Queue(`UPDATE msime_restore_stage o SET data=jsonb_set(o.data,'{data}',(o.data->'data')||jsonb_build_object('id',COALESCE((SELECT id FROM user_dictionary_entries e WHERE e.user_id=$1 AND e.kind=o.data->'data'->>'kind' AND e.code=o.data->'data'->>'code' AND e.word=o.data->'data'->>'word'),''),'revision',$2::bigint+1+o.seq,'updated_at',now())) WHERE o.data->>'type'='overlay'`, user, current)
	batch.Queue(`INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted) SELECT $1,data->'data'->>'kind',data->'data'->>'code',data->'data'->>'word',data->'data',(data->>'deleted')::boolean FROM msime_restore_stage WHERE data->>'type'='overlay'`, user)
	batch.Queue(`INSERT INTO user_candidate_positions(user_id,context,code,word,position) SELECT $1,data->'data'->>'context',data->'data'->>'code',data->'data'->>'word',(data->'data'->>'position')::integer FROM msime_restore_stage WHERE data->>'type'='position'`, user)
	batch.Queue(`INSERT INTO user_candidate_selections(user_id,context,code,word,count) SELECT $1,data->'data'->>'context',data->'data'->>'code',data->'data'->>'word',(data->'data'->>'count')::integer FROM msime_restore_stage WHERE data->>'type'='selection'`, user)
	batch.Queue(`INSERT INTO user_dictionary_changes(user_id,revision,change) VALUES($1,$2::bigint+1,jsonb_build_object('revision',$2::bigint+1,'reset',true))`, user, current)
	batch.Queue(`INSERT INTO user_dictionary_changes(user_id,revision,change)
 SELECT $1,$2::bigint+1+seq,jsonb_build_object('revision',$2::bigint+1+seq)||CASE data->>'type' WHEN 'overlay' THEN CASE WHEN (data->>'deleted')::boolean THEN jsonb_build_object('previous',data->'data','replacement',null) ELSE jsonb_build_object('previous',null,'replacement',data->'data') END WHEN 'position' THEN jsonb_build_object('position',data->'data') WHEN 'selection' THEN jsonb_build_object('selection',data->'data') END FROM msime_restore_stage WHERE data->>'type' IN ('overlay','position','selection')`, user, current)
	batch.Queue(`INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision`, user, revision)
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return 0, err
	}
	return revision, tx.Commit(ctx)
}
