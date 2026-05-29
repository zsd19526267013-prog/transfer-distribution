package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

//go:embed templates/*
var tmplFS embed.FS

var db *gorm.DB
var cfg Config
var baseURL string

type YipayConfig struct {
	ApiURL     string `json:"api_url"`
	MerchantID string `json:"merchant_id"`
	SecretKey  string `json:"secret_key"`
	Alipay     bool   `json:"alipay"`
	WechatPay  bool   `json:"wechatpay"`
}

type Config struct {
	DatabaseDSN string      `json:"database_dsn"`
	BaseURL     string      `json:"base_url"`
	Port        int         `json:"port"`
	AdminUser   string      `json:"admin_user"`
	AdminPass   string      `json:"admin_pass"`
	Yipay       YipayConfig `json:"yipay"`
}

var authSecret = func() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}()

// ---------- 登录限流 ----------

type loginRecord struct {
	count       int
	failures    int
	windowStart time.Time
	bannedUntil time.Time
}

var (
	loginMu    sync.Mutex
	loginLimit = 10                       // 每分钟最多请求数
	banAfter   = 20                       // 连续失败上限
	banDur     = 30 * time.Minute         // IP 封禁时长
	records    = make(map[string]*loginRecord)
)

func isBanned(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	r, ok := records[ip]
	if !ok {
		return false
	}
	if time.Now().Before(r.bannedUntil) {
		return true
	}
	if !r.bannedUntil.IsZero() {
		delete(records, ip)
	}
	return false
}

func checkRateLimit(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	r, ok := records[ip]
	if !ok {
		records[ip] = &loginRecord{count: 1, windowStart: now}
		return true
	}
	if now.Sub(r.windowStart) > time.Minute {
		r.count = 1
		r.windowStart = now
		return true
	}
	r.count++
	return r.count <= loginLimit
}

func recordFailure(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	r, ok := records[ip]
	if !ok {
		records[ip] = &loginRecord{failures: 1, windowStart: now, count: 1}
		return
	}
	r.failures++
	if r.failures >= banAfter {
		r.bannedUntil = now.Add(banDur)
		log.Printf("IP %s 因连续 %d 次登录失败被封禁 %v", ip, banAfter, banDur)
	}
}

func recordSuccess(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	delete(records, ip)
}

func generateSessionToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func signToken(t string) string {
	mac := hmac.New(sha256.New, authSecret)
	mac.Write([]byte(t))
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyToken(t string, sig string) bool {
	return hmac.Equal([]byte(sig), []byte(signToken(t)))
}

func authRequired(c *gin.Context) {
	cookie, err := c.Cookie("session")
	if err != nil || cookie == "" {
		c.Redirect(302, "/login")
		c.Abort()
		return
	}
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		c.Redirect(302, "/login")
		c.Abort()
		return
	}
	if !verifyToken(parts[0], parts[1]) {
		c.Redirect(302, "/login")
		c.Abort()
		return
	}
	var exp int64
	fmt.Sscanf(parts[0], "%d", &exp)
	if time.Now().Unix() > exp {
		c.Redirect(302, "/login")
		c.Abort()
		return
	}
	c.Next()
}

func loadConfig() Config {
	var cfg Config
	data, err := os.ReadFile("config.json")
	if err == nil {
		json.Unmarshal(data, &cfg)
	}
	if cfg.DatabaseDSN == "" {
		cfg.DatabaseDSN = os.Getenv("SQL_DSN")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv("BASE_URL")
		if cfg.BaseURL == "" {
			cfg.BaseURL = "http://localhost:8081"
		}
	}
	if cfg.Port == 0 {
		p := os.Getenv("PORT")
		if p == "" {
			cfg.Port = 8081
		} else {
			fmt.Sscanf(p, "%d", &cfg.Port)
		}
	}
	return cfg
}

func writeConfig(c Config) {
	data, _ := json.MarshalIndent(c, "", "\t")
	os.WriteFile("config.json", data, 0644)
}

// ---------- 分销系统自己的表 ----------

type Distributor struct {
	Id        int    `gorm:"primaryKey"`
	Key       string `gorm:"type:char(32);uniqueIndex;not null"`
	Name      string `gorm:"type:varchar(100);not null"`
	Remark    string `gorm:"type:text"`
	CreatedAt int64  `gorm:"autoCreateTime"`
	SettledAt int64  `gorm:"default:0;comment:0=未结算"`
}

