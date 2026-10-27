package main

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config 运行参数，全部可通过环境变量覆盖
type Config struct {
	Host             string
	Port             int
	DataDir          string
	FilesDir         string
	DBPath           string
	PublicDir        string
	MaxFileBytes     int64
	MaxTotalBytes    int64
	MaxFilesPerShare int
	MaxTextChars     int
	DefaultTTLHours  float64
	MaxTTLHours      float64
	SessionDays      int
	AdminUser        string
	AdminPassword    string
	Secret           string
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

func loadConfig() Config {
	dataDir := envStr("QD_DATA_DIR", "data")
	absData, err := filepath.Abs(dataDir)
	if err == nil {
		dataDir = absData
	}

	return Config{
		Host:             envStr("HOST", "0.0.0.0"),
		Port:             envInt("PORT", 6026),
		DataDir:          dataDir,
		FilesDir:         filepath.Join(dataDir, "files"),
		DBPath:           filepath.Join(dataDir, "db.json"),
		PublicDir:        envStr("QD_PUBLIC_DIR", ""),
		MaxFileBytes:     int64(envInt("QD_MAX_FILE_MB", 200)) * 1024 * 1024,
		MaxTotalBytes:    int64(envInt("QD_MAX_SHARE_MB", 500)) * 1024 * 1024,
		MaxFilesPerShare: envInt("QD_MAX_FILES", 10),
		MaxTextChars:     200000,
		DefaultTTLHours:  envFloat("QD_DEFAULT_TTL_HOURS", 24),
		MaxTTLHours:      envFloat("QD_MAX_TTL_HOURS", 720),
		SessionDays:      envInt("QD_SESSION_DAYS", 7),
		AdminUser:        envStr("QD_ADMIN_USER", "admin"),
		AdminPassword:    envStr("QD_ADMIN_PASSWORD", ""),
		Secret:           envStr("QD_SECRET", ""),
	}
}
