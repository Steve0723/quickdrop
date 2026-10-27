package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type App struct {
	cfg     Config
	store   *Store
	limiter *Limiter
}

/* ---------------- 通用响应 ---------------- */

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"encode_failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

func errJSON(w http.ResponseWriter, status int, code, message string) {
	payload := map[string]any{"error": code}
	if message != "" {
		payload["message"] = message
	}
	writeJSON(w, status, payload)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(dst)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	return strings.EqualFold(strings.TrimSpace(strings.Split(proto, ",")[0]), "https")
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

func publicUser(u User) map[string]any {
	return map[string]any{
		"id":        u.ID,
		"username":  u.Username,
		"role":      u.Role,
		"createdAt": u.CreatedAt,
		"disabled":  u.Disabled,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var mimePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.+-]*/[a-zA-Z0-9][a-zA-Z0-9.+-]*$`)

func safeMime(v string) string {
	if mimePattern.MatchString(v) {
		return v
	}
	return "application/octet-stream"
}

func safeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if strings.ContainsRune(`:*?"<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "file"
	}
	if utf8.RuneCountInString(out) > 120 {
		out = string([]rune(out)[:120])
	}
	return out
}

func asciiFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r < 0x7f && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}

/* ---------------- 会话 ---------------- */

func (a *App) currentUser(r *http.Request) (User, bool) {
	c, err := r.Cookie("qd_session")
	if err != nil || c.Value == "" {
		return User{}, false
	}

	var user User
	var ok bool
	now := nowMs()

	a.store.Read(func(db *DB) {
		for _, ss := range db.Sessions {
			if ss.Token != c.Value {
				continue
			}
			if ss.ExpiresAt <= now {
				return
			}
			if i := findUserIndex(db, ss.UserID); i >= 0 && !db.Users[i].Disabled {
				user = db.Users[i]
				ok = true
			}
			return
		}
	})
	return user, ok
}

func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, ok := a.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "unauthorized", "")
		return User{}, false
	}
	return user, true
}

func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return User{}, false
	}
	if user.Role != "admin" {
		errJSON(w, http.StatusForbidden, "forbidden", "")
		return User{}, false
	}
	return user, true
}

/* ---------------- 认证 ---------------- */

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}

	var req loginReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}

	username := strings.ToLower(strings.TrimSpace(req.Username))
	ip := clientIP(r)

	if ok, wait := a.limiter.Allow("login:"+ip+":"+username, 10, 10*time.Minute); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too_many_attempts", "retryAfter": wait,
		})
		return
	}

	var user User
	var found bool
	a.store.Read(func(db *DB) {
		for _, u := range db.Users {
			if u.Username == username {
				user = u
				found = true
				return
			}
		}
	})

	if !found || user.Disabled || !verifySecret(req.Password, user.Password) {
		errJSON(w, http.StatusUnauthorized, "invalid_credentials", "")
		return
	}

	token := randomToken(32)
	ttl := time.Duration(a.cfg.SessionDays) * 24 * time.Hour
	session := Session{
		Token:     token,
		UserID:    user.ID,
		CreatedAt: nowMs(),
		ExpiresAt: nowMs() + ttl.Milliseconds(),
		IP:        ip,
		Agent:     truncate(r.UserAgent(), 200),
	}
	_ = a.store.Update(func(db *DB) {
		db.Sessions = append(db.Sessions, session)
	})

	setCookie(w, "qd_session", token, int(ttl.Seconds()), isSecure(r))
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}

	if c, err := r.Cookie("qd_session"); err == nil {
		_ = a.store.Update(func(db *DB) {
			for i := 0; i < len(db.Sessions); i++ {
				if db.Sessions[i].Token == c.Value {
					db.Sessions = append(db.Sessions[:i], db.Sessions[i+1:]...)
					return
				}
			}
		})
	}

	setCookie(w, "qd_session", "", -1, isSecure(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := a.currentUser(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}

type updateMeReq struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (a *App) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	var req updateMeReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	if !verifySecret(req.CurrentPassword, user.Password) {
		errJSON(w, http.StatusForbidden, "invalid_password", "")
		return
	}
	if len(req.NewPassword) < 6 {
		errJSON(w, http.StatusBadRequest, "password_too_short", "密码至少 6 位")
		return
	}

	hash := hashSecret(req.NewPassword)
	token := ""
	if c, err := r.Cookie("qd_session"); err == nil {
		token = c.Value
	}

	_ = a.store.Update(func(db *DB) {
		if i := findUserIndex(db, user.ID); i >= 0 {
			db.Users[i].Password = hash
		}
		kept := db.Sessions[:0]
		for _, ss := range db.Sessions {
			if ss.UserID != user.ID || ss.Token == token {
				kept = append(kept, ss)
			}
		}
		db.Sessions = kept
	})

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- 上传 ---------------- */

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	if r.ContentLength <= 0 {
		errJSON(w, http.StatusLengthRequired, "length_required", "")
		return
	}
	if r.ContentLength > a.cfg.MaxFileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error":   "file_too_large",
			"max":     a.cfg.MaxFileBytes,
			"message": fmt.Sprintf("单个文件不能超过 %dMB", a.cfg.MaxFileBytes/1024/1024),
		})
		return
	}

	name := "file"
	if raw := r.Header.Get("X-File-Name"); raw != "" {
		if decoded, err := url.QueryUnescape(raw); err == nil {
			name = decoded
		}
	}
	name = safeFileName(name)
	contentType := safeMime(r.Header.Get("X-File-Type"))

	stored := randomToken(18)
	dest := filepath.Join(a.cfg.FilesDir, stored)

	f, err := os.Create(dest)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "write_failed", "")
		return
	}

	written, copyErr := io.Copy(f, io.LimitReader(r.Body, a.cfg.MaxFileBytes+1))
	closeErr := f.Close()

	if copyErr != nil || closeErr != nil || written > a.cfg.MaxFileBytes {
		_ = os.Remove(dest)
		errJSON(w, http.StatusRequestEntityTooLarge, "file_too_large", "")
		return
	}

	record := Upload{
		ID:        shortID(12),
		Name:      name,
		Size:      written,
		Type:      contentType,
		Stored:    stored,
		OwnerID:   user.ID,
		CreatedAt: nowMs(),
	}
	_ = a.store.Update(func(db *DB) {
		db.Uploads = append(db.Uploads, record)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"id": record.ID, "name": record.Name, "size": record.Size,
	})
}

