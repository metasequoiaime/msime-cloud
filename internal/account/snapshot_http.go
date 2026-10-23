package account

import (
	"bufio"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpdeadline"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

const snapshotRestoreBytes int64 = 512 << 20
const snapshotRestoreTimeout = 2 * time.Minute

var snapshotRestoreSlot = make(chan struct{}, 1)

func (a *Service) restoreDictionarySnapshot(w http.ResponseWriter, r *http.Request) {
	user, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-ndjson" {
		writeError(w, 415, "ndjson_required")
		return
	}
	values := r.URL.Query()
	revisions := values["revision"]
	if len(values) != 1 || len(revisions) != 1 || revisions[0] == "" {
		writeError(w, 400, "snapshot_revision_required")
		return
	}
	expected, err := strconv.ParseInt(revisions[0], 10, 64)
	if err != nil || expected < 0 {
		writeError(w, 400, "invalid_snapshot_revision")
		return
	}
	if r.ContentLength > snapshotRestoreBytes {
		writeError(w, 413, "snapshot_too_large")
		return
	}
	if err = a.store.Rate(r.Context(), "snapshot-restore:"+user.UserID, 5, time.Minute); err != nil {
		a.error(w, err)
		return
	}
	select {
	case snapshotRestoreSlot <- struct{}{}:
		defer func() { <-snapshotRestoreSlot }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, 503, "snapshot_restore_busy")
		return
	}
	// 仅延长本次上传的连接期限。
	httpdeadline.Extend(w, snapshotRestoreTimeout, snapshotRestoreTimeout+5*time.Second)
	r.Body = http.MaxBytesReader(w, r.Body, snapshotRestoreBytes)
	revision, err := a.store.RestoreDictionarySnapshot(r.Context(), user.UserID, expected, r.Body, a.engine)
	if err != nil {
		switch {
		case httpjson.IsBodyTooLarge(err):
			writeError(w, 413, "snapshot_too_large")
		case errors.Is(err, errInvalidSnapshot), errors.Is(err, bufio.ErrTooLong):
			writeError(w, 400, "invalid_dictionary_snapshot")
		default:
			a.dictionaryError(w, err)
		}
		return
	}
	write(w, 200, map[string]any{"revision": revision, "reset": true})
}
