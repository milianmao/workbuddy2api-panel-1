package pool

import (
	"fmt"
	"math"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// DailyQuota 是账号按北京时间计算的日限额视图，与禁用、冷却和余额相互独立。
type DailyQuota struct {
	Day             string    `json:"day"`
	Used            float64   `json:"used"`
	Limit           float64   `json:"limit"`
	Override        *float64  `json:"override"`
	Exceeded        bool      `json:"exceeded"`
	ResetAt         time.Time `json:"reset_at"`
	UsageIncomplete bool      `json:"usage_incomplete"`
}

// dailyCreditState 只保存实测消费；额度来自配置，切日后旧累计不再参与判定。
type dailyCreditState struct {
	Day        string  `json:"day"`
	Used       float64 `json:"used"`
	Incomplete bool    `json:"incomplete,omitempty"`
}

// DailyQuotaError 表示本地每日额度耗尽，不是上游故障，调用方不得据此惩罚账号。
type DailyQuotaError struct {
	Used    float64
	Limit   float64
	ResetAt time.Time
}

// Error 给 API 和后台任务提供同一份可操作的限额原因。
func (e *DailyQuotaError) Error() string {
	return fmt.Sprintf("账号已达每日限额（已用 %.6g / %.6g 积分），北京时间 %s 恢复", e.Used, e.Limit, e.ResetAt.In(cst.Loc).Format("2006-01-02 15:04:05"))
}

// SetDailyCreditLimits 热更新额度并复制覆盖表；nil 继承默认，显式零值不限额，不清累计。
func (p *Pool) SetDailyCreditLimits(limit float64, overrides map[string]*float64) {
	if math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
		return
	}
	values := make(map[string]float64, len(overrides))
	for uid, v := range overrides {
		if v != nil && *v >= 0 && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
			values[uid] = *v
		}
	}
	p.mu.Lock()
	p.dailyCreditLimit, p.accountDailyCreditLimits = limit, values
	blocked := false
	for _, e := range p.byUID {
		if p.dailyQuotaBlocked(e, time.Now()) {
			blocked = true
			break
		}
	}
	p.mu.Unlock()
	if blocked {
		p.Flush()
	}
}

// dailyQuotaOf 在持池锁时生成视图；读取时惰性切日，避免为零点另建调度任务。
func (p *Pool) dailyQuotaOf(e *entry, now time.Time) DailyQuota {
	local := now.In(cst.Loc)
	q := DailyQuota{
		Day: cst.Day(now), Limit: p.dailyCreditLimit,
		ResetAt: time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, cst.Loc),
	}
	if v, ok := p.accountDailyCreditLimits[e.a.UID]; ok {
		q.Limit, q.Override = v, &v
	}
	if e.dailyCredit.Day == q.Day {
		q.Used, q.UsageIncomplete = e.dailyCredit.Used, e.dailyCredit.Incomplete
	}
	q.Exceeded = q.Limit > 0 && q.Used >= q.Limit
	return q
}

// dailyQuotaBlocked 与所有选号、容量和出站准入共用同一判据；调用方须持池锁。
func (p *Pool) dailyQuotaBlocked(e *entry, now time.Time) bool {
	limit := p.dailyCreditLimit
	if v, ok := p.accountDailyCreditLimits[e.a.UID]; ok {
		limit = v
	}
	return limit > 0 && e.dailyCredit.Used >= limit && e.dailyCredit.Day == cst.Day(now)
}

// CheckDailyQuota 在真实模型出站前再检查，防止已选中的账号在等待期间达到限额。
// 不检查暂停或冷却，因为后台签到、成长任务已有各自的账号选择规则。
func (p *Pool) CheckDailyQuota(uid string) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e := p.byUID[uid]; e != nil {
		q := p.dailyQuotaOf(e, time.Now())
		if q.Exceeded {
			return &DailyQuotaError{Used: q.Used, Limit: q.Limit, ResetAt: q.ResetAt}
		}
	}
	return nil
}

// RecordDailyCredit 对每次真实上游调用结算一次，缺失或非法回执只标记用量不完整。
// 不限额时也累计，防止开启/修改额度重置当日账本；在途完成即使超额仍须入账。
func (p *Pool) RecordDailyCredit(uid string, credit float64, observed bool) {
	p.mu.Lock()
	e := p.byUID[uid]
	if e == nil {
		p.mu.Unlock()
		return
	}
	q := p.dailyQuotaOf(e, time.Now())
	if e.dailyCredit.Day != q.Day {
		e.dailyCredit = dailyCreditState{Day: q.Day}
	}
	if !observed || credit < 0 || math.IsNaN(credit) || math.IsInf(credit, 0) {
		e.dailyCredit.Incomplete = true
	} else {
		sum := e.dailyCredit.Used + credit
		if math.IsInf(sum, 0) {
			sum = math.MaxFloat64
		} else if sum < math.MaxFloat64/creditBalancePrecision {
			sum = math.Round(sum*creditBalancePrecision) / creditBalancePrecision
		}
		e.dailyCredit.Used = sum
	}
	crossed := !q.Exceeded && q.Limit > 0 && e.dailyCredit.Used >= q.Limit
	p.dirty.Store(true)
	p.mu.Unlock()
	if crossed {
		p.Flush() // 达额即保存，避免紧接着重启重新放行；不在持池锁时递归 Flush。
	}
}

// DailyQuotaErrorForRealm 在无号可选时查找本来健康、却被日限额拦住的账号。
// 其他故障仍沿原有错误路径，不把禁用/模型关闭误报成等待零点即可恢复。
func (p *Pool) DailyQuotaErrorForRealm(realm, model string) *DailyQuotaError {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if (realm != "" && e.a.Realm() != realm) || !e.healthyForModel(now, model) ||
			p.floorBlockedForRealmModel(e, model, realm, now) || p.reserveBlocked(e, p.modelFreeOf(e, model)) {
			continue
		}
		q := p.dailyQuotaOf(e, now)
		if q.Exceeded {
			return &DailyQuotaError{Used: q.Used, Limit: q.Limit, ResetAt: q.ResetAt}
		}
	}
	return nil
}
