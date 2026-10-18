package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// Telegram notice channel (unit U8).

// telegramMessageMax is Telegram's limit for one sendMessage text, counted like the Bot API counts it: in UTF-16 code units, so a character outside the BMP such as an emoji takes two.
const telegramMessageMax = 4096

// telegramNotices posts published notices to one chat with the Bot API.
type telegramNotices struct {
	endpoint string
	chatID   string
	client   *http.Client
}

// noticeBroadcaster returns what delivers published notices to admin.telegram, or nil when no channel is configured.
func (s *Server) noticeBroadcaster() account.NoticeBroadcaster {
	t := s.config.Admin.Telegram
	if t.ChatID == "" || t.botToken == "" {
		return nil
	}
	return &telegramNotices{
		endpoint: strings.TrimRight(t.APIURL, "/") + "/bot" + t.botToken + "/sendMessage",
		chatID:   t.ChatID,
		client:   &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// telegramText 拼接标题和正文，只扫描需要保留的前缀，按 UTF-16 码元上限在字符边界追加省略号。
func telegramText(n account.Notice) string {
	separator := ""
	if n.Body != "" {
		separator = "\n\n"
	}
	// UTF-16 码元数不超过字节数，每个码元最多占三个 UTF-8 字节；只对中间长度的消息计数。
	bytes := len(n.Title) + len(separator) + len(n.Body)
	if bytes <= telegramMessageMax || (bytes <= 3*telegramMessageMax && utf16Length(n.Title)+len(separator)+utf16Length(n.Body) <= telegramMessageMax) {
		return n.Title + separator + n.Body
	}
	parts := [3]string{n.Title, separator, n.Body}
	used := 0
	for partIndex, part := range parts {
		for offset, r := range part {
			size := 1
			if r > 0xffff {
				size++
			}
			if used+size > telegramMessageMax-1 {
				switch partIndex {
				case 0:
					return parts[0][:offset] + "…"
				case 1:
					return parts[0] + parts[1][:offset] + "…"
				default:
					return parts[0] + parts[1] + parts[2][:offset] + "…"
				}
			}
			used += size
		}
	}
	return n.Title + separator + n.Body
}

// telegramLength counts text in UTF-16 code units.
func telegramLength(text string) int {
	n := 0
	for _, r := range text {
		if size := utf16.RuneLen(r); size > 0 {
			n += size
		} else {
			n++
		}
	}
	return n
}

// BroadcastNotice 在 Bot API 确认投递后才成功；错误只记录状态和数字错误码，不含请求地址或上游描述。
func (t *telegramNotices) BroadcastNotice(ctx context.Context, n account.Notice) error {
	body, err := json.Marshal(map[string]any{"chat_id": t.chatID, "text": telegramText(n), "disable_web_page_preview": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: invalid endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return errors.New("telegram: request failed")
	}
	defer resp.Body.Close()
	var reply struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply) != nil || resp.StatusCode != http.StatusOK || !reply.OK {
		return fmt.Errorf("telegram: sendMessage answered %d (code %d)", resp.StatusCode, reply.ErrorCode)
	}
	return nil
}
