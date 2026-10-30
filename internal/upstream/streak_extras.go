// streak_extras.go 新手礼包 / 连登 heatmap / 补签卡 API（growth / billing 域的增补接口）。
//
// 原 blackcat.go：夜猫子 black_cat 已从上游下架，任务链与排程一并删除。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// ClaimGift 领取新手礼包（每号一次，已领返回业务错误）。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapBrokenDates 返回本月（今天所在月的 1 号起）到今天（不含）之间
// score==0 的日期，升序。
//
// 下界只能取月首，不能更宽：score==0 同时表示「那天漏签」和「那天账号还不存在」
// （探针实测：注册前的日子也是 0 分，heatmap 的 range 是近 365 天），只有上游能
// 区分两者——所以这里只做时间窗收窄，交给 UseMakeupCard 的拒绝兜底。
// 今天不算：当天未结束，0 分不代表漏签。today.date 由响应给出，不读宿主机时钟
// （宿主机若为 UTC，本地 08:00 前会算错天）。
func (c *Client) HeatmapBrokenDates(a *auth.Auth) ([]string, error) {
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Today struct {
			Date string `json:"date"`
		} `json:"today"`
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if len(resp.Today.Date) < 10 {
		return nil, fmt.Errorf("heatmap 缺 today.date（结构未知，不敢猜窗口）")
	}
	today := resp.Today.Date[:10]
	monthStart := today[:8] + "01"
	var out []string
	for _, cell := range resp.Cells {
		d := cell.Date
		if len(d) < 10 {
			continue
		}
		d = d[:10]
		if cell.Score == 0 && d >= monthStart && d < today {
			out = append(out, d)
		}
	}
	sort.Strings(out) // 上游是否保证升序未验证
	return out, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}
