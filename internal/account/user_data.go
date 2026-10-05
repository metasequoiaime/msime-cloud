package account

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// userDataExportTimeout 是 GET /v1/users/me/data/export 的路由时限：导出要把整份词库流式编码进 zip，大词库在慢网络上可能超过默认的 15 秒。
const userDataExportTimeout = 2 * time.Minute

// userDataExportsPerDay 是每个用户每天能导出的次数（auth_rates 键 export:<uid>，所有副本共享）。
const userDataExportsPerDay = 3

// userDataDeletable 是 DELETE /v1/users/me/data 能删除的分区。账号、会话和社区作品不在其中：它们分别走注销账号、移除设备和作品管理。
var userDataDeletable = map[string]bool{"preferences": true, "dictionary": true, "phrases": true, "clipboard": true, "voice": true}

// UserDataSection 是云端数据汇总里的一个分区。bytes 是 PostgreSQL 报告的行大小之和，作为统计信息而不是精确的磁盘占用。
type UserDataSection struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
	Items int64  `json:"items"`
}

// userDataSummaryQuery 用一条语句汇总本人各分区的行数和大小。头像存在对象存储里，这里只报告有没有（items 0 或 1）。
const userDataSummaryQuery = `SELECT id,bytes,items FROM (
 SELECT 1 AS o,'preferences' AS id,COALESCE(sum(pg_column_size(t.*)),0)::bigint AS bytes,count(*)::bigint AS items FROM user_preferences t WHERE user_id=$1
 UNION ALL SELECT 2,'dictionary',(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM user_dictionary_entries t WHERE user_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM user_dictionary_overlay t WHERE user_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM user_dictionary_changes t WHERE user_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM user_candidate_positions t WHERE user_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM user_candidate_selections t WHERE user_id=$1),(SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1)
 UNION ALL SELECT 3,'phrases',COALESCE(sum(pg_column_size(t.*)),0),COALESCE(sum(jsonb_array_length(t.phrases)),0) FROM user_phrases t WHERE user_id=$1
 UNION ALL SELECT 4,'clipboard',COALESCE(sum(pg_column_size(t.*)),0),count(*) FROM user_clipboard t WHERE user_id=$1
 UNION ALL SELECT 5,'community',(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM community_skins t WHERE owner_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM community_candidate_skins t WHERE owner_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM community_resources t WHERE owner_id=$1)+(SELECT COALESCE(sum(pg_column_size(t.*)),0) FROM community_plugins t WHERE owner_id=$1),(SELECT count(*) FROM community_skins WHERE owner_id=$1)+(SELECT count(*) FROM community_candidate_skins WHERE owner_id=$1)+(SELECT count(*) FROM community_resources WHERE owner_id=$1)+(SELECT count(*) FROM community_plugins WHERE owner_id=$1)
 UNION ALL SELECT 6,'voice',COALESCE(sum(octet_length(t.audio)+octet_length(t.transcript)),0),count(*) FROM voice_contributions t WHERE user_id=$1
 UNION ALL SELECT 7,'avatar',0,(SELECT count(*) FROM auth_users WHERE id=$1 AND avatar_key<>'')
) s ORDER BY o`

// UserDataSummary 返回本人云端数据的分区汇总，分区顺序固定。
func (s *Store) UserDataSummary(ctx context.Context, user string) ([]UserDataSection, int64, error) {
	rows, err := s.pool.Query(ctx, userDataSummaryQuery, user)
	if err != nil {
		return nil, 0, err
	}
	sections, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UserDataSection, error) {
		var v UserDataSection
		return v, row.Scan(&v.ID, &v.Bytes, &v.Items)
	})
	var total int64
	for _, v := range sections {
		total += v.Bytes
	}
	return sections, total, err
}

// DeleteUserData 在一个事务里删除所选分区。偏好和常用语清空内容并推进 revision，其他设备的旧 revision 写入会 409 并拉到空文档；词库删除条目、覆盖层、位置和选择次数后写一条 reset 变更并推进 revision，与快照恢复的重置方式相同，开着同步的设备拉取时会整份重载而不是把旧数据推回来。
func (s *Store) DeleteUserData(ctx context.Context, user string, sections map[string]bool) error {
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var statements []string
	if sections["preferences"] {
		statements = append(statements, `UPDATE user_preferences SET settings='{}'::jsonb,revision=revision+1 WHERE user_id=$1`)
	}
	if sections["phrases"] {
		statements = append(statements, `UPDATE user_phrases SET phrases='[]'::jsonb,revision=revision+1 WHERE user_id=$1`)
	}
	if sections["clipboard"] {
		statements = append(statements, `DELETE FROM user_clipboard WHERE user_id=$1`)
	}
	if sections["voice"] {
		statements = append(statements, `DELETE FROM voice_contributions WHERE user_id=$1`)
	}
	if sections["dictionary"] {
		statements = append(statements,
			`DELETE FROM user_dictionary_entries WHERE user_id=$1`,
			`DELETE FROM user_dictionary_overlay WHERE user_id=$1`,
			`DELETE FROM user_candidate_positions WHERE user_id=$1`,
			`DELETE FROM user_candidate_selections WHERE user_id=$1`,
			`WITH next AS (SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)+1 AS revision)
 INSERT INTO user_dictionary_changes(user_id,revision,change) SELECT $1,revision,jsonb_build_object('revision',revision,'reset',true) FROM next`,
			`INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,1) ON CONFLICT(user_id) DO UPDATE SET revision=user_dictionary_state.revision+1`)
	}
	for _, statement := range statements {
		if _, err = tx.Exec(ctx, statement, user); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// userDataSummary 处理 GET /v1/users/me/data：返回 {bytes, sections:[{id,bytes,items}]}。
func (a *Service) userDataSummary(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	sections, total, err := a.store.UserDataSummary(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"bytes": total, "sections": sections})
}

// userDataDelete 处理 DELETE /v1/users/me/data {"sections":[…]}：要求最近登录，只认 userDataDeletable 里的分区，至少一个，不能重复。
func (a *Service) userDataDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, true)
	if !ok {
		return
	}
	var v struct {
		Sections []string `json:"sections"`
	}
	if !read(w, r, &v) {
		return
	}
	chosen := map[string]bool{}
	for _, id := range v.Sections {
		if !userDataDeletable[id] || chosen[id] {
			writeError(w, 400, "invalid_sections")
			return
		}
		chosen[id] = true
	}
	if len(chosen) == 0 {
		writeError(w, 400, "invalid_sections")
		return
	}
	if err := a.store.DeleteUserData(r.Context(), p.UserID, chosen); err != nil {
		a.error(w, err)
		return
	}
	w.WriteHeader(204)
}