func (Distributor) TableName() string { return "dist_distributors" }

type DistributorCode struct {
	Id            int    `gorm:"primaryKey"`
	DistributorId int    `gorm:"index;not null"`
	CodeKey       string `gorm:"type:char(32);uniqueIndex;not null"`
	CreatedAt     int64  `gorm:"autoCreateTime"`
}

func (DistributorCode) TableName() string { return "dist_codes" }

// ---------- 主库只读模型 ----------

type Redemption struct {
	Key          string `gorm:"column:key"`
	Status       int    `gorm:"column:status"`
	UsedUserId   int    `gorm:"column:used_user_id"`
	RedeemedTime int64  `gorm:"column:redeemed_time"`
}

func (Redemption) TableName() string { return "redemptions" }

type User struct {
	Id        int    `gorm:"column:id"`
	Username  string `gorm:"column:username"`
	CreatedAt int64  `gorm:"column:created_at"`
}

func (User) TableName() string { return "users" }

type Log struct {
	UserId int `gorm:"column:user_id"`
	Type   int `gorm:"column:type"`
	Quota  int `gorm:"column:quota"`
}

func (Log) TableName() string { return "logs" }

// ---------- 发码平台表 ----------

type CodeProduct struct {
	Id        int    `gorm:"primaryKey"`
	Name      string `gorm:"type:varchar(50);not null"`
	Price     int    `gorm:"not null;comment:售价(分)"`
	Msrp      int    `gorm:"default:0;comment:建议零售价(分)"`
	SortOrder int    `gorm:"default:0"`
	IsActive  bool   `gorm:"default:true"`
	CreatedAt int64  `gorm:"autoCreateTime"`
}

func (CodeProduct) TableName() string { return "code_products" }

type CodeStock struct {
	Id        int    `gorm:"primaryKey"`
	ProductId int    `gorm:"index;not null"`
	CodeKey   string `gorm:"type:char(32);uniqueIndex;not null"`
	Status    string `gorm:"type:varchar(20);default:'available';comment:available/reserved/sold"`
	OrderId   int    `gorm:"default:0"`
	CreatedAt int64  `gorm:"autoCreateTime"`
}

func (CodeStock) TableName() string { return "code_stock" }

type SellOrder struct {
	Id            int    `gorm:"primaryKey"`
	DistributorId int    `gorm:"default:0;index;comment:分销商ID"`
	ProductId     int    `gorm:"index;not null"`
	Amount        int    `gorm:"not null;comment:实付(分)"`
	Status        string `gorm:"type:varchar(20);default:'pending';comment:pending/paid/expired/refunded"`
	OutTradeNo    string `gorm:"type:varchar(64);uniqueIndex;not null"`
	TradeNo       string `gorm:"type:varchar(100);comment:易支付交易号"`
	CodeKey       string `gorm:"type:varchar(64);comment:分配的兑换码"`
	PaidAt        int64  `gorm:"default:0"`
	CreatedAt     int64  `gorm:"autoCreateTime"`
}

func (SellOrder) TableName() string { return "sell_orders" }

// ---------- 页面数据 ----------

type DashboardRow struct {
	Id            int
	Name          string
	Key           string
	Remark        string
	CreatedAt     string
	SettledAt     string
	TotalCodes    int
	RedeemedCodes int
	UniqueUsers   int
	TotalConsume  int64
	Commission    int64
	Rate          string
}

type UserConsumption struct {
	Id               int
	Username         string
	CreatedAt        string
	TotalConsumption int64
	Commission       int64
}

type CodeDetail struct {
	Id           int
	CodeKey      string
	ImportedAt   string
	StatusText   string
	UsedUserId   int
	UsedUsername string
	RedeemedTime string
}

// ---------- main ----------

