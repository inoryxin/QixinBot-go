package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SettingsFile 是插件设置的存放位置（相对插件数据目录）。
const SettingsFile = "settings.json"

// SettingsPath 返回某个插件运行区的设置文件路径。
func SettingsPath(runtimeDir, id string) string {
	return filepath.Join(runtimeDir, id, "data", SettingsFile)
}

// SettingsBackup 是一份可恢复的设置备份元信息。
type SettingsBackup struct {
	Index   int
	ModTime time.Time
	Size    int64
}

// SettingsBackups 返回最近 4 份备份（0 = 最近）。
func SettingsBackups(runtimeDir, id string) []SettingsBackup {
	path := SettingsPath(runtimeDir, id)
	out := make([]SettingsBackup, 0, 4)
	for i := 0; i <= 3; i++ {
		fi, err := os.Stat(settingsBackupPath(path, i))
		if err != nil {
			continue
		}
		out = append(out, SettingsBackup{Index: i, ModTime: fi.ModTime(), Size: fi.Size()})
	}
	return out
}

// LoadSettingsBackup 读取指定备份；不存在或 JSON 损坏返回错误。
func LoadSettingsBackup(runtimeDir, id string, index int) (map[string]any, error) {
	if index < 0 || index > 3 {
		return nil, fmt.Errorf("备份序号越界")
	}
	raw, err := os.ReadFile(settingsBackupPath(SettingsPath(runtimeDir, id), index))
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("备份 JSON 损坏: %w", err)
	}
	return out, nil
}

// RestoreSettingsBackup 把指定备份写回当前设置（当前设置仍会先进入备份）。
func RestoreSettingsBackup(runtimeDir, id string, index int) (map[string]any, error) {
	values, err := LoadSettingsBackup(runtimeDir, id, index)
	if err != nil {
		return nil, err
	}
	if err := SaveSettings(runtimeDir, id, values); err != nil {
		return nil, err
	}
	return values, nil
}

// LoadSettings 读取已保存的设置（不含清单默认值）。
func LoadSettings(runtimeDir, id string) map[string]any {
	out := map[string]any{}
	path := SettingsPath(runtimeDir, id)
	raw, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(raw, &out) == nil {
			return out
		}
	}
	// 主文件丢了或损坏时依次尝试最近几份备份。
	for i := 0; i <= 3; i++ {
		raw, err = os.ReadFile(settingsBackupPath(path, i))
		if err != nil {
			continue
		}
		if json.Unmarshal(raw, &out) == nil {
			return out
		}
	}
	return map[string]any{}
}

// settingsBackupPath: 0 = settings.json.bak（最近），1..3 = 更早的轮转备份。
func settingsBackupPath(path string, i int) string {
	if i == 0 {
		return path + ".bak"
	}
	return fmt.Sprintf("%s.bak.%d", path, i)
}

// backupSettings 把当前 settings.json 轮转进 .bak，最多保留 4 份。
func backupSettings(path string) {
	old, err := os.ReadFile(path)
	if err != nil || len(old) == 0 {
		return
	}
	for i := 2; i >= 0; i-- {
		_ = os.Rename(settingsBackupPath(path, i), settingsBackupPath(path, i+1))
	}
	_ = os.WriteFile(settingsBackupPath(path, 0), old, 0o644)
}

// SaveSettings 原子写入设置；覆盖前保留上一份 settings.json.bak，
// 便于升级/误保存后人工恢复。
func SaveSettings(runtimeDir, id string, values map[string]any) error {
	path := SettingsPath(runtimeDir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	backupSettings(path)
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EffectiveSettings 把清单里的默认值和已保存的值合并。
func EffectiveSettings(man *Manifest, saved map[string]any) map[string]any {
	out := make(map[string]any, len(man.Settings))
	for _, s := range man.Settings {
		if s.Default != nil {
			out[s.Key] = s.Default
		} else {
			out[s.Key] = ""
		}
	}
	for k, v := range saved {
		out[k] = v
	}
	return out
}
