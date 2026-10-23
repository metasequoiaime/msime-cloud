package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
)

type snapshotWriteFailure struct {
	err    error
	after  int
	writes int
}

func (w *snapshotWriteFailure) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.after {
		return 0, w.err
	}
	return len(p), nil
}

func TestDictionaryQuerySnapshotWriter(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "snapshot-writer@example.test"}).User.ID
	for _, entry := range []DictionaryEntry{
		{Kind: "english", Code: "hello", Word: "Hello", Weight: 10},
		{Kind: "quick", Code: "hello", Word: "合成短语", Weight: 20},
	} {
		if _, err := s.EditDictionary(ctx, user, entry.Kind, "", 0, &entry); err != nil {
			t.Fatal(err)
		}
	}
	query := map[string]any{"operation": "english"}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	writeSnapshot := dictionaryQuerySnapshot(tx, user, query)
	// 回调应在执行时读取传入事务，包含尚未提交的变更。
	if _, err := tx.Exec(ctx, `UPDATE user_dictionary_overlay SET entry=jsonb_set(entry,'{word}',to_jsonb('事务内合成词'::text)) WHERE user_id=$1 AND kind='english'`, user); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := streamDictionaryQuerySnapshot(ctx, tx, user, query, func(raw json.RawMessage) error {
		want.Write(raw)
		return want.WriteByte('\n')
	}); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := writeSnapshot(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) || !bytes.Contains(got.Bytes(), []byte("事务内合成词")) || bytes.Contains(got.Bytes(), []byte("合成短语")) || bytes.Count(got.Bytes(), []byte{'\n'}) != 2 {
		t.Fatalf("查询快照字节或事务范围不一致：%s", got.Bytes())
	}
	var committed bytes.Buffer
	if err := dictionaryQuerySnapshot(s.pool, user, query)(ctx, &committed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(committed.Bytes(), []byte("事务内合成词")) || !bytes.Contains(committed.Bytes(), []byte("Hello")) {
		t.Fatalf("未提交的事务变更泄漏到连接池：%s", committed.Bytes())
	}
	for _, after := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("write_failure_after_%d", after), func(t *testing.T) {
			sentinel := errors.New("合成快照写入失败")
			writer := &snapshotWriteFailure{err: sentinel, after: after}
			if err := writeSnapshot(ctx, writer); !errors.Is(err, sentinel) || writer.writes != after+1 {
				t.Fatalf("写入失败未及时传播：error=%v writes=%d", err, writer.writes)
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := dictionaryQuerySnapshot(s.pool, user, query)(cancelled, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未传播：%v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshot(ctx, io.Discard); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("查询失败未传播：%v", err)
	}
}
