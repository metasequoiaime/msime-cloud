package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// queryPersonalDictionary 将当前用户的查询快照交给引擎，并沿用个人词库的响应和错误映射。
func (a *Service) queryPersonalDictionary(w http.ResponseWriter, r *http.Request, userID string, query map[string]any) {
	out, err := a.engine.QuerySnapshot(r.Context(), map[string]any{"operation": "personal_query", "query": query}, func(ctx context.Context, writer io.Writer) error {
		return streamDictionaryQuerySnapshot(ctx, a.store.pool, userID, query, func(raw json.RawMessage) error {
			if _, err := writer.Write(raw); err != nil {
				return err
			}
			_, err := writer.Write([]byte{'\n'})
			return err
		})
	})
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, out)
}
