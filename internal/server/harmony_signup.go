package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpdeadline"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

// 官网下载页的鸿蒙内测自助登记：访客在 HarmonyOS 版下面填自己的华为账号，通过 Cloudflare Turnstile 和限流后，服务端把账号加进 qq_bot.harmony_test 配置的外部测试群组，访客随后在同一页扫码或输入邀请码加入测试。
//
// 和群里管理员的「鸿蒙内测」命令不同，这里只加进群组，不调华为的邀请通知接口：访客本来就在下载页上，用不着通知；而且不发通知，填了别人的号码也不会给对方发短信或邮件。已经在群组里的账号和新加的账号返回同样的结果，接口不能用来探测谁在测试名单里。不保存访客信息，账号和令牌都不写进日志。
//
// 功能在 qq_bot.harmony_test 和 word_submissions（提供 Turnstile 站点密钥和密钥）都配置时开启，不需要新的配置项。
const (
	harmonySignupPath      = "/v1/community/harmony-testers"
	harmonySignupBodyBytes = 4 << 10
	// harmonySignupAccountChars 是账号输入的长度上限，够写带空格的手机号或长邮箱。
	harmonySignupAccountChars = 256
	// 查询和添加各一次华为接口，每次最多 harmonyTestTimeout。
	harmonySignupTimeout = 40 * time.Second
	// harmonySignupNickName 是在 AppGallery Connect 成员列表里显示的昵称，区分官网登记和群里管理员添加的成员。
	harmonySignupNickName = "官网"
)

// 每个地址的上限只计通过 Turnstile 的请求，短窗口先查；最后一档是全站每天的总数，防止有人换地址批量填号码把测试群组占满。
var harmonySignupLimits = []struct {
	scope      string
	limit      int
	window     time.Duration
	retryAfter string
	// global 表示按全站计数，不按访客地址。
	global bool
}{
	{"harmony-testers-10m", 3, 10 * time.Minute, "600", false},
	{"harmony-testers-day", 10, 24 * time.Hour, "3600", false},
	{"harmony-testers-total", 200, 24 * time.Hour, "3600", true},
}

type harmonySignupRequest struct {
	Account string `json:"account"`
	Token   string `json:"token"`
}

// harmonySignup 返回登记要用的 Turnstile 校验和华为接口，功能没开时都是 nil。
func (s *Server) harmonySignup() (*wordSubmitter, *harmonyTesters) {
	if s.words == nil || s.qq == nil || s.qq.harmony == nil {
		return nil, nil
	}
	return s.words, s.qq.harmony
}

func (s *Server) harmonySignupSettings(w http.ResponseWriter, r *http.Request) {
	ws, _ := s.harmonySignup()
	if ws == nil {
		respond(w, 200, map[string]any{"enabled": false, "site_key": ""})
		return
	}
	respond(w, 200, map[string]any{"enabled": true, "site_key": ws.config.Turnstile.SiteKey})
}

func (s *Server) registerHarmonyTester(w http.ResponseWriter, r *http.Request) {
	ws, testers := s.harmonySignup()
	if ws == nil {
		wordsFail(w, 503, "harmony_signup_disabled", "鸿蒙内测登记暂未开放，请稍后再试。")
		return
	}
	if !slices.Contains(ws.origins, r.Header.Get("Origin")) {
		wordsFail(w, 403, "origin_required", "请从官网表单提交。")
		return
	}
	address := ws.clientAddress(r)
	// Turnstile 前面的廉价闸门，所有副本通过 PostgreSQL 共用，脚本不能借这个接口无限制地调 siteverify。
	switch err := s.limitShared(r.Context(), "harmony-testers", address, 10); {
	case errors.Is(err, account.ErrLimited):
		w.Header().Set("Retry-After", "60")
		wordsFail(w, 429, "rate_limit_exceeded", "提交过于频繁，请稍后再试。")
		return
	case err != nil:
		slog.Error("harmony signup: rate limit unavailable", "reason", err.Error())
		w.Header().Set("Retry-After", "30")
		wordsFail(w, 503, "rate_limit_unavailable", "登记暂时不可用，请稍后再试。")
		return
	}
	if !httpjson.IsJSONContentType(r.Header.Get("Content-Type")) {
		wordsFail(w, 415, "json_required", "请求格式不正确。")
		return
	}
	var input harmonySignupRequest
	err := httpjson.DecodeValue(http.MaxBytesReader(w, r.Body, harmonySignupBodyBytes), &input)
	if httpjson.IsBodyTooLarge(err) {
		wordsFail(w, 413, "request_too_large", "内容过长。")
		return
	}
	if err != nil {
		wordsFail(w, 400, "invalid_json", "请求格式不正确。")
		return
	}
	var accounts []string
	if content := strings.TrimSpace(input.Account); content != "" && utf8.RuneCountInString(content) <= harmonySignupAccountChars {
		accounts, err = parseHarmonyAccounts(content)
	}
	if err != nil || len(accounts) != 1 {
		wordsFail(w, 400, "invalid_account", "请填写一个登录华为账号用的手机号或邮箱，大陆手机号直接写 11 位，其他地区写成「+区号 号码」。")
		return
	}
	if input.Token == "" || len(input.Token) > wordSubmissionTokenBytes {
		wordsFail(w, 400, "token_required", "请先完成提交验证。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), harmonySignupTimeout)
	defer cancel()
	httpdeadline.Extend(w, 0, harmonySignupTimeout+5*time.Second)
	if err = ws.verifyTurnstile(ctx, input.Token, "harmony"); err != nil {
		if errors.Is(err, errTurnstileRejected) {
			wordsFail(w, 403, "verification_failed", "验证已失效，请重新验证后提交。")
		} else {
			slog.Warn("harmony signup: turnstile unavailable", "reason", err.Error())
			w.Header().Set("Retry-After", "30")
			wordsFail(w, 503, "verification_unavailable", "验证服务暂时不可用，请重新验证后再试。")
		}
		return
	}
	for _, l := range harmonySignupLimits {
		subject := address
		if l.global {
			subject = "all"
		}
		if err = ws.limiter.RateLimit(ctx, l.scope, subject, l.limit, l.window); err != nil {
			if errors.Is(err, account.ErrLimited) {
				w.Header().Set("Retry-After", l.retryAfter)
				message := "提交过于频繁，请稍后再试。"
				if l.global {
					message = "今天的登记名额已用完，请明天再来。"
				}
				wordsFail(w, 429, "rate_limit_exceeded", message)
			} else {
				slog.Error("harmony signup: rate limit unavailable", "reason", err.Error())
				w.Header().Set("Retry-After", "30")
				wordsFail(w, 503, "rate_limit_unavailable", "登记暂时不可用，请稍后再试。")
			}
			return
		}
	}
	outcome := testers.add(ctx, accounts[0], harmonySignupNickName, false)
	if outcome.err != nil {
		e := outcome.err
		slog.Warn("harmony signup: agc failed", "step", e.step, "status", e.status, "code", e.code, "business", e.business)
		w.Header().Set("Retry-After", "60")
		if e.business == "101" {
			wordsFail(w, 503, "group_full", "测试名额已满，暂时无法登记，请稍后再来。")
		} else {
			wordsFail(w, 503, "agc_unavailable", "暂时无法登记到测试名单，请稍后再试。")
		}
		return
	}
	result := "added"
	if outcome.existed {
		result = "existed"
	}
	slog.Info("harmony signup: registered", "result", result)
	respond(w, 200, map[string]any{"registered": true})
}
