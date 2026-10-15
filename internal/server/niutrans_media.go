package server

// NiuTrans' document, image and voice APIs use the same signed, asynchronous
// file protocol. This adapter keeps that protocol behind the MSIME device
// token so clients never receive an API key or an upstream URL.

import (
	"crypto/md5" // #nosec G501 -- required by the NiuTrans protocol.
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	niuTransFileBytes     = 64 << 20
	niuTransDownloadBytes = 64 << 20
	niuTransResponseBytes = 1 << 20
	// 单次上传只需要文件、语言和少量可选参数，限制部件数避免大量小字段耗尽内存。
	niuTransMaxParts = 32
)

func niuTransAuth(params map[string]string, apiKey string) string {
	keys := make([]string, 0, len(params)+1)
	for key, value := range params {
		if key != "authStr" && key != "apikey" && value != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, "apikey="+apiKey)
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&"))) // #nosec G401 -- required by provider.
	return hex.EncodeToString(sum[:])
}

func niuTransEndpointReady(e NiuTransEndpoint) bool {
	return e.URL != "" && e.appID != "" && e.apiKey != ""
}

func (s *Server) niuTransEndpoint(w http.ResponseWriter, e NiuTransEndpoint) bool {
	if !niuTransEndpointReady(e) {
		fail(w, http.StatusServiceUnavailable, "niutrans_api_not_configured")
		return false
	}
	return true
}

func niuTransJSON(w http.ResponseWriter, r *http.Request, body []byte) {
	if !json.Valid(body) {
		upstreamError(w, r, errors.New("invalid upstream JSON"))
		return
	}
	respond(w, http.StatusOK, json.RawMessage(body))
}

func (s *Server) niuTransUpload(w http.ResponseWriter, r *http.Request, e NiuTransEndpoint, kind string) {
	if !s.niuTransEndpoint(w, e) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, niuTransFileBytes+1<<20)
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusUnsupportedMediaType, "multipart_required")
		return
	}
	var body *os.File
	var mw *multipart.Writer
	var fileSize int64
	filename := "upload.bin"
	fields := map[string]string{}
	seen := map[string]bool{}
	parts := 0
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			fail(w, http.StatusBadRequest, "invalid_multipart")
			return
		}
		parts++
		if parts > niuTransMaxParts {
			fail(w, http.StatusRequestEntityTooLarge, "too_many_parts")
			return
		}
		name := part.FormName()
		if name == "" || seen[name] {
			fail(w, http.StatusBadRequest, "duplicate_field")
			return
		}
		seen[name] = true
		if name == "file" {
			if part.FileName() != "" {
				filename = part.FileName()
			}
			// 先在私有临时文件中构造 multipart，后续字段仍可参与签名，不在内存中保留整个上传。
			body, err = os.CreateTemp("", "msime-niutrans-*")
			if err != nil {
				fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
				return
			}
			defer os.Remove(body.Name())
			defer body.Close()
			mw = multipart.NewWriter(body)
			file, createErr := mw.CreateFormFile("file", filename)
			if createErr != nil {
				fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
				return
			}
			fileSize, err = io.Copy(file, io.LimitReader(part, niuTransFileBytes+1))
			var storageErr *os.PathError
			if errors.As(err, &storageErr) {
				fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
				return
			}
			if err != nil || fileSize == 0 || fileSize > niuTransFileBytes {
				fail(w, http.StatusRequestEntityTooLarge, "file_too_large_or_empty")
				return
			}
		} else {
			value, readErr := io.ReadAll(io.LimitReader(part, 257))
			if readErr != nil || len(value) == 0 || len(value) > 256 || strings.ContainsAny(string(value), "\r\n") {
				fail(w, http.StatusBadRequest, "invalid_field")
				return
			}
			fields[name] = string(value)
		}
		_ = part.Close()
	}
	if body == nil || fields["from"] == "" || fields["to"] == "" {
		fail(w, http.StatusBadRequest, "file_from_to_required")
		return
	}
	params := map[string]string{
		"from": fields["from"], "to": fields["to"], "appId": e.appID,
		"timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	for _, key := range []string{"realmCode", "termId", "memoryId", "processingMode", "translationEngine"} {
		if fields[key] != "" {
			params[key] = fields[key]
		}
	}
	params["authStr"] = niuTransAuth(params, e.apiKey)
	for key, value := range params {
		if key != "authStr" && value != "" {
			if err := mw.WriteField(key, value); err != nil {
				fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
				return
			}
		}
	}
	if err := mw.WriteField("authStr", params["authStr"]); err != nil {
		fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
		return
	}
	if err := mw.Close(); err != nil {
		fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
		return
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		fail(w, http.StatusServiceUnavailable, "upload_storage_unavailable")
		return
	}
	mr, call := metered(r, "niutrans_"+kind, 0)
	b, err := s.niuTransRequest(mr, e.URL, http.MethodPost, mw.FormDataContentType(), body)
	if err == nil {
		b, err = niuTransUploadResult(r, e, "niutrans_"+kind, b)
	}
	s.settleMeter(call, err == nil)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	niuTransJSON(w, r, b)
}

