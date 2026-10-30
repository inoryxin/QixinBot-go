// Package backup 打包/恢复整个运行目录。
package backup

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Create 把运行目录打包成 zip。包含：
//   - config.toml
//   - plugins/*.anzu
//   - data/ 全部
//
// 不包含 bin/（二进制可重新构建）。
func Create(root, out string) (files int, bytes int64, err error) {
	outAbs, _ := filepath.Abs(out)
	f, err := os.Create(out)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	add := func(path string) error {
		// 备份产物自身不能进包：out 写在 data/ 里时，
		// 边写边读自己会得到一个不断膨胀的自引用 zip
		if abs, aerr := filepath.Abs(path); aerr == nil && abs == outAbs {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || strings.HasPrefix(rel, "../") {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		hdr.Name = rel
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer src.Close()
		n, _ := io.Copy(w, src)
		files++
		bytes += n
		return nil
	}

	// config.toml
	if _, err := os.Stat(filepath.Join(root, "config.toml")); err == nil {
		if err := add(filepath.Join(root, "config.toml")); err != nil {
			return 0, 0, err
		}
	}

	// plugins/*.anzu
	entries, _ := os.ReadDir(filepath.Join(root, "plugins"))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".anzu") {
			continue
		}
		if err := add(filepath.Join(root, "plugins", e.Name())); err != nil {
			return 0, 0, err
		}
	}

	// data/ 全部
	dataDir := filepath.Join(root, "data")
	if _, err := os.Stat(dataDir); err == nil {
		err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			return add(path)
		})
		if err != nil {
			return 0, 0, err
		}
	}

	if err := zw.Close(); err != nil {
		return 0, 0, err
	}
	st, _ := f.Stat()
	return files, st.Size(), nil
}

// Restore 解压备份到 root。force=false 时目标已存在则拒绝，避免误覆盖。
func Restore(root, in string, force bool) (int, error) {
	if !force {
		if _, err := os.Stat(filepath.Join(root, "data")); err == nil {
			return 0, fmt.Errorf("目标目录已有 data/，加 -force 才会覆盖")
		}
	}

	zr, err := zip.OpenReader(in)
	if err != nil {
		return 0, fmt.Errorf("不是合法的 zip: %w", err)
	}
	defer zr.Close()

	cleanRoot := filepath.Clean(root)
	n := 0
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return n, fmt.Errorf("包内非法路径: %s", f.Name)
		}
		dest := filepath.Join(cleanRoot, clean)
		if !strings.HasPrefix(dest, cleanRoot+string(os.PathSeparator)) {
			return n, fmt.Errorf("包内非法路径: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(dest, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return n, err
		}
		rc, err := f.Open()
		if err != nil {
			return n, err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = rc.Close()
			return n, err
		}
		_, copyErr := io.Copy(out, rc)
		_ = out.Close()
		_ = rc.Close()
		if copyErr != nil {
			return n, copyErr
		}
		n++
	}
	return n, nil
}