func main() {
	cfg = loadConfig()

	dsn := cfg.DatabaseDSN
	if dsn == "" {
		log.Fatalln("config.json 未设置 database_dsn")
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		log.Fatalln("database_dsn 必须是 postgres:// 开头")
	}

	baseURL = cfg.BaseURL

	var err error
	db, err = gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}), &gorm.Config{PrepareStmt: true})
	if err != nil {
		log.Fatalln("数据库连接失败:", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	db.AutoMigrate(&Distributor{}, &DistributorCode{}, &CodeProduct{}, &CodeStock{}, &SellOrder{})

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		"date": func(ts int64) string {
			if ts == 0 {
				return "-"
			}
			return time.Unix(ts, 0).Format("2006-01-02 15:04")
		},
		"percent": func(a, b int64) string {
			if b == 0 {
				return "0.0"
			}
			return fmt.Sprintf("%.1f", float64(a)/float64(b)*100)
		},
		"money": func(price int) string {
			return fmt.Sprintf("%.2f", float64(price)/100)
		},
		"human": func(n interface{}) string {
			var v float64
			switch x := n.(type) {
			case int64:
				v = float64(x)
			case int:
				v = float64(x)
			case float64:
				v = x
			default:
				return fmt.Sprint(n)
			}
			if v >= 10000 {
				return fmt.Sprintf("%.1fw", v/10000)
			}
			return fmt.Sprint(int64(v))
		},
		"formatCodeKey": func(s string) string {
			return template.HTMLEscapeString(s)
		},
		"orderStatus": orderStatusLabel,
		"sub": func(a, b int) int { return a - b },
		"add": func(a, b int) int { return a + b },
	}).ParseFS(tmplFS, "templates/*.html"))

	r := gin.Default()
	r.SetHTMLTemplate(tmpl)

	// ---- 登录 ----

	r.GET("/login", func(c *gin.Context) {
		c.HTML(http.StatusOK, "login.html", nil)
	})
	r.POST("/login", func(c *gin.Context) {
		ip := c.ClientIP()

		if isBanned(ip) {
			c.HTML(http.StatusOK, "login.html", gin.H{"err": "登录失败次数过多，IP 已被临时封禁，请 30 分钟后再试"})
			return
		}
		if !checkRateLimit(ip) {
			c.HTML(http.StatusOK, "login.html", gin.H{"err": "请求过于频繁，请稍后再试"})
			return
		}

		user := c.PostForm("user")
		pass := c.PostForm("pass")
		if user != cfg.AdminUser || pass != cfg.AdminPass {
			recordFailure(ip)
			c.HTML(http.StatusOK, "login.html", gin.H{"err": "用户名或密码错误"})
			return
		}
		recordSuccess(ip)
		exp := time.Now().Add(24 * time.Hour).Unix()
		payload := fmt.Sprintf("%d", exp)
		token := payload + "." + signToken(payload)
		c.SetCookie("session", token, 86400, "/", "", false, true)
		c.Redirect(302, "/")
	})
	r.POST("/logout", func(c *gin.Context) {
		c.SetCookie("session", "", -1, "/", "", false, true)
		c.Redirect(302, "/login")
	})

	// QR 码代理（支持下载）
	r.GET("/qr", func(c *gin.Context) {
		data := c.Query("data")
		if data == "" {
			c.String(400, "missing data")
			return
		}
		qrURL := fmt.Sprintf("https://api.qrserver.com/v1/create-qr-code/?size=300x300&data=%s", url.QueryEscape(data))
		resp, err := http.Get(qrURL)
		if err != nil {
			c.String(500, "qr fetch failed")
			return
		}
		defer resp.Body.Close()
		c.Header("Content-Type", "image/png")
		c.Header("Content-Disposition", "attachment; filename=qrcode.png")
		io.Copy(c.Writer, resp.Body)
	})

	// ---- 分销商购买兑换码 ----
	// 注意：必须在 /dist/:key 之前注册，否则会被 :key 吃掉
	r.GET("/dist/:key/shop", distShop)
	r.POST("/dist/:key/shop/buy", distBuyCreate)
	r.GET("/dist/:key/shop/status/:id", distShopStatus)
	r.GET("/dist/:key/shop/success/:id", distBuySuccess)

	// ---- 消费者直购 ----
	r.GET("/buy", directBuyIndex)
	r.POST("/buy/create", directBuyCreate)
	r.GET("/buy/success/:id", directBuySuccess)
	r.GET("/buy/status/:id", directBuyStatus)
	r.GET("/buy/query", directBuyQuery)
	r.POST("/buy/query", directBuyQueryLookup)

	// ---- 易支付异步回调 ----
	r.POST("/yipay/notify", yipayNotify)
	r.GET("/yipay/notify", yipayNotify)

	// ---- 管理员路由（需登录）----

	admin := r.Group("/", authRequired)

	// 首页看板
	admin.GET("/", func(c *gin.Context) {
		rows := dashboardData()
		c.HTML(http.StatusOK, "index.html", gin.H{
			"rows":    rows,
			"baseURL": baseURL,
		})
	})

	// 新建分销员
	admin.POST("/distributor/new", func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("name"))
		if name == "" {
			c.String(400, "名称不能为空")
			return
		}
		key := generateKey()
		remark := strings.TrimSpace(c.PostForm("remark"))
		db.Create(&Distributor{Key: key, Name: name, Remark: remark})
		c.Redirect(302, "/")
	})

	// 管理员：分销员详情（含二维码和结算操作）
	admin.GET("/distributor/:id", func(c *gin.Context) {
		id := c.Param("id")
		var d Distributor
		if db.First(&d, id).Error != nil {
			c.String(404, "分销员不存在")
			return
		}
		codes, totalCodes, redeemedCodes, uniqueUsers, consumptions := loadDistributorData(d.Id)
		c.HTML(http.StatusOK, "detail.html", gin.H{
			"d":             d,
			"baseURL":       baseURL,
			"totalCodes":    totalCodes,
			"redeemedCodes": redeemedCodes,
			"uniqueUsers":   uniqueUsers,
			"codes":         codes,
			"consumptions":  consumptions,
		})
	})

	// 结算
	admin.POST("/distributor/:id/settle", func(c *gin.Context) {
		id := c.Param("id")
		db.Model(&Distributor{}).Where("id = ?", id).Update("settled_at", time.Now().Unix())
		db.Where("distributor_id = ?", id).Delete(&DistributorCode{})
		c.Redirect(302, "/")
	})

	// 删除
	admin.POST("/distributor/:id/delete", func(c *gin.Context) {
		id := c.Param("id")
		db.Where("distributor_id = ?", id).Delete(&DistributorCode{})
		db.Delete(&Distributor{}, id)
		c.Redirect(302, "/")
	})

	// ---- 发码平台管理 ----
	admin.GET("/products", adminProducts)
	admin.POST("/products/new", adminProductNew)
	admin.POST("/products/:id/toggle", adminProductToggle)
	admin.POST("/products/:id/delete", adminProductDelete)

	admin.GET("/stock", adminStock)

	admin.GET("/orders", adminOrders)

	admin.Any("/yipay", adminYipay)

	// ---- 分销商自查看路由（无需登录，凭 Key）----
	r.GET("/dist/:key", func(c *gin.Context) {
		key := c.Param("key")
		var d Distributor
		if db.Where("\"key\" = ?", key).First(&d).Error != nil {
			c.String(404, "无效链接")
			return
		}
		codes, totalCodes, redeemedCodes, uniqueUsers, consumptions := loadDistributorData(d.Id)
		var totalCommission int64
		for _, u := range consumptions {
			totalCommission += u.Commission
		}
		c.HTML(http.StatusOK, "distributor_view.html", gin.H{
			"d":               d,
			"totalCodes":      totalCodes,
			"redeemedCodes":   redeemedCodes,
			"uniqueUsers":     uniqueUsers,
			"codes":           codes,
			"consumptions":    consumptions,
			"totalCommission": totalCommission,
		})
	})

	log.Printf("启动分销管理系统 %s", baseURL)
	r.Run(fmt.Sprintf(":%d", cfg.Port))
}