func (s *Server) niuTransDocumentUpload(w http.ResponseWriter, r *http.Request) {
	s.niuTransUpload(w, r, s.config.NiuTrans.Document, "document")
}
func (s *Server) niuTransImageUpload(w http.ResponseWriter, r *http.Request) {
	s.niuTransUpload(w, r, s.config.NiuTrans.Image, "image")
}
func (s *Server) niuTransVoiceUpload(w http.ResponseWriter, r *http.Request) {
	s.niuTransUpload(w, r, s.config.NiuTrans.Voice, "voice")
}

// niuTransFileRequest 转发文件任务的状态、终止、删除或下载请求，按 service 记录服务指标。
func (s *Server) niuTransFileRequest(w http.ResponseWriter, r *http.Request, e NiuTransEndpoint, service, operation, fileNo string) {
	if !s.niuTransEndpoint(w, e) {
		return
	}
	if fileNo == "" || len(fileNo) > 512 || (!strings.HasPrefix(fileNo, niuTransTaskPrefix) && len(fileNo) > 256) || strings.ContainsAny(fileNo, "/\\\x00\r\n") {
		fail(w, http.StatusBadRequest, "invalid_file_no")
		return
	}
	task := fileNo
	owner, _ := r.Context().Value(skinJobOwnerKey{}).(string)
	fileNo, ok := niuTransTaskFileNo(e, service, owner, fileNo)
	if !ok {
		fail(w, http.StatusNotFound, "niutrans_task_not_found")
		return
	}
	params := map[string]string{"appId": e.appID, "timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10)}
	if operation == "download" {
		typ := r.URL.Query().Get("type")
		if typ == "" {
			typ = "1"
		}
		if typ != "0" && typ != "1" && typ != "2" && typ != "3" && typ != "4" && typ != "5" {
			fail(w, http.StatusBadRequest, "invalid_download_type")
			return
		}
		params["type"] = typ
	}
	params["authStr"] = niuTransAuth(params, e.apiKey)
	base, err := url.Parse(e.URL)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	// `URL.Path` 保存解码后的任务编号，`URL.String` 会统一转义一次。
	base.Path = strings.TrimSuffix(base.Path, "/upload") + "/" + operation + "/" + fileNo
	query := base.Query()
	for key, value := range params {
		query.Set(key, value)
	}
	base.RawQuery = query.Encode()
	mr, call := metered(r, service, 0)
	if operation == "download" {
		s.niuTransDownload(w, mr, base.String(), call)
		return
	}
	b, err := s.niuTransRequest(mr, base.String(), r.Method, "", nil)
	if err == nil {
		b, err = niuTransFileResult(b, task)
	}
	s.settleMeter(call, err == nil)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	niuTransJSON(w, r, b)
}

// niuTransDownload 以固定大小缓冲区转发文件，下载上限独立于 JSON 响应上限。
func (s *Server) niuTransDownload(w http.ResponseWriter, r *http.Request, target string, call *meterCall) {
	started := time.Now()
	var err error
	defer func() {
		captureMeter(r.Context(), started, err)
		s.settleMeter(call, err == nil)
	}()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = io.ErrUnexpectedEOF
	} else if resp.ContentLength > niuTransDownloadBytes {
		err = errors.New("upstream response too large")
	}
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, io.LimitReader(resp.Body, niuTransDownloadBytes))
	if err == nil {
		// 再读一个字节区分恰好达到上限与超限，额外字节不发送给客户端。
		var extra int64
		extra, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		if err == nil && extra != 0 {
			err = errors.New("upstream response too large")
		}
	}
	if err != nil {
		// 已发送文件响应时中断连接，避免残缺文件被当作完整下载，也不追加 JSON。
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) niuTransDocumentStatus(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Document, "niutrans_document", "status", r.PathValue("file_no"))
}
func (s *Server) niuTransDocumentInterrupt(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Document, "niutrans_document", "interrupt", r.PathValue("file_no"))
}
func (s *Server) niuTransDocumentDelete(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Document, "niutrans_document", "delete", r.PathValue("file_no"))
}
func (s *Server) niuTransDocumentDownload(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Document, "niutrans_document", "download", r.PathValue("file_no"))
}

