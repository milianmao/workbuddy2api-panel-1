package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestDailyQuotaConfigSave 验证部分保存的继承/覆盖实际改变准入，非法额度不会落盘或热应用。
func TestDailyQuotaConfigSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.RecordDailyCredit("a", 2, true)
	p.RecordDailyCredit("b", 2, true)
	live, up := livecfg.New(livecfg.Snapshot{}), upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	save := func(raw string) error {
		_, err := saveConfig([]byte(raw), path, live, p, up, sch, nil, nil)
		return err
	}
	if err := save(`{"pool":{"daily_credit_limit":1,"account_daily_credit_limits":{"a":0}}}`); err != nil {
		t.Fatal(err)
	}
	if p.CheckDailyQuota("a") != nil || p.CheckDailyQuota("b") == nil {
		t.Fatal("单号不限额/全局额度未热应用")
	}
	before, _ := os.ReadFile(path)
	if err := save(`{"pool":{"account_daily_credit_limits":{"a":-1}}}`); err == nil {
		t.Fatal("负额度被接受")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || p.CheckDailyQuota("a") != nil {
		t.Fatal("非法额度改变了配置或运行态")
	}
	// 不同账号的部分保存并发到达时，读-合并-写必须串行，不能互相覆盖。
	var wg sync.WaitGroup
	for _, body := range []string{`{"pool":{"account_daily_credit_limits":{"a":null}}}`, `{"pool":{"account_daily_credit_limits":{"b":0}}}`} {
		wg.Go(func() {
			if err := save(body); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if p.CheckDailyQuota("a") == nil || p.CheckDailyQuota("b") != nil {
		t.Fatal("清除覆盖/并发部分保存未生效")
	}
}
