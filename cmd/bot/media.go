package main

// 消息段处理：媒体路径转 base64、markdown 渲染、聊天记录摘要。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/xin/onebot/internal/onebot"
	"github.com/xin/onebot/internal/render"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// segmentImages 从消息段里抽出图片地址，给聊天面板渲染缩略图。
// OneBot 的 image 段通常带 url，没有就退回 file（本地路径或文件名）。
func segmentImages(segs []onebot.Segment, dataRoot string) []string {
	var out []string
	for _, seg := range segs {
		if seg.Type != "image" {
			continue
		}
		url, _ := seg.Data["url"].(string)
		if url == "" {
			url, _ = seg.Data["file"].(string)
		}
		if url == "" {
			continue
		}
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			out = append(out, url)
			continue
		}
		// 纯路径 / file:// / data: / base64:// / 裸 base64 都先归一化成缩略图。
		// 聊天记录里不保留原始大图和本机路径，避免 chat.json 膨胀与路径泄漏。
		var raw []byte
		switch {
		case strings.HasPrefix(url, "data:"), strings.HasPrefix(url, "base64://"):
			raw = decodeInlineImage(url)
		case strings.HasPrefix(url, "file://"):
			raw = readLocalImage(fileURLPath(url), dataRoot)
		case filepath.IsAbs(url):
			raw = readLocalImage(url, dataRoot)
		default:
			if b, err := decodeBase64Image(strings.TrimSpace(url)); err == nil {
				raw = b
			} else {
				raw = readLocalImage(filepath.Join(dataRoot, url), dataRoot)
			}
		}
		if thumb := imageThumbnail(raw); thumb != "" {
			out = append(out, thumb)
		} else {
			out = append(out, "（内联图片）")
		}
	}
	return out
}

func fileURLPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return strings.TrimPrefix(raw, "file://")
	}
	p := u.Path
	if u.Host != "" && u.Host != "localhost" {
		// Windows 的 file://C:/... 会被解析成 host=C:；拼回去。
		p = u.Host + p
	}
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // /C:/...
	}
	return p
}

func readLocalImage(path, dataRoot string) []byte {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	root, err := filepath.Abs(dataRoot)
	if err != nil {
		return nil
	}
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return nil
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() || fi.Size() > 16<<20 {
		return nil
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	return b
}

// inlineThumbnail 把 base64/data 内联图压成 240px JPEG 缩略图。
// 失败返回空串，调用方退回占位文本；聊天记录不能因此丢整条消息。
func decodeInlineImage(raw string) []byte {
	payload := strings.TrimPrefix(raw, "base64://")
	if strings.HasPrefix(payload, "data:") {
		if i := strings.Index(payload, ","); i >= 0 {
			payload = payload[i+1:]
		}
	}
	b, _ := decodeBase64Image(payload)
	return b
}

// imageThumbnail 把任意图片字节压成 240px JPEG 缩略图。
// 失败返回空串，调用方退回占位文本；聊天记录不能因此丢整条消息。
func imageThumbnail(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return ""
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return ""
	}
	const maxSide = 240
	if w > maxSide || h > maxSide {
		if w >= h {
			h = h * maxSide / w
			w = maxSide
		} else {
			w = w * maxSide / h
			h = maxSide
		}
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 76}); err != nil {
		return ""
	}
	return "base64://" + base64.StdEncoding.EncodeToString(out.Bytes())
}

func decodeBase64Image(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("base64 解码失败")
}

// segmentsText 把消息段拼成一段可读文本，给聊天面板展示用。

// segmentsText 把消息段拼成一段可读文本，给聊天面板展示用。
func segmentsText(segs []onebot.Segment) string {
	var b strings.Builder
	for _, seg := range segs {
		switch seg.Type {
		case "text":
			if t, ok := seg.Data["text"].(string); ok {
				b.WriteString(t)
			}
		case "image":
			b.WriteString("[图片]")
		case "record":
			b.WriteString("[语音]")
		case "video":
			b.WriteString("[视频]")
		case "file":
			b.WriteString("[文件]")
		case "forward":
			// 合并转发的聊天记录
			b.WriteString("[聊天记录]")
		case "json":
			// 卡片消息（分享链接、小程序等）
			b.WriteString("[卡片消息]")
		case "xml":
			b.WriteString("[卡片消息]")
		case "reply":
			b.WriteString("[回复]")
		case "poke":
			b.WriteString("[戳一戳]")
		case "share":
			b.WriteString("[分享]")
		case "music":
			b.WriteString("[音乐]")
		case "dice":
			b.WriteString("[骰子]")
		case "rps":
			b.WriteString("[猜拳]")
		case "markdown":
			b.WriteString("[Markdown]")
		case "at":
			if q, ok := seg.Data["qq"].(string); ok {
				b.WriteString("@" + q + " ")
			}
		case "face":
			b.WriteString("[表情]")
		default:
			b.WriteString("[" + seg.Type + "]")
		}
	}
	return b.String()
}

// renderMarkdown 把 text 段的 Markdown 转成纯文本。
// 放在发送前统一做，插件不用各自实现，也不会漏。