// ---------- 数据加载 ----------

func loadDistributorData(distId int) ([]CodeDetail, int64, int64, int64, []UserConsumption) {
	var codes []struct {
		Id           int
		CodeKey      string
		ImportedAt   int64
		Status       int
		UsedUserId   int
		UsedUsername string
		RedeemedTime int64
	}
	db.Raw(`
		SELECT dc.id, dc.code_key, dc.created_at as imported_at,
			COALESCE(r.status,0) as status,
			COALESCE(r.used_user_id,0) as used_user_id,
			COALESCE(u.username,'') as used_username,
			COALESCE(r.redeemed_time,0) as redeemed_time
		FROM dist_codes dc
		LEFT JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL
		LEFT JOIN users u ON u.id = r.used_user_id AND u.deleted_at IS NULL
		WHERE dc.distributor_id = ?
		ORDER BY dc.id DESC
	`, distId).Scan(&codes)

	var codeDetails []CodeDetail
	for _, c := range codes {
		codeDetails = append(codeDetails, CodeDetail{
			Id: c.Id, CodeKey: c.CodeKey,
			ImportedAt:   time.Unix(c.ImportedAt, 0).Format("2006-01-02 15:04"),
			StatusText:   statusLabel(c.Status),
			UsedUserId:   c.UsedUserId,
			UsedUsername: c.UsedUsername,
			RedeemedTime: tsOrDash(c.RedeemedTime),
		})
	}

	var totalCodes, redeemedCodes, uniqueUsers int64
	db.Model(&DistributorCode{}).Where("distributor_id = ?", distId).Count(&totalCodes)
	db.Raw(`SELECT COUNT(*) FROM dist_codes dc JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL WHERE dc.distributor_id = ? AND r.status = 3`, distId).Scan(&redeemedCodes)
	db.Raw(`SELECT COUNT(DISTINCT r.used_user_id) FROM dist_codes dc JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL WHERE dc.distributor_id = ? AND r.status = 3 AND r.used_user_id > 0`, distId).Scan(&uniqueUsers)

	var consumptions []UserConsumption
	db.Raw(`
		WITH code_users AS (
			SELECT DISTINCT r.used_user_id FROM dist_codes dc
			JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL
			WHERE dc.distributor_id = ? AND r.used_user_id > 0
		)
		SELECT u.id, u.username, u.created_at,
			COALESCE(SUM(l.quota),0) as total_consumption
		FROM code_users cu
		JOIN users u ON u.id = cu.used_user_id AND u.deleted_at IS NULL
		LEFT JOIN logs l ON l.user_id = cu.used_user_id AND l.type IN (1, 2)
		GROUP BY u.id, u.username, u.created_at
		ORDER BY total_consumption DESC
	`, distId).Scan(&consumptions)

	for i := range consumptions {
		consumptions[i].CreatedAt = tsOrDash(consumptions[i].CreatedAt)
		consumptions[i].Commission = int64(float64(consumptions[i].TotalConsumption) * 0.2)
	}

	return codeDetails, totalCodes, redeemedCodes, uniqueUsers, consumptions
}

