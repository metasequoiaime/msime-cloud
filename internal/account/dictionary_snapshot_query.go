package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// dictionaryQuerySnapshot 在引擎请求的上下文中回放查询快照，保留原始 JSON 行和写入错误。
func dictionaryQuerySnapshot(q snapshotQuerier, user string, query map[string]any) func(context.Context, io.Writer) error {
	return func(ctx context.Context, w io.Writer) error {
		return streamDictionaryQuerySnapshot(ctx, q, user, query, func(raw json.RawMessage) error {
			if _, err := w.Write(raw); err != nil {
				return err
			}
			_, err := w.Write([]byte{'\n'})
			return err
		})
	}
}

// queryPersonalDictionary 将当前用户的查询快照交给引擎，并沿用个人词库的响应和错误映射。
func (a *Service) queryPersonalDictionary(w http.ResponseWriter, r *http.Request, userID string, query map[string]any) {
	tx, err := a.store.userReadTransaction(r.Context(), userID)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	out, err := a.engine.QuerySnapshot(r.Context(), map[string]any{"operation": "personal_query", "query": query}, dictionaryQuerySnapshot(tx, userID, query))
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, out)
}
