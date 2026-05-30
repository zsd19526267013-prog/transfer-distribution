package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------- 分销商：购买兑换码 ----------

func distShop(c *gin.Context) {
	key := c.Param("key")
	var d Distributor
	if db.Where("\"key\" = ?", key).First(&d).Error != nil {
		c.String(404, "无效链接")
		return
	}

	var products []CodeProduct
	db.Where("is_active = ?", true).Order("sort_order asc, id asc").Find(&products)

	c.HTML(http.StatusOK, "dist_shop.html", gin.H{
		"dist":     d,
		"products": products,
	})
}

func distBuyCreate(c *gin.Context) {
	key := c.Param("key")
	var d Distributor
	if db.Where("\"key\" = ?", key).First(&d).Error != nil {
		c.String(404, "无效链接")
		return
	}

	productId := c.PostForm("product_id")
	qtyStr := c.PostForm("qty")
	qty, _ := strconv.Atoi(qtyStr)
	if qty < 1 || qty > 100 {
		qty = 1
	}

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

	// 检查库存够不够
	var available int64
	db.Model(&CodeStock{}).Where("product_id = ? AND status = 'available'", product.Id).Count(&available)
	if available < int64(qty) {
		c.String(400, "库存不足，当前可用 %d 个，需要 %d 个", available, qty)
		return
	}

	// 扣库存：锁定 N 个码
	var stocks []CodeStock
	db.Where("product_id = ? AND status = 'available'", product.Id).
		Order("id asc").Limit(qty).Find(&stocks)

	totalAmount := product.Price * qty
	now := time.Now().Unix()
	outTradeNo := fmt.Sprintf("DS%d%d", now, d.Id)

	// 批量预留 stock
	for i := range stocks {
		db.Model(&stocks[i]).Update("status", "reserved")
	}

	order := SellOrder{
		DistributorId: d.Id,
		ProductId:     product.Id,
		Amount:        totalAmount,
		Status:        "pending",
		OutTradeNo:    outTradeNo,
		CodeKey:       fmt.Sprintf("batch:%d", qty),
		CreatedAt:     now,
	}
	if err := db.Create(&order).Error; err != nil {
		// 释放库存
		for i := range stocks {
			db.Model(&stocks[i]).Update("status", "available")
		}
		c.String(500, "创建订单失败")
		return
	}

	// 记录每个码对应的订单和分销商（用 order_id 标记预留，稍后回调完成时划转）
	for i := range stocks {
		db.Model(&stocks[i]).Updates(map[string]any{
			"order_id": order.Id,
		})
	}

	// 产品名称加上数量
	productName := fmt.Sprintf("%s x%d", product.Name, qty)
	money := fmt.Sprintf("%.2f", float64(totalAmount)/100)
	notifyURL := cfg.BaseURL + "/yipay/notify"
	returnURL := cfg.BaseURL + fmt.Sprintf("/dist/%s/shop/success/%d", key, order.Id)

	if cfg.Yipay.ApiURL == "" {
		for i := range stocks {
			db.Model(&stocks[i]).Updates(map[string]any{
				"status":   "available",
				"order_id": 0,
			})
		}
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
		productName,
		money,
		notifyURL,
		returnURL,
		c.ClientIP(),
	)
if err != nil {
		// 支付失败，释放库存
		for i := range stocks {
			db.Model(&stocks[i]).Updates(map[string]any{
				"status":   "available",
				"order_id": 0,
			})
		}
		db.Delete(&order)
		c.String(500, "支付系统异常: %v", err)
		return
	}

	c.Redirect(http.StatusFound, payURL)
}

func distBuySuccess(c *gin.Context) {
	key := c.Param("key")
	id := c.Param("id")

	var d Distributor
	if db.Where("\"key\" = ?", key).First(&d).Error != nil {
		c.String(404, "无效链接")
		return
	}

	var order SellOrder
	if db.First(&order, id).Error != nil {
		c.String(404, "订单不存在")
		return
	}

	var product CodeProduct
	db.First(&product, order.ProductId)

	// 检查有多少码已成功划转到该分销商名下
	var transferred int64
	db.Model(&CodeStock{}).Where("order_id = ? AND status = ?", order.Id, "sold").Count(&transferred)

	c.HTML(http.StatusOK, "dist_success.html", gin.H{
		"dist":        d,
		"order":       order,
		"product":     product,
		"transferred": transferred,
	})
}

