package publictools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestToolCallsPreserveTimeoutAndReleaseContext(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		call       func(context.Context, Execute, string, json.RawMessage) (Result, error)
		timeout    time.Duration
		parentCap  bool
	}{
		{"query_public_data", `{"resource":"plugins"}`, Call, 15 * time.Second, false},
		{"list_qq_drafts", `{}`, AdminCall, 15 * time.Second, false},
		{"submit_qq_draft", `{"id":"D12"}`, AdminCall, 130 * time.Second, false},
		{"close_qq_draft", `{"id":"D12"}`, AdminCall, 15 * time.Second, false},
		{"submit_qq_draft", `{"id":"D12"}`, AdminCall, 130 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := struct{}{}
			parent := context.WithValue(t.Context(), key, "synthetic")
			if tc.parentCap {
				var cancel context.CancelFunc
				parent, cancel = context.WithDeadline(parent, time.Now().Add(time.Minute))
				defer cancel()
			}
			started := time.Now()
			var captured context.Context
			_, err := tc.call(parent, func(ctx context.Context, _ Request) (json.RawMessage, error) {
				captured = ctx
				deadline, ok := ctx.Deadline()
				if !ok || ctx.Value(key) != "synthetic" {
					t.Fatal("调用没有继承上下文或设置截止时间")
				}
				if tc.parentCap {
					want, _ := parent.Deadline()
					if !deadline.Equal(want) {
						t.Fatal("调用延长了父上下文截止时间")
					}
				} else if deadline.Before(started.Add(tc.timeout)) || deadline.After(time.Now().Add(tc.timeout)) {
					t.Fatalf("调用超时时长改变：%v，期望 %v", time.Until(deadline), tc.timeout)
				}
				return json.RawMessage(`{"synthetic":true}`), nil
			}, tc.name, json.RawMessage(tc.args))
			if err != nil || captured == nil || !errors.Is(captured.Err(), context.Canceled) || parent.Err() != nil {
				t.Fatalf("调用结束后没有仅释放子上下文：%v，%v", captured, err)
			}
		})
	}
}

func TestExecuteToolRequestPreservesRequestAndResponseBoundaries(t *testing.T) {
	request := Request{Method: "POST", Path: "/synthetic", Body: json.RawMessage(`{"synthetic":true}`), NeedsToken: true}
	for _, tc := range []struct {
		name, data string
		invalid    bool
	}{
		{"合法对象", `{"synthetic":true}`, false},
		{"合法空值", `null`, false},
		{"恰好达到上限", `"` + strings.Repeat("x", MaxResponse-2) + `"`, false},
		{"超过上限", `"` + strings.Repeat("x", MaxResponse-1) + `"`, true},
		{"无法解码", "synthetic invalid JSON", true},
		{"空响应", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executed := false
			got, err := executeToolRequest(t.Context(), func(_ context.Context, r Request) (json.RawMessage, error) {
				executed = true
				if !reflect.DeepEqual(r, request) {
					t.Fatalf("请求字段改变：%+v", r)
				}
				return json.RawMessage(tc.data), nil
			}, request, time.Minute)
			if !executed {
				t.Fatal("没有执行合法请求")
			}
			if tc.invalid {
				if err == nil || err.Error() != "invalid_tool_response" || got.Source != "" || got.Data != nil {
					t.Fatalf("非法响应的错误或结果改变：%+v，%v", got, err)
				}
			} else if err != nil || got.Source != request.Path || string(got.Data) != tc.data {
				t.Fatalf("合法响应改变：%+v，%v", got, err)
			}
		})
	}
}

func TestExecuteToolRequestPreservesCancellationAndErrorPriority(t *testing.T) {
	upstream := errors.New("synthetic_failure")
	for _, tc := range []struct {
		name                         string
		cancelBefore, cancelDuring   bool
		expired, immediate, executed bool
		upstream, want               error
	}{
		{name: "调用前取消", cancelBefore: true, want: context.Canceled},
		{name: "父上下文已到期", expired: true, want: context.DeadlineExceeded},
		{name: "调用期限已到期", immediate: true, want: context.DeadlineExceeded},
		{name: "返回后取消先于响应校验", cancelDuring: true, executed: true, want: context.Canceled},
		{name: "调用错误先于响应校验", upstream: upstream, executed: true, want: upstream},
		{name: "调用错误先于返回后取消", cancelDuring: true, upstream: upstream, executed: true, want: upstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			if tc.expired {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			timeout := time.Minute
			if tc.immediate {
				timeout = 0
			}
			executed := false
			got, err := executeToolRequest(ctx, func(context.Context, Request) (json.RawMessage, error) {
				executed = true
				if tc.cancelDuring {
					cancel()
				}
				return json.RawMessage("synthetic invalid JSON"), tc.upstream
			}, Request{Path: "/synthetic"}, timeout)
			if !errors.Is(err, tc.want) || executed != tc.executed || got.Source != "" || got.Data != nil {
				t.Fatalf("取消或错误优先级改变：结果=%+v，错误=%v，执行=%t，期望错误=%v，执行=%t", got, err, executed, tc.want, tc.executed)
			}
		})
	}
}
