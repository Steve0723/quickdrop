package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	idAlphabet   = "23456789abcdefghijkmnpqrstuvwxyz"
	pbkdf2Rounds = 120000
)

func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return buf
}

func randomToken(n int) string {
	return hex.EncodeToString(randomBytes(n))
}

// shortID 生成 n 位短 ID（字符集去掉了 0/o/1/l 等易混字符）
func shortID(n int) string {
	buf := randomBytes(n)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = idAlphabet[int(buf[i])%len(idAlphabet)]
	}
	return string(out)
}

// randomDigits 生成 n 位数字串（允许前导零）
func randomDigits(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			panic(err)
		}
		b.WriteString(strconv.FormatInt(v.Int64(), 10))
	}
	return b.String()
}

// pbkdf2SHA256 手写实现，避免引入外部依赖
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	blocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)

	var idx [4]byte
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		mac.Write(idx[:])
		u := mac.Sum(nil)

		t := make([]byte, len(u))
		copy(t, u)

		for i := 1; i < iter; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func hashSecret(plain string) string {
	salt := randomBytes(16)
	dk := pbkdf2SHA256([]byte(plain), salt, pbkdf2Rounds, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Rounds, hex.EncodeToString(salt), hex.EncodeToString(dk))
}

func verifySecret(plain, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 2000000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(plain), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func hmacToken(secret, data string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func safeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomPassword(n int) string {
	const chars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := randomBytes(n)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = chars[int(buf[i])%len(chars)]
	}
	return string(out)
}

/* ---------------- 滑动窗口限流 ---------------- */

type bucket struct {
	count   int
	resetAt time.Time
}

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func NewLimiter() *Limiter {
	l := &Limiter{buckets: make(map[string]*bucket)}
	go func() {
		ticker := time.NewTicker(time.Minute)
		for range ticker.C {
			now := time.Now()
			l.mu.Lock()
			for key, b := range l.buckets {
				if b.resetAt.Before(now) {
					delete(l.buckets, key)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// Allow 返回是否放行，以及需要等待的秒数
func (l *Limiter) Allow(key string, limit int, window time.Duration) (bool, int) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok || b.resetAt.Before(now) {
		l.buckets[key] = &bucket{count: 1, resetAt: now.Add(window)}
		return true, 0
	}

	b.count++
	if b.count > limit {
		return false, int(b.resetAt.Sub(now).Seconds()) + 1
	}
	return true, 0
}
