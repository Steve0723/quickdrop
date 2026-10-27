package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

func (a *App) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}

	type item struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		Role       string `json:"role"`
		CreatedAt  int64  `json:"createdAt"`
		Disabled   bool   `json:"disabled"`
		ShareCount int    `json:"shareCount"`
	}

	items := []item{}
	a.store.Read(func(db *DB) {
		for _, u := range db.Users {
			count := 0
			for _, sh := range db.Shares {
				if sh.OwnerID == u.ID {
					count++
				}
			}
			items = append(items, item{
				ID:         u.ID,
				Username:   u.Username,
				Role:       u.Role,
				CreatedAt:  u.CreatedAt,
				Disabled:   u.Disabled,
				ShareCount: count,
			})
		}
	})

	writeJSON(w, http.StatusOK, map[string]any{"users": items})
}

type createUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (a *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}

	var req createUserReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}

	username := strings.ToLower(strings.TrimSpace(req.Username))
	if !usernamePattern.MatchString(username) {
		errJSON(w, http.StatusBadRequest, "invalid_username",
			"用户名需 3-32 位，仅限小写字母、数字、下划线、点和连字符")
		return
	}
	if len(req.Password) < 6 {
		errJSON(w, http.StatusBadRequest, "password_too_short", "密码至少 6 位")
		return
	}

	role := "member"
	if req.Role == "admin" {
		role = "admin"
	}

	user := User{
		ID:        shortID(10),
		Username:  username,
		Password:  hashSecret(req.Password),
		Role:      role,
		CreatedAt: nowMs(),
	}

	taken := false
	_ = a.store.Update(func(db *DB) {
		for _, u := range db.Users {
			if u.Username == username {
				taken = true
				return
			}
		}
		db.Users = append(db.Users, user)
	})

	if taken {
		errJSON(w, http.StatusConflict, "username_taken", "用户名已存在")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"user": publicUser(user)})
}

type updateUserReq struct {
	Password *string `json:"password"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
}

func (a *App) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")

	var req updateUserReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}

	if req.Password != nil && *req.Password != "" && len(*req.Password) < 6 {
		errJSON(w, http.StatusBadRequest, "password_too_short", "密码至少 6 位")
		return
	}
	if req.Role != nil && *req.Role != "admin" && *req.Role != "member" {
		errJSON(w, http.StatusBadRequest, "invalid_role", "角色不合法")
		return
	}
	if req.Role != nil && id == admin.ID && *req.Role != "admin" {
		errJSON(w, http.StatusBadRequest, "cannot_demote_self", "不能修改自己的角色")
		return
	}
	if req.Disabled != nil && id == admin.ID && *req.Disabled {
		errJSON(w, http.StatusBadRequest, "cannot_disable_self", "不能禁用当前登录账号")
		return
	}

	var updated User
	var found bool

	_ = a.store.Update(func(db *DB) {
		i := findUserIndex(db, id)
		if i < 0 {
			return
		}
		found = true

		if req.Password != nil && *req.Password != "" {
			db.Users[i].Password = hashSecret(*req.Password)
			kept := db.Sessions[:0]
			for _, ss := range db.Sessions {
				if ss.UserID != id {
					kept = append(kept, ss)
				}
			}
			db.Sessions = kept
		}
		if req.Role != nil {
			db.Users[i].Role = *req.Role
		}
		if req.Disabled != nil {
			db.Users[i].Disabled = *req.Disabled
			if *req.Disabled {
				kept := db.Sessions[:0]
				for _, ss := range db.Sessions {
					if ss.UserID != id {
						kept = append(kept, ss)
					}
				}
				db.Sessions = kept
			}
		}
		updated = db.Users[i]
	})

	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(updated)})
}

func (a *App) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	if id == admin.ID {
		errJSON(w, http.StatusBadRequest, "cannot_delete_self", "不能删除当前登录账号")
		return
	}

	var stored []string
	var found bool

	_ = a.store.Update(func(db *DB) {
		i := findUserIndex(db, id)
		if i < 0 {
			return
		}
		found = true

		keptShares := db.Shares[:0]
		for _, sh := range db.Shares {
			if sh.OwnerID == id {
				for _, f := range sh.Files {
					stored = append(stored, f.Stored)
				}
				continue
			}
			keptShares = append(keptShares, sh)
		}
		db.Shares = keptShares

		keptUploads := db.Uploads[:0]
		for _, up := range db.Uploads {
			if up.OwnerID == id {
				stored = append(stored, up.Stored)
				continue
			}
			keptUploads = append(keptUploads, up)
		}
		db.Uploads = keptUploads

		keptSessions := db.Sessions[:0]
		for _, ss := range db.Sessions {
			if ss.UserID != id {
				keptSessions = append(keptSessions, ss)
			}
		}
		db.Sessions = keptSessions

		db.Users = append(db.Users[:i], db.Users[i+1:]...)
	})

	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}

	for _, s := range stored {
		_ = os.Remove(filepath.Join(a.cfg.FilesDir, s))
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
