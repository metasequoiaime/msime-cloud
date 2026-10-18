package account

import (
	"net/http"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
)

// candidateUpload 是发布与替换共同接受的包内容；作品 ID、可见性和分类由各自接口处理。
type candidateUpload struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Manifest    string            `json:"manifest"`
	Files       map[string][]byte `json:"files"`
}

// readCandidateUpload 延长本次上传的连接期限，先校验分类声明，再按上传大小上限读取请求体。
func readCandidateUpload(w http.ResponseWriter, r *http.Request, input any) (withCategory, ok bool) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(candidatePublishTimeout))
	_ = controller.SetWriteDeadline(time.Now().Add(candidatePublishTimeout + 5*time.Second))
	withCategory, ok = candidateIncludeCategory(r)
	if !ok {
		writeError(w, 400, "invalid_include")
		return false, false
	}
	return withCategory, readSized(w, r, input, maxCandidatePublishBytes)
}

// prepare 规范化标题与说明，依次校验元数据、文件和清单；摘要使用原始包字节，图片解码留在重试与限流检查之后。
func (input *candidateUpload) prepare(requireLicense bool) (pkg skins.Package, digest, code string) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if !resourceText(input.Name, 1, 32, false) || !resourceText(input.Description, 0, 280, true) {
		return pkg, "", "invalid_skin_metadata"
	}
	if code := validCandidateFiles(input.Manifest, input.Files); code != "" {
		return pkg, "", code
	}
	digest = candidateRequestDigest(input.Name, input.Description, input.Manifest, input.Files)
	pkg, code = validCandidatePackage(input.Manifest, input.Files, requireLicense)
	return pkg, digest, code
}

// screenCandidateUpload 先审核文本，再持有公共图片槽清理图片；失败时写出响应，成功时返回待复核标记和清理后的文件。
func (a *Service) screenCandidateUpload(w http.ResponseWriter, r *http.Request, input candidateUpload, preview string) (clean map[string][]byte, flag *string, ok bool) {
	flag, ok = a.screenUpload(w, r, input.Name, input.Description, input.Manifest)
	if !ok {
		return nil, nil, false
	}
	var code string
	if !withCandidateImageSlot(r.Context(), func() { clean, code = sanitizeCandidateImages(input.Files, preview) }) {
		writeError(w, 503, "candidate_skin_busy")
		return nil, nil, false
	}
	if code != "" {
		writeError(w, 400, code)
		return nil, nil, false
	}
	return clean, flag, true
}
