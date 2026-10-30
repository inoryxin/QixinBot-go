// qixinpack 是 .anzu 插件的打包与签名工具。
//
//	qixinpack keygen -out mykey
//	qixinpack pack ./weather -key mykey.priv -out weather.anzu
//	qixinpack check weather.anzu -pub mykey.pub
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		runKeygen(os.Args[2:])
	case "pack":
		runPack(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	case "test":
		os.Exit(runTest(os.Args[2:]))
	case "dev":
		runDev(os.Args[2:])
	case "check":
		runCheck(os.Args[2:])
	case "new":
		runNew(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	// dev 子命令的说明在 runDev 里

	fmt.Fprintln(os.Stderr, `qixinpack — .anzu 打包工具

  new    <插件id> [-name 中文名]            生成插件骨架
  dev    <插件目录>                        起本地开发环境（假协议端 + 框架）
  test   <插件目录>                        按 manifest 的命令逐条跑一遍
  verify [-ask|-concurrency]               端到端验证（走真实 WebSocket）
  pack   <目录> -out <包> [-key <私钥>]     打包（-key 可选，给了就签名）
  check  <包> [-pub <公钥>]                校验结构与签名
  keygen -out <前缀>                       生成 ed25519 密钥对`)
}

func runKeygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	prefix := fs.String("out", "mykey", "输出文件前缀")
	_ = fs.Parse(args)
	out := *prefix

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("生成密钥失败: %v", err)
	}
	if err := os.WriteFile(out+".priv", []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		fail("写私钥失败: %v", err)
	}
	if err := os.WriteFile(out+".pub", []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		fail("写公钥失败: %v", err)
	}
	fmt.Printf("私钥: %s.priv\n公钥: %s.pub\n\n把公钥填进 config.toml：\n[plugin]\ntrusted_keys = [\"%s\"]\n",
		out, out, base64.StdEncoding.EncodeToString(pub))
}

