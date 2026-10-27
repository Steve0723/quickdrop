package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed all:public
var embeddedPublic embed.FS

func (a *App) publicFS() fs.FS {
	if a.cfg.PublicDir != "" {
		if st, err := os.Stat(a.cfg.PublicDir); err == nil && st.IsDir() {
			return os.DirFS(a.cfg.PublicDir)
		}
	}
	sub, err := fs.Sub(embeddedPublic, "public")
	if err != nil {
		log.Fatalf("嵌入资源加载失败: %v", err)
	}
	return sub
}

func (a *App) serveStatic(name string, cache string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf, err := fs.ReadFile(a.publicFS(), name)
		if err != nil {
			a.handleFallback(w, r)
			return
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(buf)))
		w.Header().Set("Cache-Control", cache)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(buf)
	}
}

func (a *App) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		http.NotFound(w, r)
		return
	}
	a.serveStatic(path.Join("assets", name), "public, max-age=86400")(w, r)
}

func (a *App) handleFallback(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		errJSON(w, http.StatusNotFound, "not_found", "")
		return
	}
	buf, err := fs.ReadFile(a.publicFS(), "404.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(buf)
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /api/me", a.handleMe)
	mux.HandleFunc("PATCH /api/me", a.handleUpdateMe)

	mux.HandleFunc("POST /api/uploads", a.handleUpload)

	mux.HandleFunc("GET /api/shares", a.handleListShares)
	mux.HandleFunc("POST /api/shares", a.handleCreateShare)
	mux.HandleFunc("GET /api/shares/{id}", a.handleShareDetail)
	mux.HandleFunc("DELETE /api/shares/{id}", a.handleDeleteShare)

	mux.HandleFunc("GET /api/public/shares/{id}", a.handlePublicShare)
	mux.HandleFunc("POST /api/public/shares/{id}/unlock", a.handleUnlock)
	mux.HandleFunc("POST /api/public/shares/{id}/reveal", a.handleReveal)
	mux.HandleFunc("GET /api/public/shares/{id}/files/{fileId}", a.handleDownload)

	mux.HandleFunc("GET /api/admin/users", a.handleAdminListUsers)
	mux.HandleFunc("POST /api/admin/users", a.handleAdminCreateUser)
	mux.HandleFunc("PATCH /api/admin/users/{id}", a.handleAdminUpdateUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", a.handleAdminDeleteUser)

	mux.HandleFunc("GET /{$}", a.serveStatic("index.html", "no-store"))
	mux.HandleFunc("GET /login", a.serveStatic("login.html", "no-store"))
	mux.HandleFunc("GET /admin", a.serveStatic("admin.html", "no-store"))
	mux.HandleFunc("GET /s/{id}", a.serveStatic("share.html", "no-store"))
	mux.HandleFunc("GET /assets/{file...}", a.handleAsset)
	mux.HandleFunc("/", a.handleFallback)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func (a *App) seedAdmin() (string, string, bool) {
	count := 0
	a.store.Read(func(db *DB) { count = len(db.Users) })
	if count > 0 {
		return "", "", false
	}

	username := strings.ToLower(strings.TrimSpace(a.cfg.AdminUser))
	if username == "" {
		username = "admin"
	}

	password := a.cfg.AdminPassword
	generated := password == ""
	if generated {
		password = randomPassword(12)
	}

	user := User{
		ID:        shortID(10),
		Username:  username,
		Password:  hashSecret(password),
		Role:      "admin",
		CreatedAt: nowMs(),
	}
	_ = a.store.Update(func(db *DB) {
		db.Users = append(db.Users, user)
	})

	if generated {
		content := fmt.Sprintf(
			"瞬传 · 初始管理员账号\n==============================\n用户名: %s\n密码:   %s\n生成时间: %s\n\n首次登录后请到「账号管理」修改密码，并删除本文件。\n",
			username, password, time.Now().Format("2006-01-02 15:04:05"),
		)
		_ = os.WriteFile(filepath.Join(a.cfg.DataDir, "admin-credentials.txt"), []byte(content), 0o600)
	}

	return username, password, generated
}

func main() {
	cfg := loadConfig()

	if err := os.MkdirAll(cfg.FilesDir, 0o755); err != nil {
		log.Fatalf("无法创建数据目录: %v", err)
	}

	store := NewStore(cfg.DBPath)
	if err := store.Load(); err != nil {
		log.Fatalf("数据加载失败: %v\n提示：若使用绑定挂载，请确认宿主机数据目录对容器内 UID 10001 可写（sudo chown -R 10001:10001 ./data）", err)
	}

	if store.Secret() == "" {
		secret := cfg.Secret
		if secret == "" {
			secret = randomToken(32)
		}
		_ = store.Update(func(db *DB) { db.Secret = secret })
	}

	app := &App{cfg: cfg, store: store, limiter: NewLimiter()}
	adminUser, adminPass, generated := app.seedAdmin()

	app.purgeExpired()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			app.purgeExpired()
		}
	}()

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	line := strings.Repeat("─", 52)
	log.Printf("%s\n  瞬传 QuickDrop · 临时分享服务已启动\n%s", line, line)
	log.Printf("  地址       http://localhost:%d", cfg.Port)
	log.Printf("  数据目录   %s", cfg.DataDir)
	log.Printf("  单文件上限 %d MB", cfg.MaxFileBytes/1024/1024)
	log.Printf("  默认有效期 %.0f 小时", cfg.DefaultTTLHours)
	if adminUser != "" {
		log.Print(line)
		log.Printf("  初始管理员 %s", adminUser)
		if generated {
			log.Printf("  初始密码   %s", adminPass)
			log.Print("  （已写入 data/admin-credentials.txt，登录后请修改）")
		} else {
			log.Print("  密码来自环境变量 QD_ADMIN_PASSWORD")
		}
	}
	log.Print(line)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Print("正在关闭服务…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
