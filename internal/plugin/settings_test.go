package plugin

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSettingsBackupAndFallback(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSettings(dir, "demo", map[string]any{"water_groups": "111"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, "demo", map[string]any{"water_groups": "222"}); err != nil {
		t.Fatal(err)
	}
	// 主文件被删，应该读回上一份备份 111
	if err := os.Remove(SettingsPath(dir, "demo")); err != nil {
		t.Fatal(err)
	}
	got := LoadSettings(dir, "demo")
	if got["water_groups"] != "111" {
		t.Fatalf("应回退到 backup 111，得到 %#v", got)
	}
}

func TestSettingsCorruptMainFallsBackToBackup(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSettings(dir, "demo", map[string]any{"water_groups": "111"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, "demo", map[string]any{"water_groups": "222"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsPath(dir, "demo"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadSettings(dir, "demo")
	if got["water_groups"] != "111" {
		t.Fatalf("损坏主文件应回退 backup 111，得到 %#v", got)
	}
}

func TestSettingsBackupRotation(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"1", "2", "3", "4", "5"} {
		if err := SaveSettings(dir, "demo", map[string]any{"water_groups": v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(SettingsPath(dir, "demo")); err != nil {
		t.Fatal(err)
	}
	// .bak 是 4，.bak.1 是 3，.bak.2 是 2，.bak.3 是 1；最多 4 份。
	want := map[int]string{0: "4", 1: "3", 2: "2", 3: "1"}
	for i, v := range want {
		b, err := os.ReadFile(settingsBackupPath(SettingsPath(dir, "demo"), i))
		if err != nil {
			t.Fatalf("backup %d 缺失: %v", i, err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["water_groups"] != v {
			t.Fatalf("backup %d 应为 %s，得到 %v", i, v, got["water_groups"])
		}
	}
}

func TestRestoreSettingsBackup(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSettings(dir, "demo", map[string]any{"water_groups": "111"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, "demo", map[string]any{"water_groups": "222"}); err != nil {
		t.Fatal(err)
	}
	values, err := RestoreSettingsBackup(dir, "demo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if values["water_groups"] != "111" {
		t.Fatalf("restore values = %#v", values)
	}
	if got := LoadSettings(dir, "demo"); got["water_groups"] != "111" {
		t.Fatalf("current after restore = %#v", got)
	}
}
