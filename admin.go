package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------- 管理员：商品管理 ----------

func adminProducts(c *gin.Context) {
	var products []CodeProduct
	db.Order("sort_order asc, id asc").Find(&products)
	c.HTML(http.StatusOK, "admin_products.html", gin.H{"products": products})
}

func adminProductNew(c *gin.Context) {
	name := strings.TrimSpace(c.PostForm("name"))
	priceStr := strings.TrimSpace(c.PostForm("price"))
	msrpStr := strings.TrimSpace(c.PostForm("msrp"))
	sortStr := strings.TrimSpace(c.PostForm("sort_order"))

	if name == "" || priceStr == "" {
		c.String(400, "参数不完整")
		return
	}

	price, _ := strconv.Atoi(priceStr)
	msrp, _ := strconv.Atoi(msrpStr)
	sortOrder, _ := strconv.Atoi(sortStr)
	now := time.Now().Unix()

	db.Create(&CodeProduct{
		Name:      name,
		Price:     price,
		Msrp:      msrp,
		SortOrder: sortOrder,
		IsActive:  true,
		CreatedAt: now,
	})
	c.Redirect(302, "/products")
}

func adminProductToggle(c *gin.Context) {
	id := c.Param("id")
	var product CodeProduct
	if db.First(&product, id).Error != nil {
		c.String(404, "商品不存在")
		return
	}
	db.Model(&product).Update("is_active", !product.IsActive)
	c.Redirect(302, "/products")
}

func adminProductDelete(c *gin.Context) {
	id := c.Param("id")
	var count int64
	db.Model(&CodeStock{}).Where("product_id = ?", id).Count(&count)
	if count > 0 {
		db.Model(&CodeProduct{}).Where("id = ?", id).Update("is_active", false)
		c.Redirect(302, "/products")
		return
	}
	db.Delete(&CodeProduct{}, id)
	c.Redirect(302, "/products")
}

// ---------- 管理员：码库存 ----------

func adminStock(c *gin.Context) {
	type StockRow struct {
		Id        int
		Product   string
		CodeKey   string
		Status    string
		OrderId   int
		CreatedAt string
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize := 50

	var total int64
	db.Model(&CodeStock{}).Count(&total)

	var rows []struct {
		CodeStock
		ProductName string
	}
	db.Table("code_stock").
		Select("code_stock.*, code_products.name as product_name").
		Joins("LEFT JOIN code_products ON code_products.id = code_stock.product_id").
		Order("code_stock.id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Scan(&rows)

	var stockRows []StockRow
	for _, r := range rows {
		stockRows = append(stockRows, StockRow{
			Id:        r.Id,
			Product:   r.ProductName,
			CodeKey:   r.CodeKey,
			Status:    r.Status,
			OrderId:   r.OrderId,
			CreatedAt: time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04"),
		})
	}

	totalPages := int(total) / pageSize
	if int(total)%pageSize > 0 {
		totalPages++
	}

	var products []CodeProduct
	db.Where("is_active = ?", true).Find(&products)

	c.HTML(http.StatusOK, "admin_stock.html", gin.H{
		"rows":        stockRows,
		"products":    products,
		"page":        page,
		"totalPages":  totalPages,
		"total":       total,
	})
}

// ---------- 管理员：导入兑换码 ----------

func adminStockImport(c *gin.Context) {
	productIdStr := c.PostForm("product_id")
	codesStr := strings.TrimSpace(c.PostForm("codes"))

	pid, err := strconv.Atoi(productIdStr)
	if err != nil || pid <= 0 || codesStr == "" {
		c.Redirect(302, "/stock")
		return
	}

	lines := strings.Split(codesStr, "\n")
	now := time.Now().Unix()
	imported := 0
	for _, line := range lines {
		code := strings.TrimSpace(line)
		if code == "" {
			continue
		}
		// 每行支持用空格/逗号/制表符分隔出多个码
		for _, part := range strings.FieldsFunc(code, func(r rune) bool {
			return r == ' ' || r == ',' || r == '\t'
		}) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// 检查是否已存在
			var exist int64
			db.Model(&CodeStock{}).Where("code_key = ?", part).Count(&exist)
			if exist > 0 {
				continue
			}
			db.Create(&CodeStock{
				ProductId: pid,
				CodeKey:   part,
				Status:    "available",
				CreatedAt: now,
			})
			imported++
		}
	}
	c.Redirect(302, "/stock")
}

// ---------- 管理员：订单管理 ----------

func adminOrders(c *gin.Context) {
	type OrderRow struct {
		Id        int
		Product   string
		Amount    string
		Status    string
		TradeNo   string
		OutTrade  string
		CodeKey   string
		CreatedAt string
		PaidAt    string
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize := 50

	var total int64
	db.Model(&SellOrder{}).Count(&total)

	var rows []struct {
		SellOrder
		ProductName string
	}
	db.Table("sell_orders").
		Select("sell_orders.*, code_products.name as product_name").
		Joins("LEFT JOIN code_products ON code_products.id = sell_orders.product_id").
		Order("sell_orders.id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Scan(&rows)

	var orderRows []OrderRow
	for _, r := range rows {
		orderRows = append(orderRows, OrderRow{
			Id:        r.Id,
			Product:   r.ProductName,
			Amount:    fmt.Sprintf("%.2f", float64(r.Amount)/100),
			Status:    orderStatusLabel(r.Status),
			TradeNo:   r.TradeNo,
			OutTrade:  r.OutTradeNo,
			CodeKey:   r.CodeKey,
			CreatedAt: time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04"),
			PaidAt:    tsOrDashInt(r.PaidAt),
		})
	}

	totalPages := int(total) / pageSize
	if int(total)%pageSize > 0 {
		totalPages++
	}

	c.HTML(http.StatusOK, "admin_orders.html", gin.H{
		"rows":       orderRows,
		"page":       page,
		"totalPages": totalPages,
		"total":      total,
	})
}

// ---------- 管理员：易支付设置 ----------

func adminYipay(c *gin.Context) {
	if c.Request.Method == "POST" {
		cfg.Yipay.ApiURL = strings.TrimRight(strings.TrimSpace(c.PostForm("api_url")), "/")
		cfg.Yipay.MerchantID = strings.TrimSpace(c.PostForm("merchant_id"))
		cfg.Yipay.SecretKey = strings.TrimSpace(c.PostForm("secret_key"))
			cfg.Yipay.PubKey = strings.TrimSpace(c.PostForm("pub_key"))
		cfg.Yipay.Alipay = c.PostForm("alipay") == "1"
		cfg.Yipay.WechatPay = c.PostForm("wechatpay") == "1"
		// 写回配置文件
		writeConfig(cfg)
		c.Redirect(302, "/yipay")
		return
	}
	c.HTML(http.StatusOK, "admin_yipay.html", gin.H{"cfg": cfg.Yipay})
}

// ---------- 工具函数 ----------

func orderStatusLabel(s string) string {
	switch s {
	case "pending":
		return "待支付"
	case "paid":
		return "已支付"
	case "expired":
		return "已过期"
	default:
		return s
	}
}

func tsOrDashInt(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// 用于在模板中安全显示兑换码
func formatCodeKey(s string) string {
	return template.HTMLEscapeString(s)
}
