package account

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/auth/credentials"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/dysmsapi"
)

type delivery struct {
	config      Config
	smtpTLS     *tls.Config
	smtpAddress string
}

// deliveryFailure 记录 SMTP 是否已经进入 DATA 阶段；进入 DATA 后的失败可能已经投递，不能退回额度或判定地址失效。
type deliveryFailure struct {
	permanent bool
	accepted  bool
}

func (e *deliveryFailure) Error() string { return "邮件发送失败" }

func permanentDeliveryFailure(err error) bool {
	var failure *deliveryFailure
	return errors.As(err, &failure) && failure.permanent
}

func deliveryMayRefund(err error) bool {
	var failure *deliveryFailure
	return errors.As(err, &failure) && !failure.accepted
}

const deliverySuppressionTTL = 30 * 24 * time.Hour

// deliveryGlobalDailyLimit 对齐 Lark 的每日免费发信上限，下载链接和验证码共用这份额度。
const deliveryGlobalDailyLimit = 450

type contextTransport struct{ ctx context.Context }

func (t contextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(r.Clone(t.ctx))
}
func (d delivery) Send(ctx context.Context, channel, target, code string) error {
	var e error
	switch channel {
	case "phone":
		e = d.sms(ctx, target, code)
	case "email":
		e = d.email(ctx, target, code)
	default:
		e = errors.New("渠道未配置")
	}
	// SDK/SMTP 错误可能包含手机号、验证码或凭据，不向调用者透传。
	if e != nil {
		if deliveryFailureError(e) {
			return e
		}
		return errors.New("验证码发送失败")
	}
	return nil
}
func (d delivery) sms(ctx context.Context, target, code string) error {
	c := d.config.SMS
	cfg := sdk.NewConfig().WithScheme("HTTPS").WithTimeout(10 * time.Second).WithAutoRetry(false)
	cfg.Transport = contextTransport{ctx}
	client, e := dysmsapi.NewClientWithOptions(c.Region, cfg, &credentials.AccessKeyCredential{AccessKeyId: os.Getenv(c.AccessKeyIDEnv), AccessKeySecret: os.Getenv(c.AccessKeySecretEnv)})
	if e != nil {
		return e
	}
	r := dysmsapi.CreateSendSmsRequest()
	r.SetScheme("HTTPS")
	r.SetDomain("dysmsapi.aliyuncs.com")
	r.PhoneNumbers = strings.TrimPrefix(target, "+")
	r.SignName = c.SignName
	r.TemplateCode = c.TemplateCode
	params, _ := json.Marshal(map[string]string{"code": code})
	r.TemplateParam = string(params)
	result, e := client.SendSms(r)
	if e != nil {
		return e
	}
	if result.Code != "OK" {
		return errors.New("短信供应商拒绝")
	}
	return nil
}

// Mailer 发送一封纯文本邮件。主题和正文都由服务端的固定模板生成，调用方不能把用户输入拼进去。
type Mailer interface {
	Mail(ctx context.Context, target, subject, body string) error
}

// Mail 发送一封通知邮件；和 Send 一样不透传 SMTP 错误，因为其中可能含收件地址或凭据。
func (d delivery) Mail(ctx context.Context, target, subject, body string) error {
	if d.config.Email.From == "" {
		return errors.New("邮件发送失败")
	}
	if e := d.mail(ctx, target, subject, body); e != nil {
		if deliveryFailureError(e) {
			return e
		}
		return errors.New("邮件发送失败")
	}
	return nil
}

// email 发送登录验证码，复用 mail 的连接、认证和发送。
func (d delivery) email(ctx context.Context, target, code string) error {
	return d.mail(ctx, target, "水杉输入法登录验证码", "你的验证码是 "+code+"，5 分钟内有效。请勿向他人透露。")
}