func dashboardData() []DashboardRow {
	var rows []DashboardRow
	db.Raw(`
		SELECT d.id, d.name, d.key, d.remark, d.created_at, d.settled_at,
			COUNT(dc.id) as total_codes,
			COUNT(r.id) FILTER (WHERE r.status = 3) as redeemed_codes,
			COUNT(DISTINCT r.used_user_id) FILTER (WHERE r.status = 3 AND r.used_user_id > 0) as unique_users
		FROM dist_distributors d
		LEFT JOIN dist_codes dc ON dc.distributor_id = d.id
		LEFT JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL
		GROUP BY d.id, d.name, d.key, d.remark, d.created_at, d.settled_at
		ORDER BY d.id DESC
	`).Scan(&rows)

	for i := range rows {
		rows[i].CreatedAt = tsOrDash(rows[i].CreatedAt)
		rows[i].SettledAt = tsOrDash(rows[i].SettledAt)
		if rows[i].TotalCodes > 0 {
			rows[i].Rate = fmt.Sprintf("%.1f%%", float64(rows[i].RedeemedCodes)/float64(rows[i].TotalCodes)*100)
		} else {
			rows[i].Rate = "-"
		}
		var total int64
		db.Raw(`
			SELECT COALESCE(SUM(l.quota),0) FROM dist_codes dc
			JOIN redemptions r ON r.key = dc.code_key AND r.deleted_at IS NULL
			JOIN logs l ON l.user_id = r.used_user_id AND l.type IN (1, 2)
			WHERE dc.distributor_id = ? AND r.status = 3
		`, rows[i].Id).Scan(&total)
		rows[i].TotalConsume = total
		rows[i].Commission = int64(float64(total) * 0.2)
	}
	return rows
}

// ---------- 工具函数 ----------

func generateKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func tsOrDash(ts any) string {
	switch v := ts.(type) {
	case int64:
		if v == 0 {
			return "-"
		}
		return time.Unix(v, 0).Format("2006-01-02 15:04")
	case string:
		if v == "" || v == "0" {
			return "-"
		}
		return v
	}
	return "-"
}

func statusLabel(status int) string {
	switch status {
	case 0, 1:
		return "未使用"
	case 2:
		return "已禁用"
	case 3:
		return "已核销"
	default:
		return "未知"
	}
}