func (s *Server) niuTransImageStatus(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Image, "niutrans_image", "status", r.PathValue("file_no"))
}
func (s *Server) niuTransImageInterrupt(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Image, "niutrans_image", "interrupt", r.PathValue("file_no"))
}
func (s *Server) niuTransImageDownload(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Image, "niutrans_image", "download", r.PathValue("file_no"))
}
func (s *Server) niuTransVoiceStatus(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Voice, "niutrans_voice", "status", r.PathValue("file_no"))
}
func (s *Server) niuTransVoiceInterrupt(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Voice, "niutrans_voice", "interrupt", r.PathValue("file_no"))
}
func (s *Server) niuTransVoiceDownload(w http.ResponseWriter, r *http.Request) {
	s.niuTransFileRequest(w, r, s.config.NiuTrans.Voice, "niutrans_voice", "download", r.PathValue("file_no"))
}

func (s *Server) niuTransResources(w http.ResponseWriter, r *http.Request) {
	if !s.niuTransEndpoint(w, s.config.NiuTrans.Resource) {
		return
	}
	// Resource management is an account-level API. Keep the upstream response
	// opaque while forwarding the signed request; clients select the documented
	// resource action with `action` and may provide only scalar parameters.
	action := r.URL.Query().Get("action")
	if action == "" || len(action) > 64 || strings.ContainsAny(action, "/\\\r\n") {
		fail(w, http.StatusBadRequest, "resource_action_required")
		return
	}
	params := map[string]string{"appId": s.config.NiuTrans.Resource.appID, "timestamp": strconv.FormatInt(time.Now().UnixMilli(), 10), "action": action}
	params["authStr"] = niuTransAuth(params, s.config.NiuTrans.Resource.apiKey)
	u, err := url.Parse(s.config.NiuTrans.Resource.URL)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + action
	q := u.Query()
	for key, value := range params {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()
	b, err := s.niuTransRequest(r, u.String(), r.Method, "", nil)
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	niuTransJSON(w, r, b)
}

func (s *Server) niuTransRequest(r *http.Request, target, method, contentType string, body io.Reader) ([]byte, error) {
	b, _, err := s.niuTransRequestWithType(r, target, method, contentType, body)
	return b, err
}

// niuTransRequestWithType sends one NiuTrans call; for a request tagged by metered the exchange is captured for the console's service metrics.
func (s *Server) niuTransRequestWithType(r *http.Request, target, method, contentType string, body io.Reader) ([]byte, string, error) {
	started := time.Now()
	b, contentType, err := s.sendNiuTrans(r, target, method, contentType, body)
	captureMeter(r.Context(), started, err)
	return b, contentType, err
}

func (s *Server) sendNiuTrans(r *http.Request, target, method, contentType string, body io.Reader) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, target, body)
	if err != nil {
		return nil, "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// 上传文件已完整写入并回到开头，显式保留固定长度，兼容不接受 chunked 的上游。
	if file, ok := body.(*os.File); ok {
		info, statErr := file.Stat()
		if statErr != nil {
			return nil, "", statErr
		}
		req.ContentLength = info.Size()
		// 传输层可在返回后异步关闭请求体；临时文件由上传处理函数独占关闭，避免 Windows 在并发关闭尚未完成时删除失败。
		req.Body = io.NopCloser(file)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, niuTransResponseBytes+1))
	if err != nil || len(data) > niuTransResponseBytes {
		return nil, "", errors.New("upstream response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", io.ErrUnexpectedEOF
	}
	return data, resp.Header.Get("Content-Type"), nil
}
