package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

// queryListedWord 先取词库读音，再按编码和词语精确查重；逐字注音成功不等于词库收录。
func queryListedWord(ctx context.Context, query func(context.Context, any) (json.RawMessage, error), word string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := query(ctx, map[string]any{"operation": "annotate", "text": word})
	if err != nil {
		return nil, err
	}
	var annotated struct {
		Code string `json:"code"`
		Word string `json:"word"`
	}
	if json.Unmarshal(raw, &annotated) != nil || annotated.Word != word || !inputCode.MatchString(annotated.Code) {
		return nil, engine.ErrFailure
	}
	raw, err = query(ctx, map[string]any{"operation": "listed_pinyin_batch", "entries": []map[string]string{{"code": annotated.Code, "word": word}}})
	if err != nil {
		return nil, err
	}
	var checked struct {
		Listed []*bool `json:"listed"`
	}
	if json.Unmarshal(raw, &checked) != nil || len(checked.Listed) != 1 || checked.Listed[0] == nil {
		return nil, engine.ErrFailure
	}
	return json.Marshal(struct {
		Word   string `json:"word"`
		Code   string `json:"code"`
		Listed bool   `json:"listed"`
	}{word, annotated.Code, *checked.Listed[0]})
}