// userDataExportFile 是导出包里的一个 JSON 文件：名字和读取它内容的查询。查询只返回一行一列 jsonb。
type userDataExportFile struct {
	name, query string
}

// userDataExportFiles 是导出包里除词库外的文件。身份只列 provider 名，不含 subject、邮箱和令牌；会话只含设备信息与时间，不含令牌哈希。
var userDataExportFiles = []userDataExportFile{
	{"profile.json", `SELECT jsonb_build_object('id',u.id,'display_name',u.display_name,'created_at',u.created_at,'identities',COALESCE((SELECT jsonb_agg(DISTINCT i.provider) FROM auth_identities i WHERE i.user_id=u.id),'[]'::jsonb)) FROM auth_users u WHERE u.id=$1`},
	{"preferences.json", `SELECT COALESCE((SELECT jsonb_build_object('revision',revision,'settings',settings) FROM user_preferences WHERE user_id=$1),jsonb_build_object('revision',0,'settings','{}'::jsonb))`},
	{"phrases.json", `SELECT COALESCE((SELECT jsonb_build_object('revision',revision,'phrases',phrases) FROM user_phrases WHERE user_id=$1),jsonb_build_object('revision',0,'phrases','[]'::jsonb))`},
	{"clipboard.json", `SELECT jsonb_build_object('retention_days',COALESCE((SELECT retention_days FROM user_clipboard_settings WHERE user_id=$1),0),'items',COALESCE((SELECT jsonb_agg(jsonb_build_object('text',text,'pinned',pinned,'device',device,'updated_at',updated_at) ORDER BY pinned DESC,sequence DESC) FROM user_clipboard WHERE user_id=$1),'[]'::jsonb))`},
	{"community.json", `SELECT jsonb_build_object(
 'skins',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'name',name,'created_at',created_at) ORDER BY created_at) FROM community_skins WHERE owner_id=$1),'[]'::jsonb),
 'candidate_skins',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'name',name,'created_at',created_at) ORDER BY created_at) FROM community_candidate_skins WHERE owner_id=$1),'[]'::jsonb),
 'resources',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'kind',kind,'name',name,'created_at',created_at) ORDER BY created_at) FROM community_resources WHERE owner_id=$1),'[]'::jsonb),
 'plugins',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'kind',kind,'name',name,'created_at',created_at) ORDER BY created_at) FROM community_plugins WHERE owner_id=$1),'[]'::jsonb))`},
	{"sessions.json", `SELECT COALESCE(jsonb_agg(jsonb_build_object('user_agent',user_agent,'created_at',created_at,'revoked',revoked,'expires_at',expires_at) ORDER BY created_at DESC),'[]'::jsonb) FROM auth_sessions WHERE user_id=$1`},
}

// userDataExport 处理 GET /v1/users/me/data/export：把本人的数据流式写成 zip（profile、preferences、dictionary.ndjson、phrases、clipboard、community、sessions）。每用户每天 userDataExportsPerDay 次。开始写响应后出错只能中断连接，客户端会拿到不完整的 zip 而不是一个看似完整的包。内容不写日志。
func (a *Service) userDataExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if err := a.store.Rate(r.Context(), "export:"+hash(p.UserID), userDataExportsPerDay, 24*time.Hour); err != nil {
		if errors.Is(err, ErrLimited) {
			w.Header().Set("Retry-After", "3600")
			writeError(w, 429, "rate_limit_exceeded")
			return
		}
		a.error(w, err)
		return
	}
	// 先读出所有小文件，失败时还能返回一个正常的错误响应；词库最大，最后流式写入。
	docs := make([][]byte, len(userDataExportFiles))
	for i, f := range userDataExportFiles {
		if err := a.store.pool.QueryRow(r.Context(), f.query, p.UserID).Scan(&docs[i]); err != nil {
			a.error(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="msime-data-`+time.Now().UTC().Format("2006-01-02")+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(200)
	archive := zip.NewWriter(w)
	modified := time.Now().UTC()
	entry := func(name string) (io.Writer, error) {
		return archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified})
	}
	for i, f := range userDataExportFiles {
		out, err := entry(f.name)
		if err == nil {
			_, err = out.Write(indentJSON(docs[i]))
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	out, err := entry("dictionary.ndjson")
	if err == nil {
		err = a.store.StreamFullDictionarySnapshot(r.Context(), p.UserID, func(raw json.RawMessage) error {
			_, e := out.Write(append(raw, '\n'))
			return e
		})
	}
	if err == nil {
		err = archive.Close()
	}
	if err != nil {
		panic(http.ErrAbortHandler)
	}
}

// indentJSON 把数据库返回的 jsonb 排成两格缩进，方便用户直接打开阅读。
func indentJSON(raw []byte) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return append(out, '\n')
}
