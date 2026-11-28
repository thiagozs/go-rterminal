package admin

import (
	"encoding/json"
	"net/http"

	"github.com/thiagozs/go-rterminal/internal/store"
)

// prefSpec lists the panel preferences an account can save, with their
// types and limits. Anything else is dropped, so the table cannot be used to
// store arbitrary data.
var prefSpec = map[string]struct {
	boolean  bool
	min, max int
}{
	"autoCloseTransfers": {boolean: true},
	"autoCloseSeconds":   {min: 1, max: 60},
	"uploadParallel":     {min: 1, max: 6},
	"confirmFolders":     {boolean: true},
	"skipGit":            {boolean: true},
	"skipNodeModules":    {boolean: true},
	"skipJunk":           {boolean: true},
	"keepExecutable":     {boolean: true},
}

// sanitizePrefs keeps the known preferences that have the right type and range.
func sanitizePrefs(in map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		spec, known := prefSpec[key]
		if !known {
			continue
		}
		if spec.boolean {
			if b, ok := value.(bool); ok {
				out[key] = b
			}
			continue
		}
		if n, ok := value.(float64); ok && n == float64(int(n)) && int(n) >= spec.min && int(n) <= spec.max {
			out[key] = int(n)
		}
	}
	return out
}

func (s *Server) savedPrefs(userID int64) (map[string]any, error) {
	raw, err := s.store.UserPrefs(userID)
	if err != nil {
		return nil, err
	}
	saved := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &saved)
	}
	return sanitizePrefs(saved), nil
}

// getPrefs returns what the account saved; an account that saved nothing gets {}.
func (s *Server) getPrefs(w http.ResponseWriter, _ *http.Request, admin *store.Admin) {
	prefs, err := s.savedPrefs(admin.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// putPrefs merges the given preferences into the saved ones and returns the result.
func (s *Server) putPrefs(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	var in map[string]any
	if !decodeLoose(w, r, &in) {
		return
	}
	prefs, err := s.savedPrefs(admin.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	for k, v := range sanitizePrefs(in) {
		prefs[k] = v
	}
	data, _ := json.Marshal(prefs)
	if err := s.store.SetUserPrefs(admin.ID, string(data)); err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}