/* ---------------- 创建 / 管理分享 ---------------- */

type createShareReq struct {
	Title          string   `json:"title"`
	Content        string   `json:"content"`
	HiddenContent  string   `json:"hiddenContent"`
	ExtractionCode string   `json:"extractionCode"`
	TTLHours       float64  `json:"ttlHours"`
	UploadIDs      []string `json:"uploadIds"`
}

var codePattern = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

func (a *App) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	var req createShareReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}

	title := strings.TrimSpace(req.Title)
	if utf8.RuneCountInString(title) > 120 {
		title = string([]rune(title)[:120])
	}
	content := req.Content
	hiddenContent := req.HiddenContent
	extractionCode := strings.TrimSpace(req.ExtractionCode)

	if utf8.RuneCountInString(content) > a.cfg.MaxTextChars || utf8.RuneCountInString(hiddenContent) > a.cfg.MaxTextChars {
		errJSON(w, http.StatusBadRequest, "text_too_long", "文本内容过长")
		return
	}
	if strings.TrimSpace(content) == "" && strings.TrimSpace(hiddenContent) == "" && len(req.UploadIDs) == 0 {
		errJSON(w, http.StatusBadRequest, "empty_share", "请填写文本或选择文件")
		return
	}
	if extractionCode != "" && !codePattern.MatchString(extractionCode) {
		errJSON(w, http.StatusBadRequest, "invalid_code", "提取码只能包含字母和数字")
		return
	}

	ttl := req.TTLHours
	if ttl <= 0 {
		ttl = a.cfg.DefaultTTLHours
	}
	if ttl < 0.25 {
		ttl = 0.25
	}
	if ttl > a.cfg.MaxTTLHours {
		ttl = a.cfg.MaxTTLHours
	}

	// 取走临时上传（先校验总量，超限则一个都不取）
	var files []ShareFile
	var totalBytes int64
	var tooLarge bool

	_ = a.store.Update(func(db *DB) {
		var indexes []int
		for _, id := range req.UploadIDs {
			for i := range db.Uploads {
				if db.Uploads[i].ID == id && db.Uploads[i].OwnerID == user.ID {
					indexes = append(indexes, i)
					totalBytes += db.Uploads[i].Size
					break
				}
			}
		}
		if totalBytes > a.cfg.MaxTotalBytes {
			tooLarge = true
			return
		}
		for i := len(indexes) - 1; i >= 0; i-- {
			up := db.Uploads[indexes[i]]
			files = append(files, ShareFile{
				ID:     shortID(10),
				Name:   up.Name,
				Size:   up.Size,
				Type:   up.Type,
				Stored: up.Stored,
			})
			db.Uploads = append(db.Uploads[:indexes[i]], db.Uploads[indexes[i]+1:]...)
		}
	})

	if tooLarge {
		errJSON(w, http.StatusRequestEntityTooLarge, "share_too_large", "单次分享总大小超出限制")
		return
	}

	// 反转顺序，保持用户选择顺序
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
	}

	createdAt := nowMs()
	var hidden *Hidden
	hiddenCode := ""
	if strings.TrimSpace(hiddenContent) != "" {
		hiddenCode = randomDigits(4)
		hidden = &Hidden{Content: hiddenContent, Code: hiddenCode}
	}

	var share Share
	_ = a.store.Update(func(db *DB) {
		id := ""
		for i := 0; i < 60; i++ {
			candidate := shortID(4)
			if findShareIndex(db, candidate) < 0 {
				id = candidate
				break
			}
		}
		if id == "" {
			id = shortID(6)
		}

		share = Share{
			ID:             id,
			OwnerID:        user.ID,
			Title:          title,
			Content:        content,
			Hidden:         hidden,
			ExtractionCode: strings.ToLower(extractionCode),
			CreatedAt:      createdAt,
			ExpiresAt:      createdAt + int64(ttl*3600*1000),
			TTLHours:       ttl,
			Files:          files,
		}
		db.Shares = append(db.Shares, share)
	})

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             share.ID,
		"path":           "/s/" + share.ID,
		"extractionCode": share.ExtractionCode,
		"hiddenCode":     hiddenCode,
		"expiresAt":      share.ExpiresAt,
	})
}

