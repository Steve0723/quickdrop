package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"createdAt"`
	Disabled  bool   `json:"disabled"`
}

type Session struct {
	Token     string `json:"token"`
	UserID    string `json:"userId"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	IP        string `json:"ip,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

type ShareFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Type      string `json:"type"`
	Stored    string `json:"storedName"`
	Downloads int    `json:"downloads"`
}

type Hidden struct {
	Content     string `json:"content"`
	Code        string `json:"code"`
	Reveals     int    `json:"reveals"`
	Fails       int    `json:"fails"`
	LockedUntil int64  `json:"lockedUntil"`
}

type Share struct {
	ID             string      `json:"id"`
	OwnerID        string      `json:"ownerId"`
	Title          string      `json:"title"`
	Content        string      `json:"content"`
	Hidden         *Hidden     `json:"hidden,omitempty"`
	ExtractionCode string      `json:"extractionCode,omitempty"`
	CreatedAt      int64       `json:"createdAt"`
	ExpiresAt      int64       `json:"expiresAt"`
	TTLHours       float64     `json:"ttlHours"`
	Files          []ShareFile `json:"files"`
	Views          int         `json:"views"`
	Downloads      int         `json:"downloads"`
}

type Upload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Type      string `json:"type"`
	Stored    string `json:"storedName"`
	OwnerID   string `json:"ownerId"`
	CreatedAt int64  `json:"createdAt"`
}

type DB struct {
	Version  int       `json:"version"`
	Secret   string    `json:"secret"`
	Users    []User    `json:"users"`
	Sessions []Session `json:"sessions"`
	Shares   []Share   `json:"shares"`
	Uploads  []Upload  `json:"uploads"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	data DB
}

func nowMs() int64 { return time.Now().UnixMilli() }

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if buf, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(buf, &s.data); err != nil {
			_ = os.WriteFile(s.path+".corrupt", buf, 0o600)
			s.data = DB{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if s.data.Users == nil {
		s.data.Users = []User{}
	}
	if s.data.Sessions == nil {
		s.data.Sessions = []Session{}
	}
	if s.data.Shares == nil {
		s.data.Shares = []Share{}
	}
	if s.data.Uploads == nil {
		s.data.Uploads = []Upload{}
	}

	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	buf, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		if werr := os.WriteFile(s.path, buf, 0o600); werr != nil {
			return werr
		}
		_ = os.Remove(tmp)
	}
	return nil
}

// Read 只读访问，回调内不要修改数据
func (s *Store) Read(fn func(*DB)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.data)
}

// Update 写访问，回调结束后自动落盘
func (s *Store) Update(fn func(*DB)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
	return s.saveLocked()
}

func (s *Store) Secret() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Secret
}

func (s *Store) SetSecret(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Secret = v
}

/* ---------------- 索引查找 ---------------- */

func findUserIndex(db *DB, id string) int {
	for i := range db.Users {
		if db.Users[i].ID == id {
			return i
		}
	}
	return -1
}

func findShareIndex(db *DB, id string) int {
	for i := range db.Shares {
		if db.Shares[i].ID == id {
			return i
		}
	}
	return -1
}

/* ---------------- 清理 ---------------- */

// PurgeExpired 移除所有已过期分享与陈旧临时上传，返回需要删除的磁盘文件名
func (s *Store) PurgeExpired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowMs()
	var removed []string
	changed := false

	keptShares := s.data.Shares[:0]
	for _, sh := range s.data.Shares {
		if sh.ExpiresAt <= now {
			for _, f := range sh.Files {
				removed = append(removed, f.Stored)
			}
			changed = true
			continue
		}
		keptShares = append(keptShares, sh)
	}
	s.data.Shares = keptShares

	keptUploads := s.data.Uploads[:0]
	for _, up := range s.data.Uploads {
		if now-up.CreatedAt > 3*60*60*1000 {
			removed = append(removed, up.Stored)
			changed = true
			continue
		}
		keptUploads = append(keptUploads, up)
	}
	s.data.Uploads = keptUploads

	keptSessions := s.data.Sessions[:0]
	for _, ss := range s.data.Sessions {
		if ss.ExpiresAt <= now {
			changed = true
			continue
		}
		keptSessions = append(keptSessions, ss)
	}
	s.data.Sessions = keptSessions

	if changed {
		_ = s.saveLocked()
	}
	return removed
}
