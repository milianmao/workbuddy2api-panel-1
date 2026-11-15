package panel

import (
	"os"
	"strings"
	"testing"
)

// 导入是两步动作：选中文件先出预览（dryRun），点「确认导入」才写盘。
//
// 这条契约靠三个前端事实保证，缺一个就会退化成「一点就导入」：
//  1. 文件选择框是多选的，且预览区与确认按钮都在页面上；
//  2. onchange 发的是 dryRun 预览，不是写入；
//  3. 写入路径上有 importPending.ready 这道闸——没成功预览过就不让写。
//
// 变异判据：把 ready 判断删掉、或把 onchange 里的 postImport(true) 改成 false，
// 本用例都会红。
func TestAppJSImportPreviewsBeforeCommit(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{`id="importFile"`, `id="importPreview"`, `id="btnImportCommit"`} {
		if !strings.Contains(h, want) {
			t.Fatalf("index.html 缺少 %s（预览区/确认按钮是多文件导入的组成部分）", want)
		}
	}
	// 文件框必须带 multiple，否则「多文件批量导入」名存实亡。
	idx := strings.Index(h, `id="importFile"`)
	if idx < 0 {
		t.Fatal("index.html 里找不到 importFile")
	}
	tail := h[idx:]
	end := strings.Index(tail, ">")
	if end < 0 || !strings.Contains(tail[:end], "multiple") {
		t.Fatalf("importFile 必须带 multiple：%s", tail[:min(end+1, len(tail))])
	}

	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for _, want := range []string{
		"const d = await postImport(true);",  // 选择文件 → 预览
		"const d = await postImport(false);", // 确认 → 写入
		"if (!importPending || !importPending.ready) return;",
		"fd.append('dryRun', 'true');",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少导入两步契约的关键片段：%q", want)
		}
	}

	// onchange 里不得出现写入调用：否则选完文件就直接落盘了。
	start := strings.Index(js, "$('importFile').onchange")
	commit := strings.Index(js, "$('btnImportCommit').onclick")
	if start < 0 || commit < 0 || commit < start {
		t.Fatal("app.js 找不到 importFile.onchange / btnImportCommit.onclick 的绑定顺序")
	}
	if body := js[start:commit]; strings.Contains(body, "postImport(false)") {
		t.Fatal("选择文件时不得直接写入：写入必须发生在「确认导入」之后")
	}
}

// 「确认导入」是 index.html 里的静态按钮，disabled / 文案不会随视图重建而复位。
// 2026-11-15 实机故障：成功导入一批之后，本次页面里再选文件，按钮正常显示但点不动
// （CSS button:disabled{opacity:.5}，浏览器直接不派发 click），只能刷新页面。
//
// 变异判据：把下面任意一处 `disabled = false` 删掉、或把收尾还原整段删掉，本用例就红。
func TestAppJSImportCommitButtonRecovers(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)

	const reenable = "$('btnImportCommit').disabled = false;"
	if n := strings.Count(js, reenable); n < 3 {
		t.Fatalf("重置「确认导入」disabled 的地方只有 %d 处，至少要 3 处"+
			"（重新选文件 / 预览通过 / 点击收尾）", n)
	}

	click := strings.Index(js, "$('btnImportCommit').onclick")
	if click < 0 {
		t.Fatal("app.js 找不到 btnImportCommit.onclick")
	}
	body := js[click:]
	if end := strings.Index(body, "\n};"); end >= 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "btn.disabled = true;") {
		t.Fatal("点击「确认导入」必须先置灰，防重复提交")
	}
	if !strings.Contains(body, "btn.disabled = false;") {
		t.Fatal("点击路径收尾必须把 disabled 还原，否则一次导入后按钮永久点不动")
	}
	if !strings.Contains(body, "btn.textContent = '确认导入';") {
		t.Fatal("点击路径收尾必须把文案还原，否则会停在「导入中…」")
	}
	if strings.Index(body, "btn.disabled = false;") < strings.Index(body, "btn.disabled = true;") {
		t.Fatal("还原必须发生在置灰之后")
	}

	// 连续第二次导入的第一步：重新选文件时就要复位。
	change := strings.Index(js, "$('importFile').onchange")
	if change < 0 || click < change {
		t.Fatal("app.js 找不到 importFile.onchange")
	}
	if !strings.Contains(js[change:click], reenable) {
		t.Fatal("重新选文件时必须复位「确认导入」的 disabled（连续导入的入口）")
	}
}
