package main

import (
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
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
	PayType string `json:"pay_type"`
	PayInfo string `json:"pay_info"`
}

// yipaySign MD5 签名（用于请求签名）
func yipaySign(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		v := params[k]
		if v == "" || k == "sign" || k == "sign_type" {
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

// yipayRsaSign RSA 签名（SHA256WithRSA）
func yipayRsaSign(params map[string]string, privKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privKeyPEM))
	if block == nil {
		return "", fmt.Errorf("解析私钥失败")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		priv2, err2 := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err2 != nil {
			return "", fmt.Errorf("解析私钥失败: %w", err2)
		}
		priv = priv2
	}

	// 构建待签名字符串（同 MD5 签名规则，剔除 sign/sign_type）
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		v := params[k]
		if v == "" || k == "sign" || k == "sign_type" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(v)
	}

	hash := sha256.Sum256([]byte(sb.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv.(*rsa.PrivateKey), crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("RSA 签名失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// yipayRsaVerify RSA 验签（SHA256WithRSA）
func yipayRsaVerify(params map[string]string, signB64 string, pubKeyPEM string) bool {
	block, _ := pem.Decode([]byte(pubKeyPEM))
	if block == nil {
		log.Printf("RSA 验签: 解析公钥失败")
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		pub2, err2 := x509.ParsePKCS1PublicKey(block.Bytes)
		if err2 != nil {
			log.Printf("RSA 验签: 解析公钥失败: %v", err2)
			return false
		}
		pub = pub2
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		log.Printf("RSA 验签: 非 RSA 公钥")
		return false
	}

	// 构建待签名字符串
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		v := params[k]
		if v == "" || k == "sign" || k == "sign_type" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(v)
	}

	sig, err := base64.StdEncoding.DecodeString(signB64)
	if err != nil {
		log.Printf("RSA 验签: Base64 解码失败: %v", err)
		return false
	}

	hash := sha256.Sum256([]byte(sb.String()))
	err = rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hash[:], sig)
	if err != nil {
		log.Printf("RSA 验签: 验证失败: %v", err)
		return false
	}
	return true
}

// yipayCreateOrder 调用 keyingpay 创建订单，返回支付跳转 URL
func yipayCreateOrder(apiURL, pid, key, outTradeNo, payType, name, money, notifyURL, returnURL, clientIP string) (string, error) {
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	params := map[string]string{
		"pid":          pid,
		"method":       "jump",
		"type":         payType,
		"out_trade_no": outTradeNo,
		"notify_url":   notifyURL,
		"return_url":   returnURL,
		"name":         name,
		"money":        money,
		"clientip":     clientIP,
		"timestamp":    timestamp,
		"sign_type":    "MD5",
	}
	params["sign"] = yipaySign(params, key)

	apiURL = strings.TrimRight(apiURL, "/")
	if !strings.HasSuffix(apiURL, "/api/pay/create") {
		apiURL += "/api/pay/create"
	}

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	log.Printf("[易支付] 请求 URL=%s form=%s", apiURL, form.Encode())

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

	log.Printf("[易支付] 响应 body=%s", string(body))

	var result YipayResp
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析响应失败: %s", string(body))
	}
	if result.Code != 0 {
		errMsg := result.Msg
		if errMsg == "" {
			errMsg = "未知错误"
		}
		return "", fmt.Errorf("易支付返回错误: %s", errMsg)
	}

	if result.PayInfo != "" {
		return result.PayInfo, nil
	}
	return "", fmt.Errorf("易支付未返回支付信息")
}

// yipayVerifyCallback 验证易支付回调签名（MD5 或 RSA 自适应）
func yipayVerifyCallback(params map[string]string, key string, pubKeyPEM string) bool {
	sign, ok := params["sign"]
	if !ok || sign == "" {
		return false
	}

	st := params["sign_type"]
	delete(params, "sign")
	delete(params, "sign_type")

	if st == "RSA" {
		if pubKeyPEM == "" {
			log.Printf("回调验签: RSA 签名但未配置平台公钥")
			// 先用 MD5 试一下
			got := yipaySign(params, key)
			return got == sign
		}
		return yipayRsaVerify(params, sign, pubKeyPEM)
	}

	// MD5 验签
	got := yipaySign(params, key)
	return got == sign
}
