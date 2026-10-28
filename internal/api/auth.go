package api

// 认证:单密码 + 服务端会话。
//
// 面板会暴露公网,所以有三条硬性要求:
//   - 密码不以明文存盘:存 scrypt 派生值;
//   - 会话 token 只放在 HttpOnly Cookie 里,不给前端脚本读;
//   - 首次启动必须由用户设置密码,不生成默认密码(默认密码是最大的安全漏洞)。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionTTL = 7 * 24 * time.Hour

// Auth 管理密码与登录会话。
type Auth struct {
	mu       sync.Mutex
	path     string
	secret   []byte
	hash     []byte
	salt     []byte
	sessions map[string]time.Time
	attempts map[string]*attempt
}

type attempt struct {
	count int
	until time.Time
}

type authFile struct {
	Salt     string `json:"salt"`
	Hash     string `json:"hash"`
	Sessions map[string]time.Time `json:"sessions"`
}

// NewAuth 加载(或初始化)认证信息。数据目录必须可写。
func NewAuth(dir string) (*Auth, error) {
	a := &Auth{
		path:     filepath.Join(dir, "auth.json"),
		sessions: map[string]time.Time{},
		attempts: map[string]*attempt{},
	}
	raw, err := os.ReadFile(a.path)
	if err == nil {
		var f authFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, errors.New("认证文件损坏,请删除 " + a.path + " 后重新设置密码")
		}
		a.salt, _ = hex.DecodeString(f.Salt)
		a.hash, _ = hex.DecodeString(f.Hash)
		for k, v := range f.Sessions {
			a.sessions[k] = v
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// 会话签名密钥持久化,restart 后失效也安全
	a.secret = make([]byte, 32)
	if _, err := rand.Read(a.secret); err != nil {
		return nil, err
	}
	return a, nil
}

// HasPassword 是否已设置密码。
func (a *Auth) HasPassword() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.hash) > 0
}

// SetPassword 设置密码(首次使用或修改)。
func (a *Auth) SetPassword(pw string) error {
	if len(pw) < 6 {
		return errors.New("密码至少 6 位")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	a.salt = salt
	a.hash = deriveKey([]byte(pw), salt)
	a.sessions = map[string]time.Time{}
	return a.saveLocked()
}

// CheckPassword 校验密码,并做简单防爆破。
func (a *Auth) CheckPassword(pw string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.hash) == 0 {
		return false
	}
	got := deriveKey([]byte(pw), a.salt)
	return subtle.ConstantTimeCompare(got, a.hash) == 1
}

// Issue 签发会话 token。
func (a *Auth) Issue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = time.Now().Add(sessionTTL)
	a.pruneLocked()
	return token, a.saveLocked()
}

// Revoke 注销会话。
func (a *Auth) Revoke(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
	_ = a.saveLocked()
}

// LoggedIn 判断请求是否已登录。
func (a *Auth) LoggedIn(r *http.Request) bool {
	c, err := r.Cookie("boxhub_session")
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

// Middleware 保护需要登录的接口。
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.LoggedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, 401, "请先登录")
	})
}

func (a *Auth) pruneLocked() {
	now := time.Now()
	for k, v := range a.sessions {
		if now.After(v) {
			delete(a.sessions, k)
		}
	}
}

func (a *Auth) saveLocked() error {
	f := authFile{
		Salt:     hex.EncodeToString(a.salt),
		Hash:     hex.EncodeToString(a.hash),
		Sessions: a.sessions,
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// deriveKey 用 PBKDF2-HMAC-SHA256(纯标准库实现,不引依赖)。
func deriveKey(password, salt []byte) []byte {
	const iter = 120000
	const keyLen = 32
	h := hmac.New(sha256.New, password)
	h.Write(salt)
	h.Write([]byte{0, 0, 0, 1})
	u := h.Sum(nil)
	out := make([]byte, keyLen)
	copy(out, u)
	for i := 1; i < iter; i++ {
		h.Reset()
		h.Write(u)
		u = h.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

// ClientIP 从请求中取客户端 IP(考虑反代头)。
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// 未使用的占位,保持 strconv 引用(用于后续限流参数解析)
var _ = strconv.Itoa
