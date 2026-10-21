package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/oauth2"
)

// Administrator sign-in from the command line (msime-cloud login admin): the RFC 8252 loopback flow with the Google desktop client. The command listens on 127.0.0.1, the browser brings the code back to it, and it hands the code here; this side holds the client secret and the PKCE verifier, checks the ID token as the web sign-in does and answers with an admin session the command sends as a bearer token. The redirect is not stored: Google refuses a code exchange whose redirect_uri differs from the one the code was issued for.

const adminCLIFlowSeconds = 600

func (s *Server) adminCLIAvailable(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		fail(w, 405, "method_not_allowed")
		return false
	}
	if s.adminGoogle == nil || s.adminCLI == nil {
		fail(w, 404, "cli_login_disabled")
		return false
	}
	return true
}

func readAdminJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(v) != nil || decoder.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request")
		return false
	}
	return true
}

func (s *Server) adminCLIStart(w http.ResponseWriter, r *http.Request) {
	if !s.adminCLIAvailable(w, r) {
		return
	}
	// 在代理之后所有人的 TCP 对端都是代理，`client_ip_header` 指定携带真实地址的头。
	peer := account.ClientAddress(r, s.config.ClientIPHeader, s.config.siteProxySecret)
	// Shares the "admin-login" budget with the web login start, so a source address gets 10 login starts per minute in total across both entry points and all replicas.
	if !s.adminLimit(r.Context(), w, "admin-login", peer, 10) {
		return
	}
	var v struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if !readAdminJSON(w, r, &v) {
		return
	}
	if !account.LoopbackTarget(v.RedirectURI) {
		fail(w, 400, "invalid_redirect_uri")
		return
	}
	state, nonce, verifier := adminRandom(), adminRandom(), oauth2.GenerateVerifier()
	if err := s.adminStore.SaveAdminFlow(r.Context(), state, account.AdminLoginFlow{Nonce: nonce, Verifier: verifier}); err != nil {
		s.adminAuthError(w, err)
		return
	}
	config := s.adminCLI.oauth
	config.RedirectURL = v.RedirectURI
	respond(w, 200, map[string]any{
		"state":             state,
		"expires_in":        adminCLIFlowSeconds,
		"authorization_url": config.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce), oauth2.SetAuthURLParam("prompt", "select_account"), oauth2.S256ChallengeOption(verifier)),
	})
}

func (s *Server) adminCLIFinish(w http.ResponseWriter, r *http.Request) {
	if !s.adminCLIAvailable(w, r) {
		return
	}
	var v struct {
		State       string `json:"state"`
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
	}
	if !readAdminJSON(w, r, &v) {
		return
	}
	if len(v.State) != 64 || v.Code == "" || len(v.Code) > 4096 || !account.LoopbackTarget(v.RedirectURI) {
		fail(w, 400, "invalid_request")
		return
	}
	flow, err := s.adminStore.ConsumeAdminFlow(r.Context(), v.State)
	if err != nil {
		s.adminAuthError(w, err)
		return
	}
	identity, err := s.adminCLIIdentity(r.Context(), flow, v.Code, v.RedirectURI)
	identity.UserAgent = r.UserAgent()
	if err != nil {
		s.adminAuthError(w, err)
		return
	}
	token, err := s.adminStore.CreateAdminSession(r.Context(), identity)
	if err != nil {
		s.adminAuthError(w, err)
		return
	}
	respond(w, 200, map[string]any{"token": token, "token_type": "Bearer", "expires_in": 8 * 60 * 60, "email": identity.Email})
}

// adminCLIIdentity 使用桌面客户端和本次回调地址校验身份，认证拒绝统一映射为 ErrInvalid。
func (s *Server) adminCLIIdentity(ctx context.Context, flow account.AdminLoginFlow, code, redirect string) (account.AdminIdentity, error) {
	config := s.adminCLI.oauth
	config.RedirectURL = redirect
	identity, err := s.adminGoogleIdentity(ctx, config, s.adminCLI.verifier, flow, code)
	if errors.Is(err, errAdminGoogleDenied) {
		return account.AdminIdentity{}, account.ErrInvalid
	}
	return identity, err
}