func runPack(args []string) {
	// 手动解析：flag 包遇到第一个位置参数就停止，导致 "pack <dir> -out x" 解析失败
	var dir, out, keyFile string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-out", "--out":
			i++
			if i < len(args) {
				out = args[i]
			}
		case "-key", "--key":
			i++
			if i < len(args) {
				keyFile = args[i]
			}
		default:
			if dir == "" {
				dir = args[i]
			}
		}
	}
	if dir == "" {
		fail("用法: qixinpack pack <目录> -out <包> [-key <私钥>]")
	}
	if out == "" {
		out = filepath.Base(filepath.Clean(dir)) + ".anzu"
	}

	manPath := filepath.Join(dir, "manifest.toml")
	manData, err := os.ReadFile(manPath)
	if err != nil {
		fail("缺少 manifest.toml: %v", err)
	}
	var man struct {
		ID   string `toml:"id"`
		Kind string `toml:"kind"`
	}
	if _, err := toml.Decode(string(manData), &man); err != nil {
		fail("manifest.toml 解析失败: %v", err)
	}
	if man.ID == "" {
		fail("manifest.toml 缺少 id")
	}
	if man.Kind != "runtime" && man.Kind != "theme" && !dirExists(filepath.Join(dir, "app")) {
		fail("缺少 app/ 目录")
	}

	files, err := collectFiles(dir)
	if err != nil {
		fail("收集文件失败: %v", err)
	}

	// 框架只在运行区缺 _sdk 时才复制自己的 SDK，所以打进包里的 _sdk
	// 就是这个插件实际用的 SDK：SDK 升级随插件包走，不再有
	// "框架仓库/release 仓库/已装插件"三份副本人肉同步的问题
	if man.Kind != "runtime" && man.Kind != "theme" && !dirExists(filepath.Join(dir, "_sdk")) {
		if injected := injectSDK(files); len(injected) > 0 {
			fmt.Printf("已注入 SDK: %s\n", strings.Join(injected, ", "))
		}
	}

	// 完整性清单
	list := buildList(files)

	f, err := os.Create(out)
	if err != nil {
		fail("创建 %s 失败: %v", out, err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	for _, name := range sortedKeys(files) {
		if err := addFile(zw, name, files[name]); err != nil {
			fail("写入 %s 失败: %v", name, err)
		}
	}

	if keyFile != "" {
		privRaw, err := os.ReadFile(keyFile)
		if err != nil {
			fail("读私钥失败: %v", err)
		}
		priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(privRaw)))
		if err != nil || len(priv) != ed25519.PrivateKeySize {
			fail("私钥格式不对")
		}
		sig := ed25519.Sign(ed25519.PrivateKey(priv), list)
		if err := addBytes(zw, "META-INF/signature.ed25519", sig); err != nil {
			fail("写签名失败: %v", err)
		}
		if err := addBytes(zw, "META-INF/manifest.sha256", list); err != nil {
			fail("写清单失败: %v", err)
		}
		// **把签名者公钥也放进去**：这样框架能告诉管理员
		// "这个包是哪个指纹签的"，不用你手动比对 44 字节 base64。
		pub := ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)
		pubB64 := base64.StdEncoding.EncodeToString(pub)
		if err := addBytes(zw, "META-INF/signer.pub", []byte(pubB64+"\n")); err != nil {
			fail("写签名者公钥失败: %v", err)
		}
		fmt.Printf("已签名，%d 个文件（签发者指纹 %s）\n", len(files), keyFingerprint(pub))
	} else {
		fmt.Printf("未签名（开发模式），%d 个文件\n", len(files))
	}

	if err := zw.Close(); err != nil {
		fail("收尾失败: %v", err)
	}
	if err := f.Close(); err != nil {
		fail("关闭文件失败: %v", err)
	}

	// 自校验：包必须能被重新打开，条目数要对得上。
	// 打包过程被中断时（Ctrl+C、磁盘满、进程被杀）会写出残缺文件，
	// 这种包丢进 plugins/ 只会在运行时报"not a valid zip file"。
	zr, err := zip.OpenReader(out)
	if err != nil {
		_ = os.Remove(out)
		fail("包损坏，已删除（重新打包即可）: %v", err)
	}
	entries := len(zr.File)
	_ = zr.Close()
	if entries == 0 {
		_ = os.Remove(out)
		fail("包是空的，已删除")
	}

	st, _ := os.Stat(out)
	fmt.Printf("输出: %s (%.1f KB, %d 个条目)\n", out, float64(st.Size())/1024, entries)
}

func runCheck(args []string) {
	var pkgPath, pubFile string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-pub", "--pub":
			i++
			if i < len(args) {
				pubFile = args[i]
			}
		default:
			if pkgPath == "" {
				pkgPath = args[i]
			}
		}
	}
	if pkgPath == "" {
		fail("用法: qixinpack check <包> [-pub <公钥>]")
	}
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		fail("读包失败: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		fail("不是合法 zip: %v", err)
	}

	var list, sig []byte
	var signerPub string
	names := []string{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		names = append(names, f.Name)
		switch f.Name {
		case "META-INF/manifest.sha256":
			list = readAll(f)
		case "META-INF/signature.ed25519":
			sig = readAll(f)
		case "META-INF/signer.pub":
			signerPub = strings.TrimSpace(string(readAll(f)))
		}
	}
	sort.Strings(names)
	fmt.Printf("文件数: %d\n", len(names))
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}

	if len(sig) == 0 {
		fmt.Println("签名: 无")
		return
	}
	if signerPub != "" {
		if raw, err := base64.StdEncoding.DecodeString(signerPub); err == nil && len(raw) == ed25519.PublicKeySize {
			fmt.Printf("签发者: 指纹 %s（公钥已随包携带）\n", keyFingerprint(raw))
		}
	}
	if pubFile == "" {
		// 包里自带的公钥只能证明"包内自洽"，不能当信任依据——
		// 谁都能换上自己的公钥重签一遍。之前在这里用包内公钥
		// 验完还打印"签名: 有效"，对拿着结果决定加不加信任的管理员是误导
		if signerPub != "" {
			if raw, err := base64.StdEncoding.DecodeString(signerPub); err == nil && len(raw) == ed25519.PublicKeySize {
				fmt.Printf("签名: 有（签发者指纹 %s；包内自带公钥不能作为信任依据，用 -pub 指定你的可信公钥再验）\n", keyFingerprint(raw))
				return
			}
		}
		fmt.Println("签名: 有（未提供公钥，无法验证）")
		return
	}
	pubRaw, err := os.ReadFile(pubFile)
	if err != nil {
		fail("读公钥失败: %v", err)
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(pubRaw)))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		fail("公钥格式不对")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), list, sig) {
		fail("签名无效")
	}
	fmt.Printf("签名: 有效（签发者指纹 %s）\n", keyFingerprint(pub))
}

