package account

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 记录服务器最初设置的连接期限，测试在该期限过去后才继续真实网络传输。
type transferDeadlineListener struct {
	net.Listener
	read, write chan time.Time
}

func (l *transferDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &transferDeadlineConn{Conn: c, read: l.read, write: l.write}, nil
}

type transferDeadlineConn struct {
	net.Conn
	read, write         chan time.Time
	readOnce, writeOnce sync.Once
}

func (c *transferDeadlineConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.readOnce.Do(func() { c.read <- deadline })
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *transferDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.writeOnce.Do(func() { c.write <- deadline })
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func pastTransferDeadline(ctx context.Context, deadlines <-chan time.Time) error {
	var deadline time.Time
	select {
	case deadline = <-deadlines:
	case <-ctx.Done():
		return ctx.Err()
	}
	// 跨过服务器实际记录的初始期限，确保旧连接期限会拒绝后续读写。
	timer := time.NewTimer(time.Until(deadline.Add(20 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func transferDeadlineServer(t *testing.T, handler http.Handler) (*httptest.Server, *transferDeadlineListener) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	listener := &transferDeadlineListener{Listener: srv.Listener, read: make(chan time.Time, 1), write: make(chan time.Time, 1)}
	srv.Listener = listener
	srv.Config.ReadTimeout = time.Second
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, listener
}

// 在第一次响应写入时暂停；Unwrap 让处理函数仍能设置真实连接的期限。
type transferWaitWriter struct {
	http.ResponseWriter
	once sync.Once
	wait func() error
	err  error
}

func (w *transferWaitWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *transferWaitWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { w.err = w.wait() })
	if w.err != nil {
		return 0, w.err
	}
	return w.ResponseWriter.Write(p)
}

func TestUserDataExportOutlivesServerWriteTimeout(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "slow-export@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var listener *transferDeadlineListener
	srv, listener := transferDeadlineServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(&transferWaitWriter{ResponseWriter: w, wait: func() error {
			return pastTransferDeadline(ctx, listener.write)
		}}, r)
	}))
	r, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/users/me/data/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal("导出未能跨过服务器的初始写期限：", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("导出被截断：status=%d，error=%v", response.StatusCode, err)
	}
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(archive.File) != 10 {
		t.Fatalf("导出没有完整的 ZIP 目录：%v", err)
	}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, reader)
		reader.Close()
		if err != nil {
			t.Fatalf("导出文件 %s 不完整：%v", file.Name, err)
		}
	}
}

func TestFeedbackOutlivesServerReadTimeout(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "slow-feedback@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	srv, listener := transferDeadlineServer(t, mux)
	body, contentType := feedbackForm(t, payloadPart(validFeedbackPayload), screenshotPart(feedbackPNG(t, 8, 8), "image/png"))
	raw := body.Bytes()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	uploaded := make(chan error, 1)
	go func() {
		_, err := writer.Write(raw[:len(raw)-3])
		if err == nil {
			err = pastTransferDeadline(ctx, listener.read)
		}
		if err == nil {
			_, err = writer.Write(raw[len(raw)-3:])
		}
		writer.CloseWithError(err)
		uploaded <- err
	}()
	r, err := http.NewRequestWithContext(ctx, "POST", srv.URL+FeedbackPath, reader)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	r.Header.Set("Content-Type", contentType)
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal("反馈未能跨过服务器的初始读期限：", err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("慢速反馈上传失败：status=%d，error=%v，body=%s", response.StatusCode, err, result)
	}
	if err := <-uploaded; err != nil {
		t.Fatal("上传没有发送完整请求体：", err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback_screenshots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("反馈没有保存完整截图：count=%d，error=%v", count, err)
	}
}