func (a *App) handleListShares(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	a.purgeExpired()

	type item struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Path      string `json:"path"`
		CreatedAt int64  `json:"createdAt"`
		ExpiresAt int64  `json:"expiresAt"`
		Expired   bool   `json:"expired"`
		HasCode   bool   `json:"hasCode"`
		HasHidden bool   `json:"hasHidden"`
		FileCount int    `json:"fileCount"`
		TotalSize int64  `json:"totalSize"`
		Views     int    `json:"views"`
		Downloads int    `json:"downloads"`
	}

	items := []item{}
	a.store.Read(func(db *DB) {
		for _, sh := range db.Shares {
			if sh.OwnerID != user.ID {
				continue
			}
			var size int64
			for _, f := range sh.Files {
				size += f.Size
			}
			items = append(items, item{
				ID:        sh.ID,
				Title:     sh.Title,
				Path:      "/s/" + sh.ID,
				CreatedAt: sh.CreatedAt,
				ExpiresAt: sh.ExpiresAt,
				Expired:   sh.ExpiresAt <= nowMs(),
				HasCode:   sh.ExtractionCode != "",
				HasHidden: sh.Hidden != nil,
				FileCount: len(sh.Files),
				TotalSize: size,
				Views:     sh.Views,
				Downloads: sh.Downloads,
			})
		}
	})

	// 按创建时间倒序
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].CreatedAt > items[j-1].CreatedAt; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"shares": items})
}

func (a *App) handleShareDetail(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	var share Share
	var found bool
	a.store.Read(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			share = db.Shares[i]
			found = true
		}
	})
	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	if share.OwnerID != user.ID && user.Role != "admin" {
		errJSON(w, http.StatusForbidden, "forbidden", "")
		return
	}

	type fileItem struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		Downloads int    `json:"downloads"`
	}
	files := []fileItem{}
	for _, f := range share.Files {
		files = append(files, fileItem{ID: f.ID, Name: f.Name, Size: f.Size, Downloads: f.Downloads})
	}

	var hidden any
	if share.Hidden != nil {
		hidden = map[string]any{"content": share.Hidden.Content, "code": share.Hidden.Code}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":             share.ID,
		"title":          share.Title,
		"path":           "/s/" + share.ID,
		"content":        share.Content,
		"hidden":         hidden,
		"extractionCode": share.ExtractionCode,
		"createdAt":      share.CreatedAt,
		"expiresAt":      share.ExpiresAt,
		"expired":        share.ExpiresAt <= nowMs(),
		"views":          share.Views,
		"downloads":      share.Downloads,
		"files":          files,
	})
}