// ---------- 易支付回调（改造：成功后划码到分销商）----------

func yipayNotify(c *gin.Context) {
	params := make(map[string]string)
	c.Request.ParseForm()
	for k, v := range c.Request.Form {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}

	if !yipayVerifyCallback(params, cfg.Yipay.SecretKey) {
		log.Printf("易支付回调验签失败: %v", params)
		c.String(200, "fail")
		return
	}

	tradeStatus := params["trade_status"]
	if tradeStatus != "TRADE_SUCCESS" && tradeStatus != "success" && params["status"] != "1" {
		log.Printf("易支付回调状态非成功: %v", params)
		c.String(200, "ok")
		return
	}

	outTradeNo := params["out_trade_no"]
	tradeNo := params["trade_no"]

	var order SellOrder
	if err := db.Where("out_trade_no = ?", outTradeNo).First(&order).Error; err != nil {
		log.Printf("订单不存在: %s", outTradeNo)
		c.String(200, "ok")
		return
	}

	if order.Status == "paid" {
		c.String(200, "ok")
		return
	}

	now := time.Now().Unix()
	db.Model(&order).Updates(map[string]any{
		"status":   "paid",
		"trade_no": tradeNo,
		"paid_at":  now,
	})

	// 查找这个订单预留的所有码
	var stocks []CodeStock
	db.Where("order_id = ?", order.Id).Find(&stocks)

	if len(stocks) == 0 {
		log.Printf("订单 %d 没有关联的码库存", order.Id)
		c.String(200, "ok")
		return
	}

	// 标记库存为已售
	nowTs := time.Now().Unix()
	for _, s := range stocks {
		db.Model(&s).Update("status", "sold")
	}

	distId := order.DistributorId
	if distId > 0 {
		// 分销商订单：把码分配给分销商
		for _, s := range stocks {
			err := db.Create(&DistributorCode{
				DistributorId: distId,
				ProductId:     s.ProductId,
				CodeKey:       s.CodeKey,
				CreatedAt:     nowTs,
			}).Error
			if err != nil {
				log.Printf("分配码 %s 给分销商 %d 失败: %v", s.CodeKey, distId, err)
			}
		}
		log.Printf("订单 %s 支付成功，分配 %d 个码给分销商 %d", outTradeNo, len(stocks), distId)
	} else {
		// 消费者直购：不回写分销商记录，码已标记 sold，成功页展示
		log.Printf("直购订单 %s 支付成功，码已发放", outTradeNo)
	}

	c.String(200, "ok")
}

func distShopStatus(c *gin.Context) {
	id := c.Param("id")
	var order SellOrder
	if db.First(&order, id).Error != nil {
		c.JSON(http.StatusOK, gin.H{"status": "not_found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": order.Status,
	})
}

// ---------- 分销商：下载兑换码 ----------

func distDownloadCodes(c *gin.Context) {
	key := c.Param("key")
	var d Distributor
	if db.Where("\"key\" = ?", key).First(&d).Error != nil {
		c.String(404, "无效链接")
		return
	}

	productIdStr := c.Query("product_id")
	productId, _ := strconv.Atoi(productIdStr)

	query := db.Table("dist_codes dc").
		Select("dc.code_key").
		Joins("LEFT JOIN code_stock cs ON cs.code_key = dc.code_key").
		Joins("LEFT JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL").
		Where("dc.distributor_id = ?", d.Id).
		Where("r.id IS NULL OR r.status != 3")

	if productId > 0 {
		query = query.Where("COALESCE(dc.product_id, cs.product_id, 0) = ?", productId)
	}

	type CodeRow struct {
		CodeKey string
	}
	var codes []CodeRow
	query.Order("dc.id DESC").Find(&codes)

	var sb strings.Builder
	for _, c := range codes {
		sb.WriteString(c.CodeKey)
		sb.WriteString("\n")
	}

	filename := fmt.Sprintf("%s_兑换码.txt", d.Name)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.String(200, sb.String())
}
