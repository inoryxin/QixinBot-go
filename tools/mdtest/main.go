package main

import (
	"fmt"

	"github.com/xin/onebot/internal/render"
)

func main() {
	cases := []string{
		"# 标题\n**粗体** 和 *斜体*",
		"- 第一项\n- 第二项",
		"`code` 和 ```go\nfmt.Println(1)\n```",
		"[链接](https://example.com) 和 > 引用",
		"算式 2*3*4 不该被改",
		"变量 snake_case_name 不该被改",
		"纯文本没有任何标记",
	}
	for _, c := range cases {
		fmt.Printf("IN : %q\nOUT: %q\n\n", c, render.ToPlain(c))
	}
}
