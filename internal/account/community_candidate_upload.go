package account

import (
	"net/http"
	"strings"

	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
)

// candidateUpload 是发布与替换共同接受的包内容；作品 ID、可见性和分类由各自接口处理。
type candidateUpload struct {
	extendedResources bool
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Manifest          string            `json:"manifest"`
	Files             map[string][]byte `json:"files"`
}

func (input *candidateUpload) setCandidateResources(extended bool) {
	input.extendedResources = extended
}

// readCandidateUpload 先校验分类声明，再按上传大小上限读取请求体。
func readCandidateUpload(w http.ResponseWriter, r *http.Request, input any, resourcesEnabled bool) (withCategory, ok bool) {
	query := r.URL.Query()
	withCategory, ok = candidateIncludeCategory(query)
	if !ok {
		writeError(w, 400, "invalid_include")
		return false, false
	}
	extended, ok := candidateResources(w, query, resourcesEnabled)
	if !ok {
		return false, false
	}
	if !readSized(w, r, input, maxCandidatePublishBytes) {
		return false, false
	}
	if upload, ok := input.(interface{ setCandidateResources(bool) }); ok {
		upload.setCandidateResources(extended && resourcesEnabled)
	}
	return withCategory, true
}

// prepare 规范化标题与说明，依次校验元数据、文件和清单；摘要使用原始包字节，图片解码留在重试与限流检查之后。
func (input *candidateUpload) prepare(requireLicense bool) (pkg skins.Package, digest, code string) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if !validCommunityTextMetadata(input.Name, input.Description) {
		return pkg, "", "invalid_skin_metadata"
	}
	if code := validCandidateResourceFiles(input.Manifest, input.Files, input.extendedResources); code != "" {
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
