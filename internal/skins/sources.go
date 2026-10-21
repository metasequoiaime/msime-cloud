package skins

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	checksum "github.com/metasequoiaime/MSIME-Backend/internal/digest"
)

// Database is the stored-package source, implemented by the account store. CandidateSkin and CandidateSkinResource return ErrNotFound for an id or path that is absent or unpublished; any other error means the database could not answer.
type Database interface {
	CandidateSkins(ctx context.Context) ([]Stored, error)
	CandidateSkin(ctx context.Context, id string) (Stored, error)
	CandidateSkinResource(ctx context.Context, id, path string) ([]byte, error)
}

// Sources merges the three package sources behind /v1/skins: the embedded builtins, the read-only skins_root directory (both in the Windows dialect) and the database (in the client dialect). Builtin ids cannot be taken by either other source. An id present in both skins_root and the database is served by neither: it is counted once in invalid_packages and its detail and resources are 404, so an operator sees the clash instead of one copy silently winning. Unpublished database rows are ignored entirely.
type Sources struct {
	Root string
	DB   Database
}

var errConflict = invalid("id exists in both skins_root and the database")

// inRoot reports whether skins_root has a package directory named id, by the same test rootIDs lists with.
func inRoot(root, id string) (bool, error) {
	if root == "" {
		return false, nil
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	info, err := dir.Lstat(id)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func (s Sources) Catalog(ctx context.Context, layout, theme string) ([]Package, int, error) {
	if s.DB == nil {
		return Catalog(s.Root, layout, theme)
	}
	stored, err := s.DB.CandidateSkins(ctx)
	if err != nil {
		return nil, 0, err
	}
	found, err := rootIDs(s.Root)
	if err != nil {
		return nil, 0, err
	}
	type entry struct {
		id     string
		stored *Stored
	}
	entries := []entry{}
	for _, id := range builtinIDs {
		entries = append(entries, entry{id: id})
	}
	invalidCount := 0
	for i := range stored {
		if slices.Contains(found, stored[i].ID) {
			invalidCount++
			continue
		}
		entries = append(entries, entry{stored[i].ID, &stored[i]})
	}
	for _, id := range found {
		if !slices.ContainsFunc(stored, func(st Stored) bool { return st.ID == id }) {
			entries = append(entries, entry{id: id})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.id, b.id) })
	packages := []Package{}
	for _, e := range entries {
		var p Package
		if e.stored != nil {
			p, err = ParseStored(*e.stored)
		} else {
			p, err = Load(s.Root, e.id)
		}
		if err != nil {
			invalidCount++
			continue
		}
		if p.Matches(layout, theme) {
			packages = append(packages, p)
		}
	}
	return packages, invalidCount, nil
}

// load resolves id to its package. fromDB is false when the package came (or would come) from builtins or skins_root.
func (s Sources) load(ctx context.Context, id string) (p Package, stored Stored, fromDB bool, err error) {
	if Builtin(id) || !SafeID(id) || s.DB == nil {
		p, err = Load(s.Root, id)
		return p, stored, false, err
	}
	stored, err = s.DB.CandidateSkin(ctx, id)
	if errors.Is(err, ErrNotFound) {
		p, err = Load(s.Root, id)
		return p, stored, false, err
	}
	if err != nil {
		return p, stored, true, err
	}
	clash, err := inRoot(s.Root, id)
	if err != nil {
		return p, stored, true, err
	}
	if clash {
		return p, stored, true, errConflict
	}
	p, err = ParseStored(stored)
	return p, stored, true, err
}

func (s Sources) Load(ctx context.Context, id string) (Package, error) {
	p, _, _, err := s.load(ctx, id)
	return p, err
}

// ReadResource serves a file listed by the package's current manifest. A database file is checked against the digest the listing reported, so a row replaced between the two reads is refused rather than served under the old digest.
func (s Sources) ReadResource(ctx context.Context, id, name string) ([]byte, string, error) {
	if Builtin(id) || s.DB == nil {
		return ReadResource(s.Root, id, name)
	}
	p, stored, fromDB, err := s.load(ctx, id)
	if !fromDB {
		return ReadResource(s.Root, id, name)
	}
	if err != nil {
		return nil, "", err
	}
	i := slices.IndexFunc(p.Resources, func(r Resource) bool { return r.Path == name })
	if i < 0 {
		return nil, "", ErrNotFound
	}
	if name == "skin.toml" {
		return stored.Manifest, p.Resources[i].MediaType, nil
	}
	raw, err := s.DB.CandidateSkinResource(ctx, id, name)
	if err != nil {
		return nil, "", err
	}
	sum := checksum.SHA256(raw)
	if len(raw) != p.Resources[i].Size || sum != p.Resources[i].SHA256 {
		return nil, "", ErrNotFound
	}
	return raw, p.Resources[i].MediaType, nil
}
