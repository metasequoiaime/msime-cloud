package account

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAliyunSMSRequestAndProviderRejection(t *testing.T) {
	t.Setenv("SMS_TEST_ID", "test-id")
	t.Setenv("SMS_TEST_SECRET", "test-secret")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, code := range []string{"OK", "isv.BUSINESS_LIMIT_CONTROL"} {
		calls := 0
		http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Scheme != "https" || r.URL.Host != "dysmsapi.aliyuncs.com" {
				t.Error("必须固定 HTTPS 短信端点")
			}
			q := r.URL.Query()
			if r.Body != nil {
				b, _ := io.ReadAll(r.Body)
				values, _ := url.ParseQuery(string(b))
				for k, v := range values {
					q[k] = v
				}
			}
			if q.Get("Action") != "SendSms" || q.Get("PhoneNumbers") != "8613800000000" || q.Get("SignName") != "水杉输入法" || q.Get("TemplateCode") != "SMS_TEST" || q.Get("Signature") == "" {
				t.Error("短信请求或签名缺失")
			}
			var params map[string]string
			json.Unmarshal([]byte(q.Get("TemplateParam")), &params)
			if params["code"] != "123456" {
				t.Error("模板变量错误")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Code":"` + code + `","Message":"test","RequestId":"test"}`))}, nil
		})
		d := delivery{config: Config{SMS: SMSConfig{Region: "cn-hangzhou", AccessKeyIDEnv: "SMS_TEST_ID", AccessKeySecretEnv: "SMS_TEST_SECRET", SignName: "水杉输入法", TemplateCode: "SMS_TEST"}}}
		e := d.Send(context.Background(), "phone", "+8613800000000", "123456")
		if (e == nil) != (code == "OK") || calls != 1 {
			t.Fatal(code, e, calls)
		}
	}
}

func TestSMTPPermanentRecipientFailure(t *testing.T) {
	if !smtpPermanentError(&textproto.Error{Code: 550, Msg: "recipient rejected"}) {
		t.Fatal("550 收件人拒收应判定为永久失败")
	}
	if smtpPermanentError(&textproto.Error{Code: 451, Msg: "try again later"}) {
		t.Fatal("451 临时失败不得加入地址抑制表")
	}
	if !permanentDeliveryFailure(&deliveryFailure{permanent: true}) {
		t.Fatal("永久投递失败标记丢失")
	}
}

func TestSMTPRequiresSTARTTLSBeforeAuthentication(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan string, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- "accept failed"
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(c, "220 fake SMTP\r\n")
		r := bufio.NewReader(c)
		hello, _ := r.ReadString('\n')
		io.WriteString(c, "250-localhost\r\n250 AUTH PLAIN\r\n")
		next, _ := r.ReadString('\n')
		done <- hello + next
	}()
	d := delivery{config: Config{Email: MailConfig{Host: "localhost", Port: 587, Username: "test", From: "login@example.test"}}, smtpAddress: listener.Addr().String()}
	e = d.Send(context.Background(), "email", "user@example.test", "123456")
	if e == nil {
		t.Fatal("不允许明文 SMTP")
	}
	if strings.Contains(<-done, "AUTH PLAIN") {
		t.Fatal("不得在 TLS 前发送认证")
	}
}
func TestSMTPTLSDelivery(t *testing.T) {
	// httptest 提供已知测试证书；仅将它加入此测试连接的信任池。
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	cert := certServer.TLS.Certificates[0]
	leaf := certServer.Certificate()
	certServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	listener, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan string, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- "accept failed"
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(c)
		io.WriteString(c, "220 fake SMTP\r\n")
		var body strings.Builder
		for {
			line, e := r.ReadString('\n')
			if e != nil {
				done <- body.String()
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				io.WriteString(c, "250-localhost\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "AUTH"):
				io.WriteString(c, "235 authenticated\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				io.WriteString(c, "250 OK\r\n")
			case strings.HasPrefix(line, "DATA"):
				io.WriteString(c, "354 continue\r\n")
				for {
					line, e = r.ReadString('\n')
					if e != nil || line == ".\r\n" {
						break
					}
					body.WriteString(line)
				}
				io.WriteString(c, "250 accepted\r\n")
			case strings.HasPrefix(line, "QUIT"):
				io.WriteString(c, "221 bye\r\n")
				done <- body.String()
				return
			}
		}
	}()
	t.Setenv("SMTP_TEST_PASSWORD", "test-password")
	d := delivery{config: Config{Email: MailConfig{Host: "example.com", Port: 465, Username: "test", PasswordEnv: "SMTP_TEST_PASSWORD", From: "login@example.test"}}, smtpTLS: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}, smtpAddress: listener.Addr().String()}
	if e = d.Send(context.Background(), "email", "user@example.test", "123456"); e != nil {
		t.Fatal(e)
	}
	message := <-done
	pieces := strings.SplitN(message, "\r\n\r\n", 2)
	if len(pieces) != 2 || !strings.Contains(message, "To: user@example.test") {
		t.Fatal("邮件头错误")
	}
	body, e := base64.StdEncoding.DecodeString(strings.TrimSpace(pieces[1]))
	if e != nil || !strings.Contains(string(body), "123456") {
		t.Fatal("邮件验证码缺失", e)
	}
}
