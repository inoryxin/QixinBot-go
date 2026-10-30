package main

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// watcher 监听插件源码，变了就重新打包并让框架热重载。
//
// 插件作者最烦的是"改一行 → 手动打包 → 手动重载"。
// 开了 -watch 之后，保存即生效。
type watcher struct {
	pluginDir string   // 插件源码目录
	pkgPath   string   // 要覆盖的 .anzu
	pluginID  string   // 插件 id（重载接口要）
	webAddr   string   // 控制台地址
	skip      []string // 跳过的目录名
}

// watchLoop 每秒扫一次文件指纹，变了就重打包。
func (w *watcher) watchLoop(stop <-chan struct{}) {
	last := w.signature()
	fmt.Printf("[watch] 监听源码变动：%s（保存即重载）\n", w.pluginDir)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			sig := w.signature()
			if sig == last {
				continue
			}
			last = sig
			fmt.Printf("\n[watch] 检测到改动，重新打包…\n")
			// 编辑器原子保存瞬间 manifest 可能不在/是空的：
			// runPack 内部的 fail() 会直接 os.Exit，从 watcher goroutine
			// 里退出去会跳过 Ctrl+C 的 killTree，框架和插件全成孤儿
			manPath := filepath.Join(w.pluginDir, "manifest.toml")
			if info, err := os.Stat(manPath); err != nil || info.Size() == 0 {
				fmt.Printf("  [watch] manifest 还没写完，跳过这一轮\n")
				continue
			}
			runPackTo(w.pluginDir, w.pkgPath)
			if err := w.reload(); err != nil {
				fmt.Printf("  [watch] 重载失败: %v\n", err)
			} else {
				fmt.Printf("  [watch] 已重载，去虚拟群试试\n")
			}
		}
	}
}

// signature 是所有源文件的 md5（只看 app/ 和 manifest.toml）。
func (w *watcher) signature() string {
	h := md5.New()
	_ = filepath.WalkDir(w.pluginDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(w.pluginDir, path)
		// 跳过产物和依赖，只看源码
		for _, s := range w.skip {
			if rel == s || strings.HasPrefix(rel, s+string(filepath.Separator)) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		// 只关心代码和清单
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".js" && ext != ".mjs" && ext != ".cjs" && ext != ".ts" &&
			ext != ".py" && ext != ".toml" && ext != ".json" && ext != ".html" &&
			filepath.Base(path) != "manifest.toml" {
			return nil
		}
		_, _ = io.WriteString(h, rel)
		if data, err := os.ReadFile(path); err == nil {
			_, _ = h.Write(data)
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

// reload 调控制台的重载接口。
func (w *watcher) reload() error {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	base := "http://" + w.webAddr

	// 登录（dev 环境固定 admin/dev）
	form := url.Values{"username": {"admin"}, "password": {"dev"}}
	resp, err := client.PostForm(base+"/login", form)
	if err != nil {
		return fmt.Errorf("登录失败: %w", err)
	}
	resp.Body.Close()

	// 重载插件
	req, _ := http.NewRequest("POST", base+"/api/plugins/"+w.pluginID+"/reload", bytes.NewReader(nil))
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 302 && resp.StatusCode != 303 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// defaultSkip 是打包时不看的目录。
func defaultSkip() []string {
	return []string{"data", "deps", "_sdk", ".git", "node_modules"}
}

// readPluginID 从 manifest.toml 里抠出 id（不引 TOML 库，够用就行）。
func readPluginID(manifestPath string) string {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "id = ") || strings.HasPrefix(line, "id=") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "id"))
			v = strings.TrimSpace(strings.TrimPrefix(v, "="))
			v = strings.Trim(v, "\"'")
			return v
		}
	}
	return ""
}
