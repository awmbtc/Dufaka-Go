package pay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Checkout 是钱包返回的收银单。
type Checkout struct {
	ID           string `json:"id"`
	MerchantName string `json:"merchantName"`
	OrderID      string `json:"orderId"`
	Title        string `json:"title"`
	Amount       int64  `json:"amount"`
	CnyFen       int64  `json:"cnyFen"`
	Status       string `json:"status"`
}

// Wallet 用商户密钥调用 cldx-wallet。密钥只留在小店服务器。
type Wallet struct {
	Base       string
	MerchantID string
	Secret     string
	HTTP       *http.Client
}

func (w Wallet) Enabled() bool {
	return w.Base != "" && w.MerchantID != "" && w.Secret != ""
}

func (w Wallet) Create(ctx context.Context, orderID, title string, cnyFen int64, ttl time.Duration) (Checkout, error) {
	body, _ := json.Marshal(map[string]any{
		"orderId": orderID, "title": title, "cnyFen": cnyFen, "ttlSeconds": int(ttl.Seconds()),
	})
	return w.call(ctx, http.MethodPost, "/v1/checkouts", body)
}

func (w Wallet) ByOrder(ctx context.Context, orderID string) (Checkout, error) {
	return w.call(ctx, http.MethodGet, "/v1/orders/"+orderID+"/checkout", nil)
}

// Quote 把人民币分折成 cldx 最小单位。
func (w Wallet) Quote(ctx context.Context, cnyFen int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(w.Base, "/")+"/v1/spend-quote?cnyFen="+strconv.FormatInt(cnyFen, 10), nil)
	if err != nil {
		return 0, err
	}
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("钱包返回 %d", res.StatusCode)
	}
	var body struct {
		Amount int64 `json:"amount"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, err
	}
	return body.Amount, nil
}

// Receipt 是公开的到账查询，对应钱包 GET /v1/payments?to=&order= 的返回
// {"to","order","from","amount","refunded","paid","at"}。钱包不返回付款编号：
// 付款方地址 From 和到账时间 At 是可选字段，缺失或格式不对都不影响解析。
type Receipt struct {
	Paid     bool
	Amount   int64
	Refunded int64
	From     string    // 付款方地址；钱包没返回时为空
	At       time.Time // 到账时间（RFC3339）；钱包没返回或格式不对时为零值
}

// UnmarshalJSON 只要求 paid / amount / refunded 类型正确；from / at 是可选字段，
// 缺失、为 null 或类型不对一律忽略，不让一笔已到账的付款因为附带字段而解析失败。
func (r *Receipt) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var core struct {
		Paid     bool  `json:"paid"`
		Amount   int64 `json:"amount"`
		Refunded int64 `json:"refunded"`
	}
	if err := json.Unmarshal(raw, &core); err != nil {
		return err
	}
	*r = Receipt{Paid: core.Paid, Amount: core.Amount, Refunded: core.Refunded}
	var from string
	if json.Unmarshal(fields["from"], &from) == nil {
		r.From = strings.TrimSpace(from)
	}
	var at string
	if json.Unmarshal(fields["at"], &at) == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(at)); err == nil {
			r.At = t
		}
	}
	return nil
}

func (w Wallet) Receipt(ctx context.Context, payee, orderID string) (Receipt, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(w.Base, "/")+"/v1/payments?to="+urlQuery(payee)+"&order="+urlQuery(orderID), nil)
	if err != nil {
		return Receipt{}, err
	}
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return Receipt{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return Receipt{}, fmt.Errorf("钱包返回 %d", res.StatusCode)
	}
	var item Receipt
	if err := json.Unmarshal(raw, &item); err != nil {
		return Receipt{}, err
	}
	return item, nil
}

func (w Wallet) call(ctx context.Context, method, path string, body []byte) (Checkout, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(w.Base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return Checkout{}, err
	}
	req.Header.Set("Authorization", "Bearer "+w.Secret)
	req.Header.Set("X-Merchant-Id", w.MerchantID)
	req.Header.Set("Content-Type", "application/json")
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return Checkout{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return Checkout{}, fmt.Errorf("钱包返回 %d", res.StatusCode)
	}
	var item Checkout
	if err := json.Unmarshal(raw, &item); err != nil {
		return Checkout{}, err
	}
	return item, nil
}

func urlQuery(v string) string { return url.QueryEscape(v) }
