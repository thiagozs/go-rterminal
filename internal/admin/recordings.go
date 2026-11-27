package admin

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/thiagozs/go-rterminal/internal/store"
)

// recordingView adds the duration, which the UI would otherwise compute.
type recordingView struct {
	store.Recording
	DurationSeconds *int64 `json:"duration_seconds"` // nil while recording
}

func (s *Server) recordingsOff(w http.ResponseWriter) bool {
	if s.recordings == nil {
		writeError(w, http.StatusNotFound, "recordings are disabled on this server")
		return true
	}
	return false
}

func (s *Server) listRecordings(w http.ResponseWriter, r *http.Request, _ *store.Admin) {
	if s.recordingsOff(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.store.ListRecordings(r.URL.Query().Get("machine"), limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	views := make([]recordingView, 0, len(list))
	for _, rec := range list {
		v := recordingView{Recording: rec}
		if rec.EndedAt != nil {
			d := int64(rec.EndedAt.Sub(rec.StartedAt).Seconds())
			v.DurationSeconds = &d
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, views)
}

// recordingCast serves the asciicast file. Watching a recording is itself audited.
func (s *Server) recordingCast(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.recordingsOff(w) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.store.GetRecording(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	file, err := s.recordings.Open(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "recording file is gone")
			return
		}
		s.storeError(w, err)
		return
	}
	defer file.Close()
	s.audit(admin.Username, "recording_view", rec.MachineName, "recording "+id)
	h := w.Header()
	h.Set("Content-Type", "application/x-asciicast")
	h.Set("Content-Disposition", `attachment; filename="`+id+`.cast"`)
	h.Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, file)
}

func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.recordingsOff(w) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.store.GetRecording(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.recordings.Delete(id); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "recording_delete", rec.MachineName, "recording "+id+" of "+rec.Username)
	w.WriteHeader(http.StatusNoContent)
}
