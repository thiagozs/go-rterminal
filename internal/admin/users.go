package admin

import (
	"net/http"

	"github.com/thiagozs/go-rterminal/internal/store"
)

type userInput struct {
	Username string  `json:"username"`
	Role     string  `json:"role"`
	GroupIDs []int64 `json:"group_ids"`
}

type userUpdate struct {
	Role     string  `json:"role"`
	GroupIDs []int64 `json:"group_ids"`
}

// userCreated carries the temporary password, shown only once.
type userCreated struct {
	User              store.User `json:"user"`
	TemporaryPassword string     `json:"temporary_password"`
}

// temporaryPassword is random and long enough to pass the password policy;
// the account must replace it at the first login.
func temporaryPassword() (string, error) {
	token, err := store.NewToken()
	if err != nil {
		return "", err
	}
	return token[:20], nil
}

func (s *Server) listUsers(w http.ResponseWriter, _ *http.Request, _ *store.Admin) {
	users, err := s.store.ListUsers()
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, actor *store.Admin) {
	var in userInput
	if !decode(w, r, &in) {
		return
	}
	password, err := temporaryPassword()
	if err != nil {
		s.storeError(w, err)
		return
	}
	created, err := s.store.CreateUser(in.Username, password, in.Role, in.GroupIDs, true)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(actor.Username, "user_create", created.Username, "role "+created.Role)
	user := store.User{Admin: *created, GroupIDs: []int64{}}
	if created.Role == store.RoleOperator && in.GroupIDs != nil {
		ids, err := s.store.UserGroupIDs(created.ID)
		if err != nil {
			s.storeError(w, err)
			return
		}
		user.GroupIDs = ids
	}
	writeJSON(w, http.StatusCreated, userCreated{User: user, TemporaryPassword: password})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, actor *store.Admin) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var in userUpdate
	if !decode(w, r, &in) {
		return
	}
	target, err := s.store.GetUser(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.store.UpdateUser(id, in.Role, in.GroupIDs); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(actor.Username, "user_update", target.Username, "role "+in.Role)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, actor *store.Admin) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if id == actor.ID {
		writeError(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	target, err := s.store.GetUser(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(actor.Username, "user_delete", target.Username, "")
	w.WriteHeader(http.StatusNoContent)
}

// resetUser issues a new temporary password and ends the account's sessions.
func (s *Server) resetUser(w http.ResponseWriter, r *http.Request, actor *store.Admin) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	target, err := s.store.GetUser(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	password, err := temporaryPassword()
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.store.ResetAdminPassword(target.Username, password); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(actor.Username, "user_password_reset", target.Username, "sessions ended")
	writeJSON(w, http.StatusOK, map[string]string{"temporary_password": password})
}
