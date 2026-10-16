package account

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

const (
	// maxAvatarUploadBytes bounds an uploaded avatar before it is decoded.
	maxAvatarUploadBytes = 1 << 20
	// maxAvatarSourceSide bounds the decoded image, so a small file that declares an enormous canvas is refused before its pixels are allocated.
	maxAvatarSourceSide = 4096
	// avatarSide is the stored avatar: one square size, re-encoded, whatever was uploaded.
	avatarSide = 256
)

var errInvalidAvatar = errors.New("invalid_avatar")

// errAvatarCommitUncertain 表示头像登记的提交结果未知，对象可能已被数据库引用，不能删除。
var errAvatarCommitUncertain = errors.New("avatar commit outcome unknown")

// googleAvatarHost reports whether a stored Google picture URL may be handed to clients: only HTTPS on Google's own image host, so a client fetching it never reaches anywhere else.
func googleAvatarHost(picture string) bool {
	u, e := url.Parse(picture)
	if e != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := u.Hostname()
	return host == "googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com")
}

// present fills the fields of a user that depend on this server's configuration. A custom avatar wins over the Google picture; a user with neither has no avatar_url, and clients draw the first character of the nickname instead.
func (a *Service) present(u User) User {
	switch {
	case u.avatarKey != "" && a.config.Avatars.PublicBaseURL != "":
		u.AvatarURL = strings.TrimSuffix(a.config.Avatars.PublicBaseURL, "/") + "/" + u.avatarKey
	case googleAvatarHost(u.googlePicture):
		u.AvatarURL = u.googlePicture
	}
	return u
}

// normalizeAvatar 在完整解码前校验尺寸和 JPEG 扫描数，再将 PNG 或 JPEG 居中裁为正方形，透明像素铺白底并重编码为不含元数据的 256×256 JPEG。
func normalizeAvatar(data []byte) ([]byte, error) {
	config, format, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > maxAvatarSourceSide || config.Height > maxAvatarSourceSide {
		return nil, errInvalidAvatar
	}
	if format == "jpeg" && jpegScans(data) > maxJPEGScans {
		return nil, errInvalidAvatar
	}
	source, _, e := image.Decode(bytes.NewReader(data))
	if e != nil {
		return nil, errInvalidAvatar
	}
	bounds := source.Bounds()
	side := min(bounds.Dx(), bounds.Dy())
	crop := image.Rect(0, 0, side, side).Add(bounds.Min).Add(image.Pt((bounds.Dx()-side)/2, (bounds.Dy()-side)/2))
	out := image.NewRGBA(image.Rect(0, 0, avatarSide, avatarSide))
	// JPEG has no alpha, so a transparent PNG is flattened onto white rather than onto black.
	draw.Draw(out, out.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(out, out.Bounds(), source, crop, xdraw.Over, nil)
	var encoded bytes.Buffer
	if e = jpeg.Encode(&encoded, out, &jpeg.Options{Quality: 88}); e != nil {
		return nil, e
	}
	return encoded.Bytes(), nil
}

// putAvatar replaces the user's custom avatar with an uploaded PNG or JPEG.
func (a *Service) putAvatar(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if a.avatars == nil {
		writeError(w, 503, "avatar_upload_disabled")
		return
	}
	mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	mediaType = strings.Trim(mediaType, " \t")
	// 仅按 ASCII 忽略大小写匹配类型名，避免接受 Unicode 空白或折叠后的非 ASCII 字符。
	isPNG := len(mediaType) == len("image/png") && strings.EqualFold(mediaType, "image/png")
	isJPEG := len(mediaType) == len("image/jpeg") && strings.EqualFold(mediaType, "image/jpeg")
	if !isPNG && !isJPEG {
		writeError(w, 415, "unsupported_media_type")
		return
	}
	if e := a.store.Rate(r.Context(), "avatar:"+p.UserID, 20, time.Hour); e != nil {
		a.error(w, e)
		return
	}
	data, e := io.ReadAll(io.LimitReader(r.Body, maxAvatarUploadBytes+1))
	if e != nil {
		writeError(w, 400, "invalid_avatar")
		return
	}
	if len(data) > maxAvatarUploadBytes {
		writeError(w, 413, "avatar_too_large")
		return
	}
	var normalized []byte
	if !withCandidateImageSlot(r.Context(), func() { normalized, e = normalizeAvatar(data) }) {
		writeError(w, 503, "server_busy")
		return
	}
	if e != nil {
		writeError(w, 400, "invalid_avatar")
		return
	}
	// A fresh random key per upload is what lets the public URL be cached forever, and it says nothing about whose avatar it is.
	key := "avatars/" + randomToken() + ".jpg"
	if e = a.avatars.Put(r.Context(), key, normalized, "image/jpeg"); e != nil {
		// 存储 SDK 的错误可能包含上游响应正文、请求地址或签名，只记录固定的操作原因。
		slog.Error("头像上传失败", "reason", "storage_write_failed")
		writeError(w, 502, "avatar_storage_unavailable")
		return
	}
	previous, u, ids, e := a.store.SetAvatarKeyAndMe(r.Context(), p.UserID, key)
	if e != nil {
		// 提交响应丢失时，数据库可能已经引用新对象；只有提交前确定失败才清理。
		if !errors.Is(e, errAvatarCommitUncertain) {
			a.deleteAvatarObject(key)
		}
		a.error(w, e)
		return
	}
	a.deleteAvatarObject(previous)
	write(w, 200, map[string]any{"user": a.present(u), "identities": ids})
}

// deleteAvatar removes the user's custom avatar; the Google picture, if any, shows again.
func (a *Service) deleteAvatar(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	previous, e := a.store.SetAvatarKey(r.Context(), p.UserID, "")
	if e != nil {
		a.error(w, e)
		return
	}
	a.deleteAvatarObject(previous)
	w.WriteHeader(204)
}

// deleteAvatarObject removes an object that no user points at any more. Best-effort: the user's change has already been committed, and an orphaned object costs storage, not correctness.
func (a *Service) deleteAvatarObject(key string) {
	if key == "" || a.avatars == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := a.avatars.Delete(ctx, key); e != nil {
		slog.Warn("旧头像删除失败", "reason", "storage_delete_failed")
	}
}
