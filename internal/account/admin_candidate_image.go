package account

import (
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
)

// adminCandidateSkinImage 为实际外观预览提供包内背景和装饰图片，路径仍须通过资源校验。
func (a *Service) adminCandidateSkinImage(w http.ResponseWriter, r *http.Request, id string) {
	path := r.URL.Query().Get("path")
	if !skins.SafeResource(path, 256) || candidateExtension(path) == "" {
		writeError(w, 400, "invalid_path")
		return
	}
	a.adminCandidateSkinPicture(w, r, id, path)
}