// renderMarkdown 把 text 段的 Markdown 转成纯文本。
// 放在发送前统一做，插件不用各自实现，也不会漏。
func renderMarkdown(segments []onebot.Segment) []onebot.Segment {
	out := make([]onebot.Segment, 0, len(segments))
	for _, seg := range segments {
		if seg.Type != "text" {
			out = append(out, seg)
			continue
		}
		text, _ := seg.Data["text"].(string)
		if text == "" {
			out = append(out, seg)
			continue
		}
		data := make(map[string]any, len(seg.Data))
		for k, v := range seg.Data {
			data[k] = v
		}
		data["text"] = render.ToPlain(text)
		out = append(out, onebot.Segment{Type: "text", Data: data})
	}
	return out
}

// resolveMedia 把指向 data/ 下文件的媒体段读成 base64。
// 插件之间只传路径，文件内容不经过 RPC，大图也不会撑爆帧。

// resolveMedia 把指向 data/ 下文件的媒体段读成 base64。
// 插件之间只传路径，文件内容不经过 RPC，大图也不会撑爆帧。
func resolveMedia(segments []onebot.Segment, dataRoot string) ([]onebot.Segment, error) {
	root, err := filepath.Abs(dataRoot)
	if err != nil {
		return segments, nil
	}

	out := make([]onebot.Segment, 0, len(segments))
	for _, seg := range segments {
		switch seg.Type {
		case "image", "record", "file", "video":
		default:
			out = append(out, seg)
			continue
		}

		file, _ := seg.Data["file"].(string)
		if file == "" || strings.HasPrefix(file, "http://") ||
			strings.HasPrefix(file, "https://") || strings.HasPrefix(file, "base64://") {
			out = append(out, seg)
			continue
		}

		abs, err := filepath.Abs(file)
		if err != nil {
			return nil, fmt.Errorf("非法媒体路径: %s", file)
		}
		if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return nil, fmt.Errorf("媒体路径必须在 data/ 下: %s", file)
		}

		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("读取媒体失败: %w", err)
		}
		if len(raw) > 20<<20 {
			return nil, fmt.Errorf("媒体超过 20MB: %s", file)
		}

		// 复制一份再改，避免污染调用方拿到的对象
		data := make(map[string]any, len(seg.Data))
		for k, v := range seg.Data {
			data[k] = v
		}
		data["file"] = "base64://" + base64.StdEncoding.EncodeToString(raw)
		out = append(out, onebot.Segment{Type: seg.Type, Data: data})
	}
	return out, nil
}

// 单张图上限与整条消息兜底上限。QQ/协议端按张限制图片大小，框架也按张检查；
// 总量上限只防内存/WS 帧过大，正常多图消息碰不到。
const (
	maxImageBytes   = 20 << 20 // 单张 20 MB（与媒体文件读取上限一致）
	maxMessageBytes = 64 << 20 // 整条消息兜底 64 MB
)

// segmentBytes 估算单个消息段里内联数据的字节数。
// base64:// 按解码后的真实大小算（长度 * 3/4），其余按字段长度。
func segmentBytes(seg onebot.Segment) int {
	total := 0
	for k, v := range seg.Data {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if (k == "file" || k == "url") && strings.HasPrefix(s, "base64://") {
			total += len(strings.TrimPrefix(s, "base64://")) / 4 * 3
			continue
		}
		total += len(s)
	}
	return total
}

// approxSize 估算消息段序列化后的大小，用于挡掉过大的 base64。
func approxSize(segments []onebot.Segment) int {
	total := 0
	for _, seg := range segments {
		if s, ok := seg.Data["file"].(string); ok {
			total += len(s)
		}
		if s, ok := seg.Data["text"].(string); ok {
			total += len(s)
		}
		total += 64
	}
	return total
}

func parseSegments(raw json.RawMessage) []onebot.Segment {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return onebot.ParseMessage(v)
}

// groupIDOfParams 从 send 类 action 的 params 里抠群号。
// params 刚从 JSON 解出来（数字=float64，有的实现发字符串），都接。

// segmentsSummary 把消息段压成审计里能看懂的一行。
// 例：你好呀 [图] [语音] [@123]
func segmentsSummary(segs []onebot.Segment) string {
	var b strings.Builder
	for _, seg := range segs {
		switch seg.Type {
		case "text":
			if t, ok := seg.Data["text"].(string); ok {
				b.WriteString(t)
			}
		case "image":
			b.WriteString("[图]")
		case "record":
			b.WriteString("[语音]")
		case "video":
			b.WriteString("[视频]")
		case "at":
			fmt.Fprintf(&b, "[@%v]", seg.Data["qq"])
		case "face":
			b.WriteString("[表情]")
		default:
			fmt.Fprintf(&b, "[%s]", seg.Type)
		}
		b.WriteString(" ")
	}
	out := strings.TrimSpace(b.String())
	r := []rune(out)
	if len(r) > 60 {
		out = string(r[:60]) + "…"
	}
	return out
}