// ---- 工具 ----

func collectFiles(root string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	isRuntime := manifestKind(root) == "runtime"
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "META-INF" || name == ".git" || name == "node_modules.cache" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "META-INF/") {
			return nil
		}
		if !isRuntime {
			if err := checkBundledDep(rel); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		limit := int64(32 << 20)
		if isRuntime {
			limit = 256 << 20
		}
		if int64(len(data)) > limit {
			return fmt.Errorf("%s 超过 %dMB，别把大文件放进插件包", rel, limit>>20)
		}
		out[rel] = data
		return nil
	})
	return out, err
}

// injectSDK 把框架自带的 SDK 塞进包里的 _sdk/（源码没带时）。
// 返回注入的文件名。
func injectSDK(files map[string][]byte) []string {
	var out []string
	for _, rel := range []string{"node/qixin.mjs", "python/qixin.py"} {
		src := findSDKFile(rel)
		if src == "" {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		name := "_sdk/" + filepath.Base(rel)
		if _, exists := files[name]; !exists {
			files[name] = data
			out = append(out, name)
		}
	}
	return out
}

// findSDKFile 在常规位置找 sdk/<rel>（和 findSDK 同一套查找顺序）。
func findSDKFile(rel string) string {
	var roots []string
	if self, err := os.Executable(); err == nil {
		selfDir := filepath.Dir(self)
		roots = append(roots,
			filepath.Join(selfDir, ".."),
			filepath.Join(selfDir, "..", ".."),
			selfDir,
		)
	}
	roots = append(roots, ".")
	for _, root := range roots {
		p := filepath.Join(root, "sdk", filepath.FromSlash(rel))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

func buildList(files map[string][]byte) []byte {
	names := sortedKeys(files)
	var b strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func addFile(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func addBytes(zw *zip.Writer, name string, data []byte) error {
	return addFile(zw, name, data)
}

func readAll(f *zip.File) []byte {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return data
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// keyFingerprint 是公钥的短指纹（前 8 字节 hex），
// 用来核对"这个包是谁签的"——和人名一样短，但不重复。
func keyFingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// manifestKind 读顶层 kind（不引 TOML，够用）。
func manifestKind(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "manifest.toml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "kind") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), `"'`)
			}
		}
	}
	return ""
}

// checkBundledDep 拦截插件包里的平台相关依赖；运行时就该提供这些。
func checkBundledDep(rel string) error {
	if !strings.HasPrefix(rel, "deps/") {
		return nil
	}
	base := strings.ToLower(filepath.Base(rel))
	switch filepath.Ext(base) {
	case ".pyd", ".so", ".dll", ".node":
		return fmt.Errorf("不要打包平台相关依赖：%s（改成 [[requires]]，由运行时或安装时下载提供）", rel)
	}
	if strings.HasSuffix(base, ".whl") && !strings.HasSuffix(base, "-none-any.whl") {
		return fmt.Errorf("不要打包平台相关 wheel：%s（改成 [[requires]]，由运行时或安装时下载当前平台的 wheel）", rel)
	}
	return nil
}
