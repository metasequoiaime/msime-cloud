package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/oauth2"
)

// 认证拒绝与成员存储错误分开，供网页和 CLI 保留各自的错误响应。
var errAdminGoogleDenied = errors.New("admin Google identity denied")

// adminGoogleIdentity 按所选客户端交换授权码，校验身份及管理员邮箱。
func (s *Server) adminGoogleIdentity(ctx context.Context, config oauth2.Config, verifier account.Verifier, flow account.AdminLoginFlow, code string) (account.AdminIdentity, error) {
	tokens, err := config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, s.client), code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return account.AdminIdentity{}, errAdminGoogleDenied
	}
	raw, ok := tokens.Extra("id_token").(string)
	if !ok || len(raw) > 16384 {
		return account.AdminIdentity{}, errAdminGoogleDenied
	}
	token, err := verifier.Verify(ctx, raw)
	if err != nil || token.Subject == "" || len(token.Subject) > 255 || token.IssuedAt.IsZero() || token.IssuedAt.After(time.Now().Add(30*time.Second)) || subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(flow.Nonce)) != 1 {
		return account.AdminIdentity{}, errAdminGoogleDenied
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Name     string `json:"name"`
	}
	if token.Claims(&claims) != nil || !claims.Verified {
		return account.AdminIdentity{}, errAdminGoogleDenied
	}
	allowed, err := s.adminEmailAllowed(ctx, claims.Email)
	if err != nil {
		return account.AdminIdentity{}, err
	}
	if !allowed {
		return account.AdminIdentity{}, errAdminGoogleDenied
	}
	return account.AdminIdentity{Subject: token.Subject, Email: strings.ToLower(claims.Email), Name: claims.Name}, nil
}
