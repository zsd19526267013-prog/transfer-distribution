package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------- 发卡地址 ----------

func cardIndex(c *gin.Context) {
	var products []CodeProduct
	db.Where("is_active = ?", true).Order("sort_order asc, id asc").Find(&products)
	c.HTML(http.StatusOK, "card.html", gin.H{
		"products": products,
	})
}

func cardBuy(c *gin.Context) {
	productId := c.PostForm("product_id")
	payType := c.PostForm("type")
	if payType == "" {
		payType = "alipay"
	}
	if !cfg.Yipay.Alipay && payType == "alipay" {
		payType = "wxpay"
	}
	if !cfg.Yipay.WechatPay && payType == "wxpay" {
		payType = "alipay"
	}

	var product CodeProduct
	if db.First(&product, productId).Error != nil || !product.IsActive {
		c.String(400, "商品不存在或已下架")
		return
	}
	if product.Msrp <= 0 {
		c.String(400, "该商品未设置建议零售价")
		return
	}

	// 检查库存
	var available int64
	db.Model(&CodeStock{}).Where("product_id = ? AND status = 'available'", product.Id).Count(&available)
	if available < 1 {
		c.String(400, "库存不足")
		return
	}

	// 扣库存：锁定 1 个码
	var stock CodeStock
	db.Where("product_id = ? AND status = 'available'", product.Id).
		Order("id asc").Limit(1).Find(&stock)
	if stock.Id == 0 {
		c.String(400, "库存不足")
		return
	}

	now := time.Now().Unix()
	outTradeNo := fmt.Sprintf("CC%d%d", now, stock.Id)

	db.Model(&stock).Update("status", "reserved")

	order := SellOrder{
		DistributorId: 0,
		ProductId:     product.Id,
		Amount:        product.Msrp,
		Status:        "pending",
		OutTradeNo:    outTradeNo,
		CreatedAt:     now,
	}
	if err := db.Create(&order).Error; err != nil {
		db.Model(&stock).Update("status", "available")
		c.String(500, "创建订单失败")
		return
	}

	// 关联 stock 到订单
	db.Model(&stock).Updates(map[string]any{
		"order_id": order.Id,
	})

	money := fmt.Sprintf("%.2f", float64(product.Msrp)/100)
	notifyURL := cfg.BaseURL + "/yipay/notify"
	returnURL := cfg.BaseURL + fmt.Sprintf("/card/success/%d", order.Id)

	if cfg.Yipay.ApiURL == "" {
		db.Model(&stock).Updates(map[string]any{
			"status":   "available",
			"order_id": 0,
		})
		db.Delete(&order)
		c.String(500, "支付系统异常: 易支付未配置，请先联系管理员设置支付参数")
		return
	}
	payURL, err := yipayCreateOrder(
		cfg.Yipay.ApiURL,
		cfg.Yipay.MerchantID,
		cfg.Yipay.SecretKey,
		outTradeNo,
		payType,
		product.Name,
		money,
		notifyURL,
		returnURL,
		c.ClientIP(),
	)
	if err != nil {
		db.Model(&stock).Updates(map[string]any{
			"status":   "available",
			"order_id": 0,
		})
		db.Delete(&order)
		c.String(500, "支付系统异常: %v", err)
		return
	}

	c.Redirect(http.StatusFound, payURL)
}

func cardSuccess(c *gin.Context) {
	id := c.Param("id")
	var order SellOrder
	if db.First(&order, id).Error != nil {
		c.String(404, "订单不存在")
		return
	}

	var product CodeProduct
	db.First(&product, order.ProductId)

	var stock CodeStock
	db.Where("order_id = ?", order.Id).First(&stock)

	codeKey := ""
	if order.Status == "paid" {
		codeKey = stock.CodeKey
	}

	c.HTML(http.StatusOK, "card.html", gin.H{
		"order":   order,
		"product": product,
		"codeKey": codeKey,
	})
}

func cardStatus(c *gin.Context) {
	id := c.Param("id")
	var order SellOrder
	if db.First(&order, id).Error != nil {
		c.JSON(http.StatusOK, gin.H{"status": "not_found"})
		return
	}

	codeKey := ""
	if order.Status == "paid" {
		var stock CodeStock
		db.Where("order_id = ?", order.Id).First(&stock)
		codeKey = stock.CodeKey
	}

	c.JSON(http.StatusOK, gin.H{
		"status":   order.Status,
		"code_key": codeKey,
	})
}