func (a *App) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	var stored []string
	var forbidden, found bool

	_ = a.store.Update(func(db *DB) {
		i := findShareIndex(db, id)
		if i < 0 {
			return
		}
		found = true
		if db.Shares[i].OwnerID != user.ID && user.Role != "admin" {
			forbidden = true
			return
		}
		for _, f := range db.Shares[i].Files {
			stored = append(stored, f.Stored)
		}
		db.Shares = append(db.Shares[:i], db.Shares[i+1:]...)
	})

	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	if forbidden {
		errJSON(w, http.StatusForbidden, "forbidden", "")
		return
	}

	for _, s := range stored {
		_ = os.Remove(filepath.Join(a.cfg.FilesDir, s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- 访客访问 ---------------- */

func (a *App) purgeExpired() {
	for _, stored := range a.store.PurgeExpired() {
		_ = os.Remove(filepath.Join(a.cfg.FilesDir, stored))
	}
}

func (a *App) purgeShare(id string) {
	var stored []string
	_ = a.store.Update(func(db *DB) {
		i := findShareIndex(db, id)
		if i < 0 {
			return
		}
		for _, f := range db.Shares[i].Files {
			stored = append(stored, f.Stored)
		}
		db.Shares = append(db.Shares[:i], db.Shares[i+1:]...)
	})
	for _, s := range stored {
		_ = os.Remove(filepath.Join(a.cfg.FilesDir, s))
	}
}

func (a *App) hasAccess(r *http.Request, share Share) bool {
	if share.ExtractionCode == "" {
		return true
	}
	c, err := r.Cookie("qd_acc_" + share.ID)
	if err != nil {
		return false
	}
	return safeEqual(c.Value, hmacToken(a.store.Secret(), "access:"+share.ID))
}

func (a *App) handlePublicShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var share Share
	var found bool
	a.store.Read(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			share = db.Shares[i]
			found = true
		}
	})

	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	if share.ExpiresAt <= nowMs() {
		a.purgeShare(id)
		errJSON(w, http.StatusGone, "expired", "该分享已过期，内容已自动删除")
		return
	}
	if !a.hasAccess(r, share) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":     "need_code",
			"title":     share.Title,
			"expiresAt": share.ExpiresAt,
		})
		return
	}

	_ = a.store.Update(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			db.Shares[i].Views++
		}
	})

	ownerName := ""
	a.store.Read(func(db *DB) {
		if i := findUserIndex(db, share.OwnerID); i >= 0 {
			ownerName = db.Users[i].Username
		}
	})

	type fileItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
		Type string `json:"type"`
	}
	files := []fileItem{}
	for _, f := range share.Files {
		files = append(files, fileItem{ID: f.ID, Name: f.Name, Size: f.Size, Type: f.Type})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":        share.ID,
		"title":     share.Title,
		"content":   share.Content,
		"hasHidden": share.Hidden != nil,
		"files":     files,
		"createdAt": share.CreatedAt,
		"expiresAt": share.ExpiresAt,
		"owner":     ownerName,
	})
}

type unlockReq struct {
	Code string `json:"code"`
}

