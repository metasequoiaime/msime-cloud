package account

import (
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestSMTPDataFailureIsNotAcceptedMetric(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"temporary_rejection", "451 synthetic temporary failure"},
		{"permanent_data_rejection", "554 synthetic message rejection"},
		{"connection_closed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			done := make(chan error, 1)
			go func() { done <- smtpMetricDataResult(serverConn, tc.reply) }()
			client, err := smtp.NewClient(clientConn, "example.test")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			d := delivery{config: Config{Email: MailConfig{From: "login@example.test"}}}
			err = d.smtpSend(client, "user@example.test", "合成主题", "合成正文")
			if serverErr := <-done; serverErr != nil {
				t.Fatal(serverErr)
			}
			if err == nil {
				t.Fatal("供应商没有确认接受，发送仍然成功")
			}
			if deliveryMayRefund(err) || permanentDeliveryFailure(err) {
				t.Fatal("DATA 后失败不能退款或将收件地址标为永久拒收")
			}
			if got := emailDeliveryStatus(err); got != emailDeliveryFailed {
				t.Fatalf("供应商未确认接受，却计入 %q", got)
			}
		})
	}
}

func smtpMetricDataResult(conn net.Conn, reply string) error {
	server := textproto.NewConn(conn)
	defer server.Close()
	if err := server.PrintfLine("220 synthetic SMTP"); err != nil {
		return err
	}
	for _, step := range []struct{ prefix, reply string }{
		{"EHLO ", "250 synthetic SMTP"},
		{"MAIL FROM:", "250 OK"},
		{"RCPT TO:", "250 OK"},
		{"DATA", "354 send data"},
	} {
		line, err := server.ReadLine()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, step.prefix) {
			return fmt.Errorf("SMTP 命令应以 %q 开头", step.prefix)
		}
		if err := server.PrintfLine("%s", step.reply); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, server.DotReader()); err != nil {
		return err
	}
	if reply != "" {
		return server.PrintfLine("%s", reply)
	}
	return nil
}
