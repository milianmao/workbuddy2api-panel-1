package server

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// writeDailyQuotaError 让聊天、图片及适配入口共用本地额度错误，不混入上游积分耗尽或熔断。
func writeDailyQuotaError(w http.ResponseWriter, limited *pool.DailyQuotaError) {
	seconds := max(1, int(math.Ceil(time.Until(limited.ResetAt).Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeOpenAIError(w, http.StatusTooManyRequests, "account_daily_limit_exceeded", limited.Error())
}