func (a *App) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}

	id := r.PathValue("id")

	var share Share
	var found bool
	a.store.Read(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			share = db.Shares[i]
			found = true
		}
	})
	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	if share.ExpiresAt <= nowMs() {
		a.purgeShare(id)
		errJSON(w, http.StatusGone, "expired", "该分享已过期，内容已自动删除")
		return
	}

	if ok, wait := a.limiter.Allow("unlock:"+clientIP(r)+":"+id, 12, 10*time.Minute); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too_many_attempts", "retryAfter": wait,
		})
		return
	}

	var req unlockReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}

	if share.ExtractionCode == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if !safeEqual(strings.ToLower(strings.TrimSpace(req.Code)), share.ExtractionCode) {
		errJSON(w, http.StatusForbidden, "invalid_code", "提取码不正确")
		return
	}

	maxAge := int((share.ExpiresAt - nowMs()) / 1000)
	if maxAge < 60 {
		maxAge = 60
	}
	setCookie(w, "qd_acc_"+share.ID, hmacToken(a.store.Secret(), "access:"+share.ID), maxAge, isSecure(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type revealReq struct {
	Code string `json:"code"`
}

func (a *App) handleReveal(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		errJSON(w, http.StatusForbidden, "bad_origin", "")
		return
	}

	id := r.PathValue("id")

	var share Share
	var found bool
	a.store.Read(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			share = db.Shares[i]
			found = true
		}
	})
	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	if share.ExpiresAt <= nowMs() {
		a.purgeShare(id)
		errJSON(w, http.StatusGone, "expired", "该分享已过期，内容已自动删除")
		return
	}
	if !a.hasAccess(r, share) {
		errJSON(w, http.StatusUnauthorized, "need_code", "")
		return
	}
	if share.Hidden == nil {
		errJSON(w, http.StatusNotFound, "no_hidden", "")
		return
	}

	if ok, wait := a.limiter.Allow("reveal:"+clientIP(r)+":"+id, 10, 10*time.Minute); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too_many_attempts", "retryAfter": wait,
		})
		return
	}

	var req revealReq
	if err := decodeJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "")
		return
	}

	if share.Hidden.LockedUntil > nowMs() {
		wait := int((share.Hidden.LockedUntil-nowMs())/1000) + 1
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":      "locked",
			"retryAfter": wait,
			"message":    fmt.Sprintf("密码错误次数过多，请 %d 分钟后再试", (wait+59)/60),
		})
		return
	}

	input := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, req.Code)
	if len(input) > 4 {
		input = input[:4]
	}

	if !safeEqual(input, share.Hidden.Code) {
		var lockedUntil int64
		var fails int
		_ = a.store.Update(func(db *DB) {
			if i := findShareIndex(db, id); i >= 0 && db.Shares[i].Hidden != nil {
				db.Shares[i].Hidden.Fails++
				fails = db.Shares[i].Hidden.Fails
				if fails >= 8 {
					db.Shares[i].Hidden.Fails = 0
					db.Shares[i].Hidden.LockedUntil = nowMs() + 30*60*1000
					lockedUntil = db.Shares[i].Hidden.LockedUntil
				}
			}
		})
		if lockedUntil > 0 {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": "locked", "retryAfter": 1800,
				"message": "密码错误次数过多，已锁定 30 分钟",
			})
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":     "invalid_code",
			"message":   "密码不正确",
			"remaining": 8 - fails,
		})
		return
	}

	_ = a.store.Update(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 && db.Shares[i].Hidden != nil {
			db.Shares[i].Hidden.Fails = 0
			db.Shares[i].Hidden.LockedUntil = 0
			db.Shares[i].Hidden.Reveals++
		}
	})

	writeJSON(w, http.StatusOK, map[string]any{"content": share.Hidden.Content})
}

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fileID := r.PathValue("fileId")

	var share Share
	var found bool
	a.store.Read(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			share = db.Shares[i]
			found = true
		}
	})
	if !found {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	if share.ExpiresAt <= nowMs() {
		a.purgeShare(id)
		errJSON(w, http.StatusGone, "expired", "该分享已过期，内容已自动删除")
		return
	}
	if !a.hasAccess(r, share) {
		errJSON(w, http.StatusUnauthorized, "need_code", "")
		return
	}

	var target *ShareFile
	for i := range share.Files {
		if share.Files[i].ID == fileID {
			target = &share.Files[i]
			break
		}
	}
	if target == nil {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}

	filePath := filepath.Join(a.cfg.FilesDir, target.Stored)
	f, err := os.Open(filePath)
	if err != nil {
		errJSON(w, http.StatusNotFound, "file_missing", "")
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	inline := r.URL.Query().Get("mode") == "inline" && strings.HasPrefix(target.Type, "image/")
	disposition := "attachment"
	contentType := "application/octet-stream"
	if inline {
		disposition = "inline"
		contentType = target.Type
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		disposition, asciiFileName(target.Name), url.PathEscape(target.Name)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	http.ServeContent(w, r, target.Name, stat.ModTime(), f)

	_ = a.store.Update(func(db *DB) {
		if i := findShareIndex(db, id); i >= 0 {
			db.Shares[i].Downloads++
			for j := range db.Shares[i].Files {
				if db.Shares[i].Files[j].ID == fileID {
					db.Shares[i].Files[j].Downloads++
					break
				}
			}
		}
	})
}

/* ---------------- 辅助 ---------------- */

func parseIntOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
