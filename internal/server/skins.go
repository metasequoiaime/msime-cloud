package server

import (
	"errors"
	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
	"net/http"
)

// skinSources merges builtins, skins_root and, when accounts are enabled, the database packages.
func (s *Server) skinSources() skins.Sources {
	return skins.Sources{Root: s.config.SkinsRoot, DB: s.accounts.SkinDatabase()}
}

// skinMissing tells a package or resource that does not exist, or is not valid, apart from a source that could not be read.
func skinMissing(err error) bool {
	return errors.Is(err, skins.ErrNotFound) || errors.Is(err, skins.ErrInvalid)
}

func (s *Server) skinCatalog(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	layout, theme := query.Get("layout"), query.Get("theme")
	for key := range query {
		if key != "layout" && key != "theme" {
			fail(w, 400, "invalid_skin_filter")
			return
		}
	}
	if (layout != "" && layout != "horizontal" && layout != "vertical") || (theme != "" && theme != "dark" && theme != "light") {
		fail(w, 400, "invalid_skin_filter")
		return
	}
	packages, invalid, err := s.skinSources().Catalog(r.Context(), layout, theme)
	if err != nil {
		fail(w, 503, "skin_catalog_unavailable")
		return
	}
	respond(w, 200, map[string]any{"skins": packages, "invalid_packages": invalid, "license_url": "/v1/skins/license", "source_url": "/v1/skins/source"})
}
func (s *Server) skinDetails(w http.ResponseWriter, r *http.Request) {
	p, err := s.skinSources().Load(r.Context(), r.PathValue("id"))
	if skinMissing(err) {
		fail(w, 404, "skin_not_found")
		return
	}
	if err != nil {
		fail(w, 503, "skin_catalog_unavailable")
		return
	}
	respond(w, 200, p)
}
func (s *Server) skinResource(w http.ResponseWriter, r *http.Request) {
	raw, kind, err := s.skinSources().ReadResource(r.Context(), r.PathValue("id"), r.PathValue("resource"))
	if skinMissing(err) {
		fail(w, 404, "skin_resource_not_found")
		return
	}
	if err != nil {
		fail(w, 503, "skin_catalog_unavailable")
		return
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Disposition", "attachment")
	w.Write(raw)
}
