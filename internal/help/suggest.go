package help

import "strings"

// Suggest 在候选命令里找和输入最接近的一个。
// 只用于"命令没匹配上"的场景，避免误触发。
// 中文和英文都按字符比较，距离 1~2 认为是手滑。
func Suggest(input string, candidates []string) (string, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", false
	}
	// 只比较第一个词（命令名本身，不含参数）
	if i := strings.IndexAny(input, " \t　"); i > 0 {
		input = input[:i]
	}
	// 太短的输入不猜，避免 "a" 匹配到一堆东西
	if len([]rune(input)) < 2 {
		return "", false
	}

	best := ""
	bestDist := 3 // 只接受 1~2
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" || c == input {
			continue
		}
		d := distance(input, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// distance 是 Levenshtein 编辑距离，跑满整张表（命令名都很短，够快）。
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
