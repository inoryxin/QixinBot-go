package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 信任来源，界面上要区分（用户得知道"这个是谁给的信任"）。
const (
	TrustOfficial = "official" // 框架内置（官方）
	TrustConfig   = "config"   // 部署者写在 config.toml
	TrustAdded    = "added"    // 控制台里热加的（第三方自用）
)

// TrustStore 是"可信签名者"的合并视图：
//
//	官方内置（编译进二进制，谁都不用配）
//	  + config.toml 的 trusted_keys（部署者自己加的）
//	  + data/trusted-keys.json（控制台热加，立刻生效）
//
// 为什么三层：
//
//   - 官方：装官方插件不需要任何配置，换密钥只随框架版本走；
//   - config：部署者想给全公司统一固定一份信任；
//   - 热加：临时装个第三方插件，贴一下就能用，不用重启也不用改文件。
//
// TOML 重写会丢注释，所以热加的密钥单独存 JSON，不回写 config。
type TrustStore struct {
	path string
	log  *slog.Logger

	mu      sync.RWMutex
	added   []string
	fromCfg []string
}

// NewTrustStore 打开（或创建）信任列表。
func NewTrustStore(path string, cfgKeys []string, log *slog.Logger) *TrustStore {
	t := &TrustStore{path: path, log: log, fromCfg: append([]string(nil), cfgKeys...)}
	raw, err := os.ReadFile(path)
	if err == nil {
		var data struct {
			Keys []string `json:"keys"`
		}
		if err := json.Unmarshal(raw, &data); err == nil {
			t.added = data.Keys
		} else {
			t.log.Warn("trusted-keys.json 解析失败，忽略", "err", err)
		}
	} else if !os.IsNotExist(err) {
		t.log.Warn("读 trusted-keys.json 失败", "err", err)
	}
	return t
}

// All 返回合并后的公钥（去重；官方在前，其次 config，最后热加）。
func (t *TrustStore) All() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.allLocked()
}

func (t *TrustStore) allLocked() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(officialKeys)+len(t.fromCfg)+len(t.added))
	for _, list := range [][]string{officialKeys, t.fromCfg, t.added} {
		for _, k := range list {
			k = trimKey(k)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Source 返回某个公钥的来源（official / config / added）。
func (t *TrustStore) Source(key string) string {
	key = trimKey(key)
	if IsOfficialKey(key) {
		return TrustOfficial
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, k := range t.fromCfg {
		if trimKey(k) == key {
			return TrustConfig
		}
	}
	for _, k := range t.added {
		if trimKey(k) == key {
			return TrustAdded
		}
	}
	return "" // 不在列表里
}

// FromConfig 返回 config 里配的那些。
func (t *TrustStore) FromConfig() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]string(nil), t.fromCfg...)
}

// Add 加一个公钥（已存在就返回 nil）。
func (t *TrustStore) Add(key string) error {
	key = trimKey(key)
	if key == "" {
		return fmt.Errorf("公钥不能为空")
	}
	// 先校验格式：长度不对的公钥加进去也没用，早报早好
	if _, err := parseKeys([]string{key}); err != nil {
		return fmt.Errorf("公钥格式不对: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range t.allLocked() {
		if k == key {
			return nil // 已经有了（可能来自官方或 config）
		}
	}
	t.added = append(t.added, key)
	sort.Strings(t.added)
	return t.saveLocked()
}

// Remove 删一个热加的公钥。
// 官方内置的删不掉（那是编译进去的），config 的要去改文件。
func (t *TrustStore) Remove(key string) error {
	key = trimKey(key)

	if IsOfficialKey(key) {
		return fmt.Errorf("这是框架内置的官方公钥，不能移除（要移除只能换一个框架版本）")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, k := range t.fromCfg {
		if trimKey(k) == key {
			return fmt.Errorf("这个公钥来自 config.toml 的 trusted_keys，要删请改配置文件并重启")
		}
	}

	next := t.added[:0]
	found := false
	for _, k := range t.added {
		if trimKey(k) == key {
			found = true
			continue
		}
		next = append(next, k)
	}
	if !found {
		return fmt.Errorf("没找到这个公钥")
	}
	t.added = next
	return t.saveLocked()
}

func (t *TrustStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]any{"keys": t.added}, "", "  ")
	if err != nil {
		return err
	}
	// 原子写：写一半断电会把 trusted-keys.json 截断，
	// 重启后热加的公钥全丢，第三方签名插件突然就装不上了
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return err
	}
	t.log.Info("信任列表已更新", "path", t.path, "hot_keys", len(t.added))
	return nil
}

// KeyFingerprint 是公钥的短指纹（SHA256 前 8 字节）。
// 和 qixinpack check 显示的指纹一致。
func KeyFingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// FingerprintOf 接受 base64 公钥，返回指纹；格式不对返回空串。
func FingerprintOf(key string) string {
	parsed, err := parseKeys([]string{key})
	if err != nil || len(parsed) != 1 {
		return ""
	}
	return KeyFingerprint(parsed[0])
}

// trimKey 去掉粘贴时常见的空白和引号。
func trimKey(k string) string {
	return strings.Trim(strings.TrimSpace(k), "\"'`")
}
