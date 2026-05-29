package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type YipayResp struct {
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
	TradeNo string `json:"trade_no"`
	PayURL  string `json:"payurl"`
	QRCode  string `json:"qrcode"`
}

func yipaySign(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		v := params[k]
		if v == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(v)
	}
	sb.WriteString(key)

	h := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(h[:])
}

// yipayCreateOrder 调用易支付创建订单，返回支付跳转 URL
func yipayCreateOrder(apiURL, pid, key, outTradeNo, payType, name, money, notifyURL, returnURL string) (string, error) {
	params := map[string]string{
		"pid":          pid,
		"type":         payType,
		"out_trade_no": outTradeNo,
		"notify_url":   notifyURL,
		"return_url":   returnURL,
		"name":         name,
		"money":        money,
		"sign_type":    "MD5",
	}
	params["sign"] = yipaySign(params, key)

	apiURL = strings.TrimRight(apiURL, "/")
	if !strings.Contains(apiURL, "/api.php") {
		apiURL += "/api.php"
	}

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, form)
	if err != nil {
		return "", fmt.Errorf("请求易支付失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	var result YipayResp
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析响应失败: %s", string(body))
	}
	if result.Code != 1 {
		return "", fmt.Errorf("易支付返回错误: %s", result.Msg)
	}

	if result.PayURL != "" {
		return result.PayURL, nil
	}
	if result.QRCode != "" {
		return result.QRCode, nil
	}
	return "", fmt.Errorf("易支付未返回支付链接")
}

// yipayVerifyCallback 验证易支付回调签名
func yipayVerifyCallback(params map[string]string, key string) bool {
	sign, ok := params["sign"]
	if !ok || sign == "" {
		return false
	}

	// sign_type 不参与签名
	delete(params, "sign")
	delete(params, "sign_type")

	got := yipaySign(params, key)
	return got == sign
}
