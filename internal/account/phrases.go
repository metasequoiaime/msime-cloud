package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// 无编码常用语的整列表同步，与偏好同一套 revision CAS：PUT 整份替换，revision 不符返回 409 revision_conflict。
const (
	maximumPhrasesBytes = 256 * 1024
	maximumPhrases      = 500
	maximumPhraseText   = 2000
	maximumPhraseGroup  = 32
	maximumPhraseID     = 64
	maximumPhraseOrder  = 1000000
)

// Phrase 是一条常用语。id 由客户端生成（推荐 UUID），在列表内唯一；text 可以多行；group 是分组名，空字符串表示未分组；position 是客户端的排序键。
type Phrase struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Group    string `json:"group"`
	Position int64  `json:"position"`
}

type Phrases struct {
	Revision int64    `json:"revision"`
	Phrases  []Phrase `json:"phrases"`
}

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

// validPhraseID 只接受 1–64 个 ASCII 字母、数字、`-` 和 `_`。
func validPhraseID(id string) bool {
	if id == "" || len(id) > maximumPhraseID {
		return false
	}
	for _, c := range id {
		if !(c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// validPhrases 校验整份列表：最多 500 条，id 唯一，text 为 1–2000 个 UTF-16 单元且不含 NUL，group 最多 32 个 UTF-16 单元且不含控制字符。
func validPhrases(list []Phrase) bool {
	if len(list) > maximumPhrases {
		return false
	}
	seen := make(map[string]bool, len(list))
	for _, p := range list {
		if !validPhraseID(p.ID) || seen[p.ID] {
			return false
		}
		seen[p.ID] = true
		if !utf8.ValidString(p.Text) || strings.ContainsRune(p.Text, 0) || utf16Length(p.Text) < 1 || utf16Length(p.Text) > maximumPhraseText {
			return false
		}
		if !utf8.ValidString(p.Group) || strings.ContainsFunc(p.Group, unicode.IsControl) || utf16Length(p.Group) > maximumPhraseGroup {
			return false
		}
		if p.Position < 0 || p.Position > maximumPhraseOrder {
			return false
		}
	}
	return true
}

func (s *Store) Phrases(ctx context.Context, user string) (Phrases, error) {
	out := Phrases{Phrases: []Phrase{}}
	var raw []byte
	e := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_phrases WHERE user_id=$1),0),COALESCE((SELECT phrases FROM user_phrases WHERE user_id=$1),'[]'::jsonb)`, user).Scan(&out.Revision, &raw)
	if e == nil {
		e = json.Unmarshal(raw, &out.Phrases)
	}
	return out, e
}

// PutPhrases 在 expected 等于当前 revision 时整份替换列表并把 revision 加一，否则返回 errRevisionConflict。行锁取自 userDataTransaction，被封禁的账号写不进来。
func (s *Store) PutPhrases(ctx context.Context, user string, expected int64, list []Phrase) (Phrases, error) {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return Phrases{}, e
	}
	defer tx.Rollback(ctx)
	var revision int64
	if e = tx.QueryRow(ctx, "SELECT COALESCE((SELECT revision FROM user_phrases WHERE user_id=$1),0)", user).Scan(&revision); e != nil {
		return Phrases{}, e
	}
	if revision != expected {
		return Phrases{}, errRevisionConflict
	}
	raw, e := json.Marshal(list)
	if e != nil {
		return Phrases{}, e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO user_phrases(user_id,revision,phrases) VALUES($1,$2,$3::jsonb)
 ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision,phrases=excluded.phrases`, user, revision+1, raw); e != nil {
		return Phrases{}, e
	}
	return Phrases{Revision: revision + 1, Phrases: list}, tx.Commit(ctx)
}

// phrases 处理 GET/PUT /v1/users/me/phrases。新用户返回 revision=0 和空列表。
func (a *Service) phrases(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.Method == "GET" {
		out, e := a.store.Phrases(r.Context(), p.UserID)
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, out)
		return
	}
	var v struct {
		Revision *int64   `json:"revision"`
		Phrases  []Phrase `json:"phrases"`
	}
	if !readSized(w, r, &v, maximumPhrasesBytes) {
		return
	}
	if v.Revision == nil || *v.Revision < 0 || v.Phrases == nil || !validPhrases(v.Phrases) {
		writeError(w, 400, "invalid_phrases")
		return
	}
	out, e := a.store.PutPhrases(r.Context(), p.UserID, *v.Revision, v.Phrases)
	if errors.Is(e, errRevisionConflict) {
		writeError(w, 409, "revision_conflict")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, out)
}
