package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWordSubmissionLocalQueueHonorsContext(t *testing.T) {
	for _, name := range []string{"cancelled", "deadline"} {
		t.Run(name, func(t *testing.T) {
			s, upstream, _ := wordsFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			entered, hold := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(hold) })
			defer unblock()
			var started sync.Once
			upstream.before = func(key string) {
				if key == "GET "+wordsTestRepo+"/pulls" {
					started.Do(func() { close(entered) })
					select {
					case <-hold:
					case <-ctx.Done():
					}
				}
			}
			first := make(chan error, 1)
			go func() {
				_, err := s.words.submit(ctx, submission{kind: kindTranslations, lines: translationLines([]translationSubmissionEntry{{"first", "第一条"}})})
				first <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("首个投稿没有进入 GitHub 临界区：", ctx.Err())
			}
			queuedCtx, stop := context.WithCancel(ctx)
			want := context.Canceled
			if name == "deadline" {
				stop()
				queuedCtx, stop = context.WithTimeout(ctx, 50*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer stop()
			queued := make(chan error, 1)
			go func() {
				_, err := s.words.submit(queuedCtx, submission{kind: kindTranslations, lines: translationLines([]translationSubmissionEntry{{"cancelled", "已取消"}})})
				queued <- err
			}()
			if name == "cancelled" {
				stop()
			}
			<-queuedCtx.Done()
			var queuedErr error
			responded := false
			// 先确认请求已取消或到期；它必须在持有者继续写入前退出本地队列。
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case queuedErr = <-queued:
				responded = true
			case <-timer.C:
				t.Error("取消或到期的投稿仍被进程内队列阻塞")
			}
			unblock()
			select {
			case err := <-first:
				if err != nil {
					t.Fatal("等待者退出影响了正在写入的投稿：", err)
				}
			case <-ctx.Done():
				t.Fatal("首个投稿没有完成：", ctx.Err())
			}
			if !responded {
				select {
				case queuedErr = <-queued:
				case <-ctx.Done():
					t.Fatal("排队投稿没有退出：", ctx.Err())
				}
			}
			if !errors.Is(queuedErr, want) || !errors.Is(queuedErr, errWordsBusy) {
				t.Errorf("队列没有保留取消原因和忙碌错误：%v", queuedErr)
			}
			if upstream.count("PUT "+wordsTestRepo+"/contents/custom/translations.txt") != 1 {
				t.Fatal("已取消的等待者仍写入 GitHub")
			}
			// 取消等待者不能释放持有者的名额或泄漏名额；下一次投稿仍应追加到原 PR。
			number, err := s.words.submit(ctx, submission{kind: kindTranslations, lines: translationLines([]translationSubmissionEntry{{"next", "下一条"}})})
			if err != nil || number != 12 || upstream.count("PUT "+wordsTestRepo+"/contents/custom/translations.txt") != 2 || upstream.count("POST "+wordsTestRepo+"/pulls") != 1 {
				t.Fatalf("取消后串行写入未恢复：number=%d，error=%v", number, err)
			}
		})
	}
}
