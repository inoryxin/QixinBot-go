package plugin

// 官方签名公钥（编译进二进制）。
//
// **为什么内置而不是让用户配**：
//
//	用户装官方插件前不该先用记事本改 config。官方的信任是框架自带的，
//	不依赖每个部署者的手工操作——否则每发一个官方插件，所有用户都要
//	同步改一次配置。
//
// **为什么是常量而不是"从网上下载"**：
//
//	启动时去拉公钥等于把信任链交给网络。写死在二进制里，
//	换公钥必须发新版本——这正是我们想要的（用户知情地升级）。
//
// **换密钥怎么做**：
//
//	把新公钥加进这个数组（保留旧的，让用旧包的人还能装），
//	发个新版本。旧密钥在下一个大版本再摘掉。
var officialKeys = []string{
	// 2026-10 首把官方密钥（qixinpack keygen -out plugin-signing）
	"UkVeBEDsEKbcwm+2bsVgk1zhwj78z74x5ygnG2E7+9I=",
}

// OfficialKeys 返回框架内置的官方公钥（拷贝，调用方随便改）。
func OfficialKeys() []string {
	return append([]string(nil), officialKeys...)
}

// IsOfficialKey 判断某个公钥是不是官方的。
func IsOfficialKey(key string) bool {
	key = trimKey(key)
	for _, k := range officialKeys {
		if trimKey(k) == key {
			return true
		}
	}
	return false
}

// OfficialFingerprints 返回官方公钥的指纹列表（给界面和 CI 用）。
func OfficialFingerprints() []string {
	out := make([]string, 0, len(officialKeys))
	for _, k := range officialKeys {
		if fp := FingerprintOf(k); fp != "" {
			out = append(out, fp)
		}
	}
	return out
}
