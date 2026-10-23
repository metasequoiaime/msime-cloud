package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// voiceContributionsPath 是用户自愿上传语音样本的接口。需要用户会话（匿名账号也是用户），配置的客户端令牌不能上传。
const voiceContributionsPath = "/v1/voice/contributions"

const (
	// voiceContributionAudioBytes 是一段音频的上限，60 秒 16 kHz 单声道 16 位 PCM 约 1.9 MiB。
	voiceContributionAudioBytes = 2 << 20
	// voiceContributionPayloadBytes 是 payload JSON 的上限。
	voiceContributionPayloadBytes = 16 << 10
	// voiceContributionMaxMS 是客户端声明时长的上限；服务端不解析音频的实际时长。
	voiceContributionMaxMS = 60000
	// voiceContributionTranscriptRunes 是识别文本的上限。
	voiceContributionTranscriptRunes = 2000
	// voiceContributionsPerHour 是每个用户每小时能上传的次数（auth_rates，所有副本共享）。
	voiceContributionsPerHour = 30
)

type voiceContributionPayload struct {
	Language   string `json:"language"`
	Provider   string `json:"provider"`
	DurationMS int    `json:"duration_ms"`
	Transcript string `json:"transcript"`
	AppVersion string `json:"app_version"`
}

// voiceAudioMime 按文件头判断音频格式：WAV 要通过与转写接口相同的 RIFF 结构校验，Ogg 只认 OggS 起始页。其他格式返回空字符串。
func voiceAudioMime(audio []byte) string {
	switch {
	case validWAV(audio):
		return "audio/wav"
	case len(audio) >= 28 && bytes.HasPrefix(audio, []byte("OggS")) && audio[4] == 0:
		return "audio/ogg"
	default:
		return ""
	}
}

// voiceContribution 处理 `POST /v1/voice/contributions`：multipart，`payload` 为 JSON {language,provider,duration_ms,transcript,app_version}，`audio` 为 WAV 或 Ogg（≤2 MiB），声明的 `duration_ms` 为 1–60000。成功返回 201 {id}。音频和文本只存进数据库，保留 180 天，不写日志；实际时长由客户端在上传前确认，服务端不解析。
func (s *Server) voiceContribution(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		fail(w, 503, "service_disabled")
		return
	}
	owner, _ := r.Context().Value(skinJobOwnerKey{}).(string)
	user, isUser := strings.CutPrefix(owner, "user:")
	if !isUser || user == "" {
		fail(w, 401, "user_session_required")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "multipart/form-data" {
		fail(w, 415, "multipart_required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, voiceContributionAudioBytes+voiceContributionPayloadBytes+64<<10)
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "invalid_multipart")
		return
	}
	var payload *voiceContributionPayload
	var audio []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if httpjson.IsBodyTooLarge(err) {
			fail(w, 413, "contribution_too_large")
			return
		}
		if err != nil {
			fail(w, 400, "invalid_multipart")
			return
		}
		switch part.FormName() {
		case "payload":
			if payload != nil {
				fail(w, 400, "invalid_payload")
				return
			}
			raw, err := io.ReadAll(io.LimitReader(part, voiceContributionPayloadBytes+1))
			if err != nil || len(raw) > voiceContributionPayloadBytes {
				fail(w, 400, "invalid_payload")
				return
			}
			var v voiceContributionPayload
			if httpjson.DecodeValue(bytes.NewReader(raw), &v) != nil {
				fail(w, 400, "invalid_payload")
				return
			}
			payload = &v
		case "audio":
			if audio != nil {
				fail(w, 400, "invalid_audio")
				return
			}
			audio, err = io.ReadAll(io.LimitReader(part, voiceContributionAudioBytes+1))
			if httpjson.IsBodyTooLarge(err) || len(audio) > voiceContributionAudioBytes {
				fail(w, 413, "audio_too_large")
				return
			}
			if err != nil {
				fail(w, 400, "invalid_audio")
				return
			}
		default:
			fail(w, 400, "unexpected_part")
			return
		}
	}
	if payload == nil {
		fail(w, 400, "invalid_payload")
		return
	}
	transcript := strings.TrimSpace(payload.Transcript)
	if !textutil.ValidClientLabel(payload.Language) || !textutil.ValidClientLabel(payload.Provider) || !textutil.ValidClientLabel(payload.AppVersion) ||
		payload.DurationMS < 1 || payload.DurationMS > voiceContributionMaxMS ||
		!utf8.ValidString(transcript) || utf8.RuneCountInString(transcript) > voiceContributionTranscriptRunes || strings.ContainsRune(transcript, 0) {
		fail(w, 400, "invalid_payload")
		return
	}
	audioMime := voiceAudioMime(audio)
	if audioMime == "" {
		fail(w, 400, "invalid_audio")
		return
	}
	if err = s.accounts.RateLimit(r.Context(), "voice-contribution", user, voiceContributionsPerHour, time.Hour); err != nil {
		if errors.Is(err, account.ErrLimited) {
			w.Header().Set("Retry-After", "3600")
			fail(w, 429, "rate_limit_exceeded")
			return
		}
		fail(w, 503, "service_unavailable")
		return
	}
	id, err := s.accounts.SaveVoiceContribution(r.Context(), user, account.VoiceContribution{Language: payload.Language, Provider: payload.Provider, DurationMS: payload.DurationMS, Transcript: transcript, AppVersion: payload.AppVersion, AudioMime: audioMime, Audio: audio})
	if errors.Is(err, account.ErrBanned) {
		fail(w, 403, "account_banned")
		return
	}
	if errors.Is(err, account.ErrInvalid) {
		fail(w, 401, "invalid_credentials")
		return
	}
	if err != nil {
		slog.Error("voice contribution not stored", "reason", err.Error())
		fail(w, 503, "service_unavailable")
		return
	}
	respond(w, 201, map[string]string{"id": id})
}
