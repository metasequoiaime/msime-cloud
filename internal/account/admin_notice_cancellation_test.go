package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
)

// cancelNoticeBroadcaster 在广播已返回结果时取消请求，重现客户端断开连接的边界。
type cancelNoticeBroadcaster struct {
	cancel context.CancelFunc
	err    error
	calls  []Notice
}

func (b *cancelNoticeBroadcaster) BroadcastNotice(ctx context.Context, n Notice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.calls = append(b.calls, n)
	b.cancel()
	return b.err
}

const cancelNoticeValue = `{"title":"合成公告","body":"合成正文","targets":["all"],"channels":["app","telegram"]}`

func noticePublishRequest(t *testing.T, a *Service, draft bool) (string, string) {
	t.Helper()
	if !draft {
		return `{"action":"publish_notice","value":` + cancelNoticeValue + `}`, ""
	}
	status, out := noticeAction(t, a, adminTestContext(t.Context(), "draft-author"), `{"action":"save_notice_draft","value":`+cancelNoticeValue+`}`)
	if status != http.StatusOK {
		t.Fatal("无法准备公告草稿", status, out)
	}
	id := out["id"].(string)
	return `{"action":"publish_notice","id":"` + id + `"}`, id
}

func TestNoticePublishSurvivesCancellationAfterBroadcast(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(strconv.FormatBool(draft), func(t *testing.T) {
			a, db := noticeTestService(t)
			request, _ := noticePublishRequest(t, a, draft)
			const actor = "google:notice-owner:notice-owner@example.test"
			ctx, cancel := context.WithCancel(adminTestContext(t.Context(), actor))
			defer cancel()
			broadcaster := &cancelNoticeBroadcaster{cancel: cancel}
			a.ConfigureNoticeBroadcaster(broadcaster)
			status, out := noticeAction(t, a, ctx, request)
			if len(broadcaster.calls) != 1 || ctx.Err() == nil {
				t.Fatal("未复现广播确认后取消请求", len(broadcaster.calls), ctx.Err())
			}
			if status != http.StatusOK {
				t.Errorf("Telegram 已确认发送后仍因请求取消报告发布失败：%d %v", status, out)
			}
			id := strconv.FormatInt(broadcaster.calls[0].ID, 10)
			if status == http.StatusOK && out["id"] != id {
				t.Errorf("未返回已发布的公告编号：%v want=%s", out, id)
			}
			items := publicNotices(t, a, "/v1/notices?channel=app")
			if len(items) != 1 || items[0]["id"] != id {
				t.Errorf("外部消息已发出，但公告没有提交到 App 列表：%v", items)
			}
			var audits int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE action='publish_notice' AND target=$1 AND actor=$2`, id, actor).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if audits != 1 {
				t.Errorf("请求取消后丢失发布审计或操作者：%d", audits)
			}
			// 同一公告不能因第一次提交丢失而再次向 Telegram 广播。
			retry := `{"action":"publish_notice","id":"` + id + `"}`
			status, out = noticeAction(t, a, adminTestContext(t.Context(), actor), retry)
			if status != http.StatusConflict || errorCode(out) != "not_draft" || len(broadcaster.calls) != 1 {
				t.Errorf("已广播公告仍能重试或重复广播：%d %v calls=%d", status, out, len(broadcaster.calls))
			}
		})
	}
}

func TestNoticeBroadcastFailureStillRollsBackAfterCancellation(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(strconv.FormatBool(draft), func(t *testing.T) {
			a, db := noticeTestService(t)
			request, id := noticePublishRequest(t, a, draft)
			ctx, cancel := context.WithCancel(adminTestContext(t.Context(), "notice-owner"))
			defer cancel()
			broadcaster := &cancelNoticeBroadcaster{cancel: cancel, err: errors.New("合成广播失败")}
			a.ConfigureNoticeBroadcaster(broadcaster)
			status, out := noticeAction(t, a, ctx, request)
			if status != http.StatusBadGateway || errorCode(out) != "telegram_failed" || len(broadcaster.calls) != 1 {
				t.Fatal("广播失败被错误提交或未保留错误返回", status, out, len(broadcaster.calls))
			}
			if items := publicNotices(t, a, "/v1/notices"); len(items) != 0 || auditCount(t, db, "publish_notice") != 0 {
				t.Fatal("广播失败后仍发布或写入成功审计", items)
			}
			if draft {
				var stored string
				if err := db.pool.QueryRow(t.Context(), `SELECT status FROM admin_notices WHERE id=$1`, id).Scan(&stored); err != nil || stored != "draft" {
					t.Fatal("广播失败没有恢复草稿", stored, err)
				}
			}
		})
	}
}

func TestNoticePublishCommitFailureAfterBroadcast(t *testing.T) {
	a, db := noticeTestService(t)
	request, _ := noticePublishRequest(t, a, true)
	if _, err := db.pool.Exec(t.Context(), `CREATE FUNCTION reject_notice_commit() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''合成公告提交失败''; END';
CREATE CONSTRAINT TRIGGER reject_notice_commit AFTER INSERT OR UPDATE ON admin_notices DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status='live') EXECUTE FUNCTION reject_notice_commit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_notice_commit ON admin_notices; DROP FUNCTION IF EXISTS reject_notice_commit()`); err != nil {
			t.Error(err)
		}
	})
	broadcaster := &fakeBroadcaster{}
	a.ConfigureNoticeBroadcaster(broadcaster)
	status, out := noticeAction(t, a, adminTestContext(t.Context(), "notice-owner"), request)
	if status != http.StatusServiceUnavailable || errorCode(out) != "auth_unavailable" || len(broadcaster.sent) != 1 {
		t.Fatal("未保留广播之后真实数据库提交失败的语义", status, out, len(broadcaster.sent))
	}
	if items := publicNotices(t, a, "/v1/notices"); len(items) != 0 || auditCount(t, db, "publish_notice") != 0 {
		t.Fatal("真实提交失败仍留下公告或成功审计", items)
	}
}