// smtpConnect 按配置建立到 SMTP 服务器的连接：465 端口直接 TLS，587 端口先明文再 STARTTLS。连接的期限取 10 秒和 ctx 期限中较早的一个，ctx 取消时连接随之关闭；返回的 release 负责收尾。
func (d delivery) smtpConnect(ctx context.Context) (*smtp.Client, func(), error) {
	c := d.config.Email
	address := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	if d.smtpAddress != "" {
		address = d.smtpAddress
	}
	tlsConfig := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	if d.smtpTLS != nil {
		tlsConfig = d.smtpTLS.Clone()
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var e error
	if c.Port == 465 {
		conn, e = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, e = dialer.DialContext(ctx, "tcp", address)
	}
	if e != nil {
		return nil, nil, e
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	deadline := time.Now().Add(10 * time.Second)
	if v, ok := ctx.Deadline(); ok && v.Before(deadline) {
		deadline = v
	}
	conn.SetDeadline(deadline)
	client, e := smtp.NewClient(conn, c.Host)
	if e != nil {
		stop()
		conn.Close()
		return nil, nil, e
	}
	release := func() {
		client.Close()
		stop()
		conn.Close()
	}
	if c.Port == 587 {
		if e = client.StartTLS(tlsConfig); e != nil {
			release()
			return nil, nil, e
		}
	}
	return client, release, nil
}

// smtpAuthenticate 用配置的账号和环境变量里的密码做 PLAIN 认证。
func (d delivery) smtpAuthenticate(client *smtp.Client) error {
	c := d.config.Email
	return client.Auth(smtp.PlainAuth("", c.Username, os.Getenv(c.PasswordEnv), c.Host))
}

// mailBodyBase64 按 RFC 2045 将正文编码为每行至多 76 字符的 Base64，行间使用 CRLF。
func mailBodyBase64(body string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	if len(encoded) <= 76 {
		return encoded
	}
	var out strings.Builder
	out.Grow(len(encoded) + 2*((len(encoded)-1)/76))
	for len(encoded) > 76 {
		out.WriteString(encoded[:76])
		out.WriteString("\r\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded)
	return out.String()
}

// smtpSend 投递一封 UTF-8 纯文本邮件，主题用 Q 编码，正文 base64。
func (d delivery) smtpSend(client *smtp.Client, target, subject, body string) error {
	from := d.config.Email.From
	if e := client.Mail(from); e != nil {
		return &deliveryFailure{accepted: false}
	}
	if e := client.Rcpt(target); e != nil {
		if smtpPermanentError(e) {
			return &deliveryFailure{permanent: true}
		}
		return &deliveryFailure{accepted: false}
	}
	message := fmt.Sprintf("Date: %s\r\nFrom: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", time.Now().UTC().Format(time.RFC1123Z), from, target, mime.QEncoding.Encode("UTF-8", subject), mailBodyBase64(body))
	writer, e := client.Data()
	if e != nil {
		return &deliveryFailure{accepted: false}
	}
	if _, e = writer.Write([]byte(message)); e != nil {
		writer.Close()
		return &deliveryFailure{accepted: true}
	}
	if e = writer.Close(); e != nil {
		return &deliveryFailure{accepted: true}
	}
	// DATA 已确认即视为成功；QUIT 失败不能触发重复投递。
	_ = client.Quit()
	return nil
}

func smtpPermanentError(err error) bool {
	var smtpErr *textproto.Error
	return errors.As(err, &smtpErr) && smtpErr.Code >= 500 && smtpErr.Code < 600
}

func deliveryFailureError(err error) bool {
	var failure *deliveryFailure
	return errors.As(err, &failure)
}

// mail 依次连接、认证、发送。
func (d delivery) mail(ctx context.Context, target, subject, body string) error {
	client, release, e := d.smtpConnect(ctx)
	if e != nil {
		return &deliveryFailure{accepted: false}
	}
	defer release()
	if e = d.smtpAuthenticate(client); e != nil {
		return &deliveryFailure{accepted: false}
	}
	return d.smtpSend(client, target, subject, body)
}
