package httpdeadline

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

type deadlineWriter struct {
	*httptest.ResponseRecorder
	read, write       time.Time
	readErr, writeErr error
	calls             []string
}

func (w *deadlineWriter) SetReadDeadline(at time.Time) error {
	w.read = at
	w.calls = append(w.calls, "read")
	return w.readErr
}

func (w *deadlineWriter) SetWriteDeadline(at time.Time) error {
	w.write = at
	w.calls = append(w.calls, "write")
	return w.writeErr
}

type wrappedWriter struct{ http.ResponseWriter }

func (w wrappedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestExtend(t *testing.T) {
	for _, tc := range []struct {
		name              string
		read, write       time.Duration
		readErr, writeErr error
		calls             []string
	}{
		{name: "上传", read: time.Minute, write: time.Minute + 5*time.Second, calls: []string{"read", "write"}},
		{name: "只延长写期限", write: 2 * time.Minute, calls: []string{"write"}},
		{name: "只延长读期限", read: time.Minute, calls: []string{"read"}},
		{name: "保留原期限"},
		{name: "读失败仍设置写期限", read: time.Minute, write: 2 * time.Minute, readErr: errors.New("合成读期限错误"), calls: []string{"read", "write"}},
		{name: "忽略写失败", write: time.Minute, writeErr: errors.New("合成写期限错误"), calls: []string{"write"}},
		{name: "过去期限", read: -time.Second, write: -time.Second, calls: []string{"read", "write"}},
	} {
		for _, wrapped := range []bool{false, true} {
			name := tc.name
			if wrapped {
				name += "/多层包装"
			}
			t.Run(name, func(t *testing.T) {
				original := time.Unix(10, 0)
				w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), read: original, write: original, readErr: tc.readErr, writeErr: tc.writeErr}
				var response http.ResponseWriter = w
				if wrapped {
					response = wrappedWriter{wrappedWriter{w}}
				}
				before := time.Now()
				Extend(response, tc.read, tc.write)
				after := time.Now()
				if !slices.Equal(w.calls, tc.calls) {
					t.Fatalf("期限设置顺序=%v，期望 %v", w.calls, tc.calls)
				}
				for _, side := range []struct {
					name    string
					at      time.Time
					timeout time.Duration
				}{{"read", w.read, tc.read}, {"write", w.write, tc.write}} {
					if side.timeout == 0 {
						if side.at != original {
							t.Fatalf("%s 原期限被修改", side.name)
						}
					} else if side.at.Before(before.Add(side.timeout)) || side.at.After(after.Add(side.timeout)) {
						t.Fatalf("%s 期限未按当前时间及请求时限设置：%v", side.name, side.at)
					}
				}
			})
		}
	}
}

func TestExtendUnsupportedWriter(t *testing.T) {
	w := httptest.NewRecorder()
	Extend(wrappedWriter{w}, time.Minute, time.Minute+5*time.Second)
	if len(w.Header()) != 0 || w.Body.Len() != 0 || w.Code != http.StatusOK {
		t.Fatal("不支持连接期限的响应器被写入响应")
	}
}
