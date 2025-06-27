package main

import (
    "database/sql"
    _ "github.com/mattn/go-sqlite3"
    "fmt"
    "log"
    "strconv"
    "strings"
    "time"
    "math/rand"
    "net/http"
    "net/url"
    "encoding/json"
	"io"
    "github.com/google/uuid"
    "bytes"
    "os"
    "github.com/joho/godotenv"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
)

var db *sql.DB

// متغیرهای اصلی از env
var (
    adminIDs   []int64
    botToken   string
    cardNumber string
    cardHolder string
)

// ساختارهای داده برای مدیریت وضعیت کاربران
type UserState struct {
    WaitingForAmount  bool
    WaitingForReceipt int
    PendingAmount     int
}

type TelegramBot struct {
    bot           *tgbotapi.BotAPI
    balances      map[int64]int
    users         map[int64]string
    userStates    map[int64]*UserState
    processedReceipts map[string]bool // نگهداری فیش‌های پردازش شده
}

// وضعیت مرحله‌ای شارژ دستی
var adminManualChargeState = struct {
    Step int
    TargetUserID int64
}{Step: 0, TargetUserID: 0}

// وضعیت اطلاع‌رسانی همگانی
var adminBroadcastState = struct {
    Waiting bool
}{Waiting: false}

// وضعیت اطلاعات کاربر
var adminUserInfoState = struct {
    Waiting bool
}{Waiting: false}

// --- Panel Config Struct ---
type PanelConfig struct {
    ID           int
    PanelURL     string
    Cookie       string
    ConfigMiddle string
    PanelID      string
    UserLimit    int
    UsedCount    int
}

// وضعیت افزودن پنل جدید
var adminAddPanelState = struct {
    Step int
    TempPanel PanelConfig
}{Step: 0}

// --- Admin add service state ---
var adminAddServiceState = struct {
    Step int
    TempDesc string
    TempDays int
    TempGB int
    TempPrice int
    TempName string
}{Step: 0}

// --- Usage info struct ---
type UsageInfo struct {
    Email      string
    Up         int64
    Down       int64
    Total      int64
    ExpiryTime int64
}

// ساختار فیش در انتظار بررسی
type PendingReceipt struct {
    ID      string
    UserID  int64
    Amount  int
    PhotoID string
}

// نگهداری فیش‌های در انتظار بررسی
var pendingReceipts = make(map[string]PendingReceipt)

// راه‌اندازی دیتابیس و ساخت جدول شارژها و سرویس‌ها
func initDB() {
    var err error
    db, err = sql.Open("sqlite3", "botdata.db")
    if err != nil {
        log.Fatalf("خطا در باز کردن دیتابیس: %v", err)
    }
    createCharges := `CREATE TABLE IF NOT EXISTS charges (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER,
        amount INTEGER,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );`
    _, err = db.Exec(createCharges)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول شارژها: %v", err)
    }
    createServices := `CREATE TABLE IF NOT EXISTS services (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL UNIQUE,
        description TEXT,
        price INTEGER,
        days INTEGER
    );`
    _, err = db.Exec(createServices)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول سرویس‌ها: %v", err)
    }
    // مهاجرت: افزودن ستون‌های جدید اگر وجود ندارند
    db.Exec("ALTER TABLE services ADD COLUMN description TEXT;")
    db.Exec("ALTER TABLE services ADD COLUMN price INTEGER;")
    db.Exec("ALTER TABLE services ADD COLUMN days INTEGER;")
    db.Exec("ALTER TABLE services ADD COLUMN gb INTEGER;")
    createUserConfigs := `CREATE TABLE IF NOT EXISTS user_configs (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER,
        service_id INTEGER,
        email TEXT,
        sub_id TEXT,
        config_link TEXT,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );`
    _, err = db.Exec(createUserConfigs)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول کانفیگ‌های کاربران: %v", err)
    }
    createPanels := `CREATE TABLE IF NOT EXISTS panels (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        panel_url TEXT,
        cookie TEXT,
        config_middle TEXT,
        panel_id TEXT,
        user_limit INTEGER DEFAULT 0,
        used_count INTEGER DEFAULT 0
    );`
    _, err = db.Exec(createPanels)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول پنل‌ها: %v", err)
    }
}

// ارسال فایل دیتابیس به ادمین
func sendDBBackupToAdmin(bot *tgbotapi.BotAPI, adminID int64) {
    doc := tgbotapi.NewDocumentUpload(adminID, "botdata.db")
    doc.Caption = "📦 بکاپ هفتگی دیتابیس ربات"
    bot.Send(doc)
}

// ارسال بکاپ هفتگی به همه ادمین‌ها
func sendWeeklyBackupToAllAdmins(bot *tgbotapi.BotAPI) {
    // ارسال بکاپ به اولین ادمین
    if len(adminIDs) > 0 {
        doc := tgbotapi.NewDocumentUpload(adminIDs[0], "botdata.db")
        doc.Caption = "📦 بکاپ هفتگی دیتابیس ربات"
        sentDoc, err := bot.Send(doc)
        if err == nil && len(adminIDs) > 1 {
            // Forward به بقیه ادمین‌ها
            for i := 1; i < len(adminIDs); i++ {
                forward := tgbotapi.NewForward(adminIDs[i], sentDoc.Chat.ID, sentDoc.MessageID)
                bot.Send(forward)
            }
        }
    }
}

// واکشی همه آیدی کاربران از جدول charges
func getAllUserIDsFromDB() ([]int64, error) {
    rows, err := db.Query("SELECT DISTINCT user_id FROM charges")
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var ids []int64
    for rows.Next() {
        var id int64
        if err := rows.Scan(&id); err == nil {
            ids = append(ids, id)
        }
    }
    return ids, nil
}

// تابع اصلی
func main() {
    // خواندن فایل .env
    err := godotenv.Load()
    if err != nil {
        log.Println("Warning: .env file not found, using system environment variables")
    }
    
    // خواندن متغیرها از env
    botToken = os.Getenv("BOT_TOKEN")
    cardNumber = os.Getenv("CARD_NUMBER")
    cardHolder = os.Getenv("CARD_HOLDER")
    adminIDsStr := os.Getenv("ADMIN_IDS") // مثال: "629590481,1150702474"
    if adminIDsStr == "" {
        adminIDsStr = "629590481,1150702474" // مقدار پیش‌فرض برای dev
    }
    for _, s := range strings.Split(adminIDsStr, ",") {
        s = strings.TrimSpace(s)
        if s == "" { continue }
        id, err := strconv.ParseInt(s, 10, 64)
        if err == nil {
            adminIDs = append(adminIDs, id)
        }
    }
    if botToken == "" {
        log.Fatal("BOT_TOKEN env is not set!")
    }
    if cardNumber == "" {
        cardNumber = "5859-8312-4246-5762" // پیش‌فرض dev
    }
    if cardHolder == "" {
        cardHolder = "علی اسماعیلی"
    }
    initDB()
    bot := NewTelegramBot()

    // بکاپ هفتگی دیتابیس
    go func() {
        for {
            sendWeeklyBackupToAllAdmins(bot.bot)
            time.Sleep(7 * 24 * time.Hour) // هر هفته یکبار
        }
    }()

    bot.Start()
}

// ایجاد نمونه جدید ربات
func NewTelegramBot() *TelegramBot {
    bot, err := tgbotapi.NewBotAPI(botToken)
    if err != nil {
        log.Panic(err)
    }

    tg := &TelegramBot{
        bot:        bot,
        balances:   make(map[int64]int),
        users:      make(map[int64]string),
        userStates: make(map[int64]*UserState),
        processedReceipts: make(map[string]bool),
    }
    tg.loadAllBalancesFromDB() // بارگذاری موجودی کاربران از دیتابیس
    return tg
}

// شروع ربات و گوش دادن به پیام‌ها
func (t *TelegramBot) Start() {
    u := tgbotapi.NewUpdate(0)
    u.Timeout = 60
    updates, _ := t.bot.GetUpdatesChan(u)

    for update := range updates {
        t.handleUpdate(update)
    }
}

// مدیریت اصلی تمام آپدیت‌ها
func (t *TelegramBot) handleUpdate(update tgbotapi.Update) {
    // مدیریت پیام‌های متنی
    if update.Message != nil {
        t.handleMessage(update.Message)
        return
    }

    // مدیریت کلیک روی دکمه‌ها
    if update.CallbackQuery != nil {
        t.handleCallbackQuery(update.CallbackQuery)
    }
}

// مدیریت پیام‌های متنی
func (t *TelegramBot) handleMessage(message *tgbotapi.Message) {
    chatID := message.Chat.ID
    userID := int64(message.From.ID)
    
    // ثبت اطلاعات کاربر
    t.registerUser(userID, message.From.FirstName+" "+message.From.LastName)

    // فرآیند مرحله‌ای شارژ دستی توسط ادمین
    if isAdmin(userID) && adminManualChargeState.Step > 0 {
        switch adminManualChargeState.Step {
        case 1:
            // دریافت آیدی عددی
            id, err := strconv.ParseInt(message.Text, 10, 64)
            if err != nil || id <= 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ آیدی عددی معتبر وارد کنید."))
                return
            }
            adminManualChargeState.TargetUserID = id
            t.bot.Send(tgbotapi.NewMessage(chatID, "💰 مبلغ شارژ را وارد کنید (تومان):"))
            adminManualChargeState.Step = 2
            return
        case 2:
            // دریافت مبلغ
            amount, err := strconv.Atoi(message.Text)
            if err != nil || amount <= 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ مبلغ معتبر وارد کنید (مثلاً 50000)."))
                return
            }
            // شارژ کاربر
            _, err = db.Exec("INSERT INTO charges (user_id, amount) VALUES (?, ?)", adminManualChargeState.TargetUserID, amount)
            if err != nil {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در ثبت شارژ: "+err.Error()))
            } else {
                // افزایش موجودی در map حافظه
                t.balances[adminManualChargeState.TargetUserID] += amount
                t.bot.Send(tgbotapi.NewMessage(chatID, "✅ مبلغ با موفقیت به کاربر اضافه شد."))
                // اطلاع به کاربر
                t.bot.Send(tgbotapi.NewMessage(adminManualChargeState.TargetUserID, fmt.Sprintf("👑 ادمین مبلغ %d تومان به حساب شما اضافه کرد!", amount)))
            }
            adminManualChargeState.Step = 0
            adminManualChargeState.TargetUserID = 0
            return
        }
    }

    // فرآیند اطلاعات کاربر
    if isAdmin(userID) && adminUserInfoState.Waiting {
        targetID, err := strconv.ParseInt(message.Text, 10, 64)
        if err != nil || targetID <= 0 {
            t.bot.Send(tgbotapi.NewMessage(chatID, "❌ آیدی عددی معتبر وارد کنید."))
            return
        }
        t.showSpecificUserInfo(chatID, targetID)
        adminUserInfoState.Waiting = false
        return
    }

    // اطلاع‌رسانی همگانی: اگر ادمین در حالت انتظار پیام است
    if isAdmin(userID) && adminBroadcastState.Waiting {
        text := message.Text
        // ارسال پیام به همه کاربران دیتابیس
        ids, err := getAllUserIDsFromDB()
        if err != nil || len(ids) == 0 {
            t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در واکشی کاربران یا هیچ کاربری یافت نشد."))
        } else {
            for _, uid := range ids {
                t.bot.Send(tgbotapi.NewMessage(uid, text))
            }
            // پیام تایید و دکمه بازگشت
            msg := tgbotapi.NewMessage(chatID, "✅ پیام با موفقیت برای همه کاربران ارسال شد.")
            msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
                tgbotapi.NewInlineKeyboardRow(
                    tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به پنل ادمین", "back_to_admin_panel"),
                ),
            )
            t.bot.Send(msg)
        }
        adminBroadcastState.Waiting = false
        return
    }

    // شروع فرآیند شارژ دستی
    if isAdmin(userID) && (message.Text == "شارژ دستی کاربر" || message.Text == "/manual_charge") {
        adminManualChargeState.Step = 1
        adminManualChargeState.TargetUserID = 0
        t.bot.Send(tgbotapi.NewMessage(chatID, "🔢 آیدی عددی کاربر را وارد کنید:"))
        return
    }

    // شروع اطلاع‌رسانی همگانی
    if isAdmin(userID) && message.Text == "اطلاع رسانی همگانی" {
        adminBroadcastState.Waiting = true
        t.bot.Send(tgbotapi.NewMessage(chatID, "✏️ لطفاً پیام مورد نظر برای ارسال به همه کاربران را وارد کنید:"))
        return
    }

    // منطق حذف سرویس توسط ادمین
    if isAdmin(userID) && strings.HasPrefix(message.Text, "حذف سرویس ") {
        name := strings.TrimSpace(strings.TrimPrefix(message.Text, "حذف سرویس "))
        if name != "" {
            res, err := db.Exec("DELETE FROM services WHERE name = ?", name)
            count, _ := res.RowsAffected()
            if err != nil || count == 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در حذف سرویس یا چنین سرویسی وجود ندارد."))
            } else {
                t.bot.Send(tgbotapi.NewMessage(chatID, "✅ سرویس با موفقیت حذف شد."))
            }
        }
        return
    }

    // --- Admin add panel flow ---
    if isAdmin(userID) && adminAddPanelState.Step > 0 {
        switch adminAddPanelState.Step {
        case 1:
            adminAddPanelState.TempPanel.PanelURL = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "🍪 مقدار کوکی را وارد کنید:"))
            adminAddPanelState.Step = 2
            return
        case 2:
            adminAddPanelState.TempPanel.Cookie = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "🔗 مقدار configMiddle را وارد کنید:"))
            adminAddPanelState.Step = 3
            return
        case 3:
            adminAddPanelState.TempPanel.ConfigMiddle = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "🆔 مقدار id را وارد کنید:"))
            adminAddPanelState.Step = 4
            return
        case 4:
            adminAddPanelState.TempPanel.PanelID = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "👥 محدودیت تعداد کاربر این پنل را وارد کنید (عدد):"))
            adminAddPanelState.Step = 5
            return
        case 5:
            limit, err := strconv.Atoi(message.Text)
            if err != nil || limit <= 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ لطفاً یک عدد معتبر وارد کنید."))
                return
            }
            adminAddPanelState.TempPanel.UserLimit = limit
            // Save to DB
            _, err = db.Exec("INSERT INTO panels (panel_url, cookie, config_middle, panel_id, user_limit, used_count) VALUES (?, ?, ?, ?, ?, 0)",
                adminAddPanelState.TempPanel.PanelURL,
                adminAddPanelState.TempPanel.Cookie,
                adminAddPanelState.TempPanel.ConfigMiddle,
                adminAddPanelState.TempPanel.PanelID,
                adminAddPanelState.TempPanel.UserLimit,
            )
            if err != nil {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در ذخیره پنل: "+err.Error()))
            } else {
                t.bot.Send(tgbotapi.NewMessage(chatID, "✅ پنل جدید با موفقیت ذخیره شد!"))
            }
            adminAddPanelState.Step = 0
            adminAddPanelState.TempPanel = PanelConfig{}
            t.showAdminMenu(chatID)
            return
        }
    }

    // --- Admin add service flow ---
    if isAdmin(userID) && adminAddServiceState.Step > 0 {
        switch adminAddServiceState.Step {
        case 1:
            adminAddServiceState.TempDesc = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "💾 حجم سرویس را به گیگ وارد کنید (مثلاً 20):"))
            adminAddServiceState.Step = 2
            return
        case 2:
            gb, err := strconv.Atoi(message.Text)
            if err != nil || gb <= 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ لطفاً یک عدد معتبر وارد کنید."))
                return
            }
            adminAddServiceState.TempGB = gb
            t.bot.Send(tgbotapi.NewMessage(chatID, "📆 تعداد روز سرویس را وارد کنید (مثلاً 60):"))
            adminAddServiceState.Step = 3
            return
        case 3:
            days, err := strconv.Atoi(message.Text)
            if err != nil || days <= 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ لطفاً یک عدد معتبر وارد کنید."))
                return
            }
            adminAddServiceState.TempDays = days
            t.bot.Send(tgbotapi.NewMessage(chatID, "💰 قیمت سرویس را وارد کنید (تومان):"))
            adminAddServiceState.Step = 4
            return
        case 4:
            price, err := strconv.Atoi(message.Text)
            if err != nil || price <= 0 {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ لطفاً یک عدد معتبر وارد کنید."))
                return
            }
            adminAddServiceState.TempPrice = price
            t.bot.Send(tgbotapi.NewMessage(chatID, "📝 یک نام کوتاه برای سرویس وارد کنید (مثلاً: تست):"))
            adminAddServiceState.Step = 5
            return
        case 5:
            adminAddServiceState.TempName = message.Text
            // ذخیره در دیتابیس
            _, err := db.Exec("INSERT INTO services (name, description, price, days, gb) VALUES (?, ?, ?, ?, ?)", adminAddServiceState.TempName, adminAddServiceState.TempDesc, adminAddServiceState.TempPrice, adminAddServiceState.TempDays, adminAddServiceState.TempGB)
            if err != nil {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در ذخیره سرویس: "+err.Error()))
            } else {
                t.bot.Send(tgbotapi.NewMessage(chatID, "✅ سرویس جدید با موفقیت ذخیره شد!"))
            }
            adminAddServiceState.Step = 0
            adminAddServiceState.TempDesc = ""
            adminAddServiceState.TempDays = 0
            adminAddServiceState.TempGB = 0
            adminAddServiceState.TempPrice = 0
            adminAddServiceState.TempName = ""
            t.showAdminMenu(chatID)
            return
        }
    }

    // مدیریت دستورات
    if message.IsCommand() {
        t.handleCommand(message)
        return
    }

    // مدیریت ورودی‌های کاربر
    t.handleUserInput(message)
}

// ثبت کاربر جدید
func (t *TelegramBot) registerUser(userID int64, fullName string) {
    t.users[userID] = fullName
    if _, exists := t.balances[userID]; !exists {
        t.balances[userID] = 0
    }
    if _, exists := t.userStates[userID]; !exists {
        t.userStates[userID] = &UserState{}
    }
}

// مدیریت دستورات
func (t *TelegramBot) handleCommand(message *tgbotapi.Message) {
    userID := int64(message.From.ID)

    if message.Command() == "start" {
        if isAdmin(userID) {
            t.showAdminMenu(message.Chat.ID)
        } else {
            // پیام خوش‌آمدگویی فقط هنگام استارت
            t.bot.Send(tgbotapi.NewMessage(message.Chat.ID, "سلام عزیز! به ربات خوش اومدی. یکی از گزینه‌ها رو انتخاب کن:"))
            t.showUserMenu(message.Chat.ID)
        }
    }
}

// نمایش منوی ادمین
func (t *TelegramBot) showAdminMenu(chatID int64) {
    adminKeyboard := tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("➕ شارژ دستی کاربر", "manual_charge"),
            tgbotapi.NewInlineKeyboardButtonData("📄 اطلاعات کاربران", "user_info"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("➕ افزودن سرویس", "start_add_service"),
            tgbotapi.NewInlineKeyboardButtonData("🗑 حذف سرویس", "delete_service"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("📢 اطلاع رسانی همگانی", "broadcast"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("⚙️ افزودن پنل جدید", "add_panel"),
            tgbotapi.NewInlineKeyboardButtonData("🗑 حذف پنل", "delete_panel"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("📋 نمایش همه پنل‌ها", "show_panels"),
        ),
    )
    msg := tgbotapi.NewMessage(chatID, "👑 به پنل ادمین خوش اومدی")
    msg.ReplyMarkup = adminKeyboard
    t.bot.Send(msg)
}

// نمایش منوی کاربر
func (t *TelegramBot) showUserMenu(chatID int64) {
    userKeyboard := tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🛒 خرید اشتراک", "buy_subscription"),
            tgbotapi.NewInlineKeyboardButtonData("📦 اشتراک‌های من", "my_subscriptions"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("👤 حساب کاربری", "user_account"),
            tgbotapi.NewInlineKeyboardButtonData("🧠 آموزش‌ها", "tutorials"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("⚡ افزایش موجودی", "top_up"),
        ),
    )
    msg := tgbotapi.NewMessage(chatID, "👇 منوی کاربری:")
    msg.ReplyMarkup = userKeyboard
    t.bot.Send(msg)
}

// مدیریت ورودی‌های کاربر
func (t *TelegramBot) handleUserInput(message *tgbotapi.Message) {
    // chatID := message.Chat.ID
    userID := int64(message.From.ID)
    state := t.userStates[userID]

    // مدیریت ورود مبلغ
    if state.WaitingForAmount {
        t.handleAmountInput(message)
        return
    }

    // مدیریت ارسال فیش
    if message.Photo != nil && state.WaitingForReceipt > 0 {
        t.handleReceiptPhoto(message)
    }
}

// مدیریت ورود مبلغ
func (t *TelegramBot) handleAmountInput(message *tgbotapi.Message) {
    chatID := message.Chat.ID
    userID := int64(message.From.ID)
    state := t.userStates[userID]

    amount, err := strconv.Atoi(message.Text)
    if err != nil || amount <= 0 {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❗ لطفاً فقط عدد معتبر وارد کن (مثلاً 50000)"))
        return
    }

    state.WaitingForReceipt = amount
    state.WaitingForAmount = false

    text := fmt.Sprintf("✅ مبلغ *%d تومان* ثبت شد.\n\n💳 لطفاً مبلغ را به شماره کارت زیر واریز کن:\n\n`%s`\n👤 به نام *%s*\n\nسپس دکمه زیر را بزن و تصویر فیش را ارسال کن.", 
        amount, cardNumber, cardHolder)
    
    msg := tgbotapi.NewMessage(chatID, text)
    msg.ParseMode = "Markdown"
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("📎 ارسال تصویر فیش", "send_receipt"),
        ),
    )
    t.bot.Send(msg)
}

// مدیریت ارسال فیش
func (t *TelegramBot) handleReceiptPhoto(message *tgbotapi.Message) {
    chatID := message.Chat.ID
    userID := int64(message.From.ID)
    state := t.userStates[userID]

    amount := state.WaitingForReceipt
    state.PendingAmount = amount
    state.WaitingForReceipt = 0

    t.bot.Send(tgbotapi.NewMessage(chatID, "✅ فیش دریافت شد. در حال بررسی توسط ادمین..."))

    // تولید شناسه یکتا برای فیش
    receiptID := fmt.Sprintf("%d_%d_%d", userID, time.Now().UnixNano(), rand.Intn(10000))
    photos := *message.Photo
    lastPhoto := photos[len(photos)-1]
    pendingReceipts[receiptID] = PendingReceipt{
        ID:      receiptID,
        UserID:  userID,
        Amount:  amount,
        PhotoID: lastPhoto.FileID,
    }

    // ارسال فیش برای بررسی ادمین
    t.sendReceiptToAdminWithID(userID, amount, lastPhoto.FileID, receiptID)
}

// ارسال فیش برای بررسی ادمین با شناسه یکتا
func (t *TelegramBot) sendReceiptToAdminWithID(userID int64, amount int, fileID, receiptID string) {
    caption := fmt.Sprintf("🧾 فیش جدید:\n👤 %s\n🆔 آیدی تلگرام: %d\n💰 مبلغ: %d تومان", 
        t.users[userID], userID, amount)
    // ارسال به همه ادمین‌ها
    for _, adminID := range adminIDs {
        adminMsg := tgbotapi.NewPhotoShare(adminID, fileID)
        adminMsg.Caption = caption
        adminMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
            tgbotapi.NewInlineKeyboardRow(
                tgbotapi.NewInlineKeyboardButtonData("✅ تایید", "approve_"+receiptID),
                tgbotapi.NewInlineKeyboardButtonData("❌ رد", "reject_"+receiptID),
            ),
        )
        t.bot.Send(adminMsg)
    }
}

// مدیریت کلیک روی دکمه‌ها
func (t *TelegramBot) handleCallbackQuery(callback *tgbotapi.CallbackQuery) {
    chatID := callback.Message.Chat.ID
    userID := int64(callback.From.ID)
    data := callback.Data

    // پاسخ به callback query
    callbackAnswer := tgbotapi.NewCallback(callback.ID, "")
    t.bot.AnswerCallbackQuery(callbackAnswer)

    switch data {
    case "top_up":
        t.showTopUpMethods(chatID)
    case "card_to_card":
        t.askForAmount(chatID, userID)
    case "send_receipt":
        t.askForReceipt(chatID)
    case "user_account":
        t.showUserAccount(chatID, userID)
    case "buy_subscription":
        t.showServicesForUser(chatID, userID)
    case "my_subscriptions":
        t.showMySubscriptions(chatID)
    case "tutorials":
        t.showTutorials(chatID)
    case "manual_charge":
        adminManualChargeState.Step = 1
        adminManualChargeState.TargetUserID = 0
        t.bot.Send(tgbotapi.NewMessage(chatID, "🔢 آیدی عددی کاربر را وارد کنید:"))
    case "user_info":
        adminUserInfoState.Waiting = true
        t.bot.Send(tgbotapi.NewMessage(chatID, "🔢 آیدی عددی کاربر مورد نظر را وارد کنید:"))
    case "back_to_menu":
        t.showUserMenu(chatID)
    case "start_add_service":
        if isAdmin(userID) {
            adminAddServiceState.Step = 1
            adminAddServiceState.TempDesc = ""
            adminAddServiceState.TempDays = 0
            adminAddServiceState.TempGB = 0
            adminAddServiceState.TempPrice = 0
            adminAddServiceState.TempName = ""
            t.bot.Send(tgbotapi.NewMessage(chatID, "📝 توضیحات سرویس را وارد کنید (مثلاً: ویژه تابستان):"))
        }
    case "delete_service":
        t.showServicesForAdminDelete(chatID)
    case "broadcast":
        adminBroadcastState.Waiting = true
        t.bot.Send(tgbotapi.NewMessage(chatID, "✏️ لطفاً پیام مورد نظر برای ارسال به همه کاربران را وارد کنید:"))
    case "back_to_admin_panel":
        t.showAdminMenu(chatID)
    case "add_panel":
        if isAdmin(userID) {
            adminAddPanelState.Step = 1
            adminAddPanelState.TempPanel = PanelConfig{}
            t.bot.Send(tgbotapi.NewMessage(chatID, "🌐 مقدار panelURL را وارد کنید:"))
        }
    case "delete_panel":
        // نمایش لیست پنل‌ها برای حذف
        rows, err := db.Query("SELECT id, panel_url, user_limit, used_count FROM panels")
        if err != nil {
            t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت لیست پنل‌ها."))
            return
        }
        defer rows.Close()
        var btns [][]tgbotapi.InlineKeyboardButton
        for rows.Next() {
            var id, userLimit, usedCount int
            var url string
            rows.Scan(&id, &url, &userLimit, &usedCount)
            text := fmt.Sprintf("%s | %d/%d", url, usedCount, userLimit)
            btns = append(btns, tgbotapi.NewInlineKeyboardRow(
                tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("delete_panel_%d", id)),
            ))
        }
        if len(btns) == 0 {
            t.bot.Send(tgbotapi.NewMessage(chatID, "هیچ پنلی برای حذف وجود ندارد."))
            return
        }
        msg := tgbotapi.NewMessage(chatID, "برای حذف، روی پنل مورد نظر کلیک کنید:")
        msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(btns...)
        t.bot.Send(msg)
    case "show_panels":
        // نمایش همه پنل‌ها
        rows, err := db.Query("SELECT id, panel_url, panel_id, user_limit, used_count FROM panels")
        if err != nil {
            t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت لیست پنل‌ها."))
            return
        }
        defer rows.Close()
        var msg string
        for rows.Next() {
            var id, userLimit, usedCount int
            var url, panelID string
            rows.Scan(&id, &url, &panelID, &userLimit, &usedCount)
            msg += fmt.Sprintf("PanelID: %s\nURL: %s\nظرفیت: %d/%d\n\n", panelID, url, usedCount, userLimit)
        }
        if msg == "" {
            msg = "هیچ پنلی ثبت نشده است."
        }
        t.bot.Send(tgbotapi.NewMessage(chatID, msg))
    default:
        if strings.HasPrefix(data, "delete_service_") {
            serviceID := strings.TrimPrefix(data, "delete_service_")
            t.handleAdminDeleteService(chatID, serviceID)
        } else if strings.HasPrefix(data, "service_") {
            serviceID := strings.TrimPrefix(data, "service_")
            t.handleUserServiceSelect(chatID, userID, serviceID)
        } else if strings.HasPrefix(data, "approve_") {
            receiptID := strings.TrimPrefix(data, "approve_")
            t.approveReceiptByID(receiptID, callback)
        } else if strings.HasPrefix(data, "reject_") {
            receiptID := strings.TrimPrefix(data, "reject_")
            t.rejectReceiptByID(receiptID, callback)
        } else if strings.HasPrefix(data, "delete_panel_") {
            idStr := strings.TrimPrefix(data, "delete_panel_")
            id, _ := strconv.Atoi(idStr)
            _, err := db.Exec("DELETE FROM panels WHERE id = ?", id)
            if err != nil {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در حذف پنل."))
            } else {
                t.bot.Send(tgbotapi.NewMessage(chatID, "✅ پنل با موفقیت حذف شد."))
            }
            t.showAdminMenu(chatID)
            return
        }
    }
}

// نمایش روش‌های افزایش موجودی
func (t *TelegramBot) showTopUpMethods(chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "💸 روش افزایش موجودی را انتخاب کن:")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("💳 کارت به کارت", "card_to_card"),
        ),
    )
    t.bot.Send(msg)
}

// درخواست مبلغ از کاربر
func (t *TelegramBot) askForAmount(chatID int64, userID int64) {
    if t.userStates[userID] == nil {
        t.userStates[userID] = &UserState{}
    }
    t.userStates[userID].WaitingForAmount = true
    t.bot.Send(tgbotapi.NewMessage(chatID, "💰 لطفاً مبلغ مورد نظر را وارد کن (فقط عدد):"))
}

// درخواست فیش از کاربر
func (t *TelegramBot) askForReceipt(chatID int64) {
    t.bot.Send(tgbotapi.NewMessage(chatID, "📎 لطفاً تصویر فیش کارت‌به‌کارت رو همین‌جا بفرست."))
}

// نمایش اطلاعات حساب کاربر
func (t *TelegramBot) showUserAccount(chatID int64, userID int64) {
    balance := t.balances[userID]
    msg := fmt.Sprintf("👤 اطلاعات حساب شما:\n\n📌 نام: %s\n💰 موجودی: %d تومان", t.users[userID], balance)
    // همه کانفیگ‌های کاربر
    rows, err := db.Query("SELECT email FROM user_configs WHERE user_id = ? ORDER BY id DESC", userID)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت کانفیگ‌ها."))
        return
    }
    defer rows.Close()
    var emails []string
    for rows.Next() {
        var email string
        rows.Scan(&email)
        emails = append(emails, email)
    }
    if len(emails) == 0 {
        msg += "\n\n📦 شما هیچ کانفیگی ندارید."
        t.bot.Send(tgbotapi.NewMessage(chatID, msg))
        return
    }
    // گرفتن همه پنل‌ها و جستجوی ایمیل‌ها
    panels, err := getAllPanels()
    if err != nil || len(panels) == 0 {
        msg += "\n\n❌ خطا در دریافت پنل‌ها."
        t.bot.Send(tgbotapi.NewMessage(chatID, msg))
        return
    }
    found := make(map[string]bool)
    for _, panel := range panels {
        usages, err := getPanelUsages(panel)
        if err != nil {
            msg += fmt.Sprintf("\n\n❌ خطا در ارتباط با پنل %s: %v", panel.PanelURL, err)
            continue
        }
        for _, email := range emails {
            if usage, ok := usages[email]; ok {
                used := usage.Up + usage.Down
                left := usage.Total - used
                gbTotal := float64(usage.Total) / 1073741824.0
                gbLeft := float64(left) / 1073741824.0
                expireText := "نامشخص"
                if usage.ExpiryTime > 0 {
                    now := time.Now().UnixMilli()
                    leftMs := usage.ExpiryTime - now
                    if leftMs > 0 {
                        daysLeft := int(leftMs / (1000 * 60 * 60 * 24))
                        hoursLeft := int((leftMs / (1000 * 60 * 60)) % 24)
                        expireText = fmt.Sprintf("%d روز و %d ساعت", daysLeft, hoursLeft)
                    } else {
                        expireText = "منقضی شده"
                    }
                } else if usage.ExpiryTime < 0 {
                    leftMs := -usage.ExpiryTime
                    daysLeft := int(leftMs / (1000 * 60 * 60 * 24))
                    hoursLeft := int((leftMs / (1000 * 60 * 60)) % 24)
                    expireText = fmt.Sprintf("%d روز و %d ساعت", daysLeft, hoursLeft)
                }
                msg += fmt.Sprintf("\n\n📧 %s\nحجم باقی‌مانده: %.2fGB از %.2fGB\nروز باقی‌مانده: %s",
                    usage.Email, gbLeft, gbTotal, expireText)
                found[email] = true
            }
        }
    }
    // ایمیل‌هایی که پیدا نشدند
    for _, email := range emails {
        if !found[email] {
            msg += fmt.Sprintf("\n\n📧 %s\n⛔ اطلاعاتی یافت نشد!", email)
        }
    }
    t.bot.Send(tgbotapi.NewMessage(chatID, msg))
}

// نمایش اطلاعات اشتراک
func (t *TelegramBot) showSubscriptionInfo(chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "🛒 فعلاً پلن‌ها فعال نیستن، ولی به‌زودی اضافه می‌شن!")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
}

// نمایش اشتراک‌های کاربر
func (t *TelegramBot) showMySubscriptions(chatID int64) {
    userID := chatID
    rows, err := db.Query("SELECT config_link, email, sub_id, created_at FROM user_configs WHERE user_id = ? ORDER BY id DESC", userID)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت اشتراک‌ها."))
        return
    }
    defer rows.Close()
    var msg string
    i := 1
    for rows.Next() {
        var link, email, subID, created string
        rows.Scan(&link, &email, &subID, &created)
        msg += fmt.Sprintf("%d. 📧 Email: %s\n🔗 لینک کانفیگ:\n`%s`\n\n", i, email, link)
        i++
    }
    if msg == "" {
        msg = "📦 شما در حال حاضر اشتراک فعالی ندارید."
    }
    m := tgbotapi.NewMessage(chatID, msg)
    m.ParseMode = "Markdown"
    m.DisableWebPagePreview = true
    m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(m)
}

// نمایش آموزش‌ها
func (t *TelegramBot) showTutorials(chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "🧠 آموزش‌ها در حال آماده‌سازی هستند. به‌زودی اضافه می‌شن...")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
}

// نمایش اطلاعات کاربر خاص
func (t *TelegramBot) showSpecificUserInfo(chatID int64, targetUserID int64) {
    // بررسی وجود کاربر در دیتابیس
    var userName string
    var balance int
    
    // ابتدا از map حافظه چک می‌کنیم
    if name, exists := t.users[targetUserID]; exists {
        userName = name
        balance = t.balances[targetUserID]
    } else {
        // اگر در حافظه نبود، از دیتابیس چک می‌کنیم
        err := db.QueryRow("SELECT SUM(amount) FROM charges WHERE user_id = ?", targetUserID).Scan(&balance)
        if err != nil {
            if err == sql.ErrNoRows {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ کاربری با این آیدی یافت نشد."))
                return
            }
            t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در بررسی کاربر: "+err.Error()))
            return
        }
        // اگر کاربر در دیتابیس وجود دارد ولی در حافظه نیست، نام پیش‌فرض قرار می‌دهیم
        userName = fmt.Sprintf("کاربر %d", targetUserID)
    }
    
    info := fmt.Sprintf("👤 %s\n🆔 آیدی: %d\n💰 موجودی: %d تومان\n", userName, targetUserID, balance)
    
    // تعداد کانفیگ‌ها و ایمیل‌ها
    rows, err := db.Query("SELECT email FROM user_configs WHERE user_id = ? ORDER BY id DESC", targetUserID)
    if err == nil {
        defer rows.Close()
        var emails []string
        for rows.Next() {
            var email string
            rows.Scan(&email)
            emails = append(emails, email)
        }
        info += fmt.Sprintf("📦 تعداد کانفیگ: %d\n", len(emails))
        
        if len(emails) > 0 {
            info += "📧 ایمیل‌ها:\n"
            for _, email := range emails {
                info += fmt.Sprintf("  • %s\n", email)
            }
            
            // اطلاعات مصرف از پنل‌ها
            panels, err := getAllPanels()
            if err == nil && len(panels) > 0 {
                info += "📊 اطلاعات مصرف:\n"
                for _, panel := range panels {
                    usages, err := getPanelUsages(panel)
                    if err == nil {
                        for _, email := range emails {
                            if usage, ok := usages[email]; ok {
                                used := usage.Up + usage.Down
                                left := usage.Total - used
                                gbTotal := float64(usage.Total) / 1073741824.0
                                gbLeft := float64(left) / 1073741824.0
                                expireText := "نامشخص"
                                if usage.ExpiryTime > 0 {
                                    now := time.Now().UnixMilli()
                                    leftMs := usage.ExpiryTime - now
                                    if leftMs > 0 {
                                        daysLeft := int(leftMs / (1000 * 60 * 60 * 24))
                                        hoursLeft := int((leftMs / (1000 * 60 * 60)) % 24)
                                        expireText = fmt.Sprintf("%d روز و %d ساعت", daysLeft, hoursLeft)
                                    } else {
                                        expireText = "منقضی شده"
                                    }
                                } else if usage.ExpiryTime < 0 {
                                    leftMs := -usage.ExpiryTime
                                    daysLeft := int(leftMs / (1000 * 60 * 60 * 24))
                                    hoursLeft := int((leftMs / (1000 * 60 * 60)) % 24)
                                    expireText = fmt.Sprintf("%d روز و %d ساعت", daysLeft, hoursLeft)
                                }
                                info += fmt.Sprintf("  📧 %s: %.2fGB باقی از %.2fGB | %s\n", 
                                    email, gbLeft, gbTotal, expireText)
                            }
                        }
                    }
                }
            }
        }
    }
    
    msg := tgbotapi.NewMessage(chatID, info)
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به پنل ادمین", "back_to_admin_panel"),
        ),
    )
    t.bot.Send(msg)
}

// نمایش سرویس‌ها به کاربر
func (t *TelegramBot) showServicesForUser(chatID int64, userID int64) {
    rows, err := db.Query("SELECT id, name, description, price, days, gb FROM services")
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت سرویس‌ها."))
        return
    }
    defer rows.Close()
    type Service struct {
        ID    int
        Name  string
        Desc  string
        Price int
        Days  int
        GB    int
    }
    var services []Service
    for rows.Next() {
        var s Service
        rows.Scan(&s.ID, &s.Name, &s.Desc, &s.Price, &s.Days, &s.GB)
        services = append(services, s)
    }
    if len(services) == 0 {
        t.bot.Send(tgbotapi.NewMessage(chatID, "🛒 فعلاً پلنی برای خرید فعال نیست!"))
        return
    }
    // ساخت دکمه‌ها
    var btns [][]tgbotapi.InlineKeyboardButton
    for _, s := range services {
        text := fmt.Sprintf("%d روزه | %d گیگ | %d تومان | %s", s.Days, s.GB, s.Price, s.Name)
        btns = append(btns, tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("service_%d", s.ID)),
        ))
    }
    msg := tgbotapi.NewMessage(chatID, "لطفاً یکی از سرویس‌های زیر را انتخاب کن:")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(btns...)
    t.bot.Send(msg)
}

// بررسی موجودی کاربر هنگام انتخاب سرویس
func (t *TelegramBot) handleUserServiceSelect(chatID, userID int64, serviceData string) {
    // واکشی id عددی سرویس
    serviceID, err := strconv.Atoi(serviceData)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ سرویس انتخابی نامعتبر است."))
        return
    }
    var price int
    var desc, name string
    var days, gb int
    err = db.QueryRow("SELECT price, description, name, days, gb FROM services WHERE id = ?", serviceID).Scan(&price, &desc, &name, &days, &gb)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت اطلاعات سرویس یا سرویس حذف شده است."))
        return
    }
    balance := t.balances[userID]
    if balance < price {
        msg := tgbotapi.NewMessage(chatID, "❌ موجودی کافی نیست. لطفاً حساب خود را شارژ کنید.")
        msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
            tgbotapi.NewInlineKeyboardRow(
                tgbotapi.NewInlineKeyboardButtonData("⚡ افزایش موجودی", "top_up"),
            ),
        )
        t.bot.Send(msg)
        return
    }
    // --- خرید سرویس: گرفتن کانفیگ و ذخیره ---
    panel, err := getAvailablePanelConfig()
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ هیچ پنلی با ظرفیت آزاد وجود ندارد. لطفاً بعداً تلاش کنید یا به ادمین اطلاع دهید."))
        t.bot.Send(tgbotapi.NewMessage(adminIDs[0], fmt.Sprintf("❌ خطا در خرید سرویس برای کاربر %d: %v", userID, err)))
        return
    }
    _, email, subID, config, err := addClient(panel.PanelURL, panel.Cookie, panel.ConfigMiddle, panel.PanelID, gb, days)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت کانفیگ: "+err.Error()))
        t.bot.Send(tgbotapi.NewMessage(adminIDs[0], fmt.Sprintf("❌ خطا در دریافت کانفیگ برای کاربر %d: %v", userID, err)))
        return
    }
    // ذخیره در دیتابیس
    _, err = db.Exec("INSERT INTO user_configs (user_id, service_id, email, sub_id, config_link) VALUES (?, ?, ?, ?, ?)",
        userID, serviceID, email, subID, config)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در ذخیره کانفیگ: "+err.Error()))
        return
    }
    // کم کردن موجودی
    t.balances[userID] -= price
    _, _ = db.Exec("INSERT INTO charges (user_id, amount) VALUES (?, ?)", userID, -price)
    // افزایش used_count پنل
    _, _ = db.Exec("UPDATE panels SET used_count = used_count + 1 WHERE id = ?", panel.ID)
    // ارسال کانفیگ به کاربر
    configMsg := fmt.Sprintf("✅ خرید با موفقیت انجام شد!\n\n🔗 لینک کانفیگ شما:\n`%s`", config)
    msg := tgbotapi.NewMessage(chatID, configMsg)
    msg.ParseMode = "Markdown"
    msg.DisableWebPagePreview = true
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
}

// تایید فیش توسط ادمین با receiptID
func (t *TelegramBot) approveReceiptByID(receiptID string, callback *tgbotapi.CallbackQuery) {
    receipt, ok := pendingReceipts[receiptID]
    if !ok {
        t.bot.Send(tgbotapi.NewMessage(adminIDs[0], "❌ این فیش قبلاً بررسی شده یا وجود ندارد."))
        return
    }
    
    // آپدیت موجودی در دیتابیس
    _, err := db.Exec("INSERT INTO charges (user_id, amount) VALUES (?, ?)", receipt.UserID, receipt.Amount)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(adminIDs[0], "❌ خطا در ثبت شارژ در دیتابیس: "+err.Error()))
        return
    }
    
    // آپدیت موجودی در map حافظه
    t.balances[receipt.UserID] += receipt.Amount
    
    // اطلاع‌رسانی به کاربر
    msg := tgbotapi.NewMessage(receipt.UserID, 
        fmt.Sprintf("✅ فیش شما تأیید شد و %d تومان به حساب‌تان افزوده شد.\n💰 موجودی جدید: %d تومان", receipt.Amount, t.balances[receipt.UserID]))
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
    
    // حذف دکمه‌ها از پیام ادمین
    editMsg := tgbotapi.NewEditMessageReplyMarkup(callback.Message.Chat.ID, 
        callback.Message.MessageID, 
        tgbotapi.NewInlineKeyboardMarkup())
    t.bot.Send(editMsg)
    
    // اطلاع‌رسانی به ادمین
    for _, adminID := range adminIDs {
        t.bot.Send(tgbotapi.NewMessage(adminID, 
            fmt.Sprintf("🟢 فیش کاربر %d تأیید شد و %d تومان شارژ شد.", receipt.UserID, receipt.Amount)))
    }
    
    // حذف فیش از map
    delete(pendingReceipts, receiptID)
}

// رد فیش توسط ادمین با receiptID
func (t *TelegramBot) rejectReceiptByID(receiptID string, callback *tgbotapi.CallbackQuery) {
    receipt, ok := pendingReceipts[receiptID]
    if !ok {
        t.bot.Send(tgbotapi.NewMessage(adminIDs[0], "❌ این فیش قبلاً بررسی شده یا وجود ندارد."))
        return
    }
    // اطلاع‌رسانی به کاربر
    msg := tgbotapi.NewMessage(receipt.UserID, 
        "❌ فیش شما رد شد. لطفاً بررسی کرده و مجدداً ارسال نمایید.")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
    // حذف دکمه‌ها از پیام ادمین
    editMsg := tgbotapi.NewEditMessageReplyMarkup(callback.Message.Chat.ID, 
        callback.Message.MessageID, 
        tgbotapi.NewInlineKeyboardMarkup())
    t.bot.Send(editMsg)
    // اطلاع‌رسانی به ادمین
    for _, adminID := range adminIDs {
        t.bot.Send(tgbotapi.NewMessage(adminID, 
            fmt.Sprintf("🔴 فیش کاربر %d رد شد.", receipt.UserID)))
    }
    // حذف فیش از map
    delete(pendingReceipts, receiptID)
}

// نمایش سرویس‌ها به ادمین برای حذف
func (t *TelegramBot) showServicesForAdminDelete(chatID int64) {
    rows, err := db.Query("SELECT id, description, price FROM services")
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت سرویس‌ها."))
        return
    }
    defer rows.Close()
    type Service struct {
        ID    int
        Desc  string
        Price int
    }
    var services []Service
    for rows.Next() {
        var s Service
        rows.Scan(&s.ID, &s.Desc, &s.Price)
        services = append(services, s)
    }
    if len(services) == 0 {
        t.bot.Send(tgbotapi.NewMessage(chatID, "هیچ سرویسی برای حذف وجود ندارد."))
        return
    }
    var btns [][]tgbotapi.InlineKeyboardButton
    for _, s := range services {
        text := s.Desc + " | " + fmt.Sprintf("%d تومان", s.Price)
        btns = append(btns, tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("delete_service_%d", s.ID)),
        ))
    }
    msg := tgbotapi.NewMessage(chatID, "برای حذف، روی سرویس مورد نظر کلیک کنید:")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(btns...)
    t.bot.Send(msg)
}

// حذف سرویس با id
func (t *TelegramBot) handleAdminDeleteService(chatID int64, serviceID string) {
    id, err := strconv.Atoi(serviceID)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ سرویس انتخابی نامعتبر است."))
        return
    }
    res, err := db.Exec("DELETE FROM services WHERE id = ?", id)
    count, _ := res.RowsAffected()
    if err != nil || count == 0 {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در حذف سرویس یا چنین سرویسی وجود ندارد."))
    } else {
        t.bot.Send(tgbotapi.NewMessage(chatID, "✅ سرویس با موفقیت حذف شد."))
    }
}

// محاسبه و بارگذاری موجودی همه کاربران از دیتابیس
func (t *TelegramBot) loadAllBalancesFromDB() error {
    rows, err := db.Query("SELECT user_id, SUM(amount) FROM charges GROUP BY user_id")
    if err != nil {
        return err
    }
    defer rows.Close()
    for rows.Next() {
        var uid int64
        var sum int
        if err := rows.Scan(&uid, &sum); err == nil {
            t.balances[uid] = sum
        }
    }
    return nil
}

// --- Utility: randomEmail ---
func randomEmail(length int) string {
    const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
    rand.Seed(time.Now().UnixNano())
    result := make([]byte, length)
    for i := range result {
        result[i] = charset[rand.Intn(len(charset))]
    }
    return string(result)
}

// --- Utility: addClient (panel request) ---
func addClient(panelURL, cookie, configMiddle, id string, gb int, days int) (string, string, string, string, error) {
    clientID := uuid.New().String()
    email := randomEmail(8)
    subID := randomEmail(16)
    expiryTime := -1 * days * 24 * 60 * 60 * 1000 // milliseconds
    clients := map[string]interface{}{
        "clients": []map[string]interface{}{
            {
                "id":         clientID,
                "flow":       "",
                "email":      email,
                "limitIp":    0,
                "totalGB":    gb * 1073741824, // convert GB to bytes
                "expiryTime": expiryTime,
                "enable":     true,
                "tgId":       "",
                "subId":      subID,
                "reset":      0,
            },
        },
    }
    settingsJson, _ := json.Marshal(clients)
    data := url.Values{}
    data.Set("id", id)
    data.Set("settings", string(settingsJson))
    fullURL := fmt.Sprintf("http://%s/panel/inbound/addClient", panelURL)
    req, err := http.NewRequest("POST", fullURL, strings.NewReader(data.Encode()))
    if err != nil {
        return "", "", "", "", err
    }
    req.Header.Set("Accept", "application/json, text/plain, */*")
    req.Header.Set("Accept-Language", "en-US,en;q=0.9,fa-IR;q=0.8,fa;q=0.7")
    req.Header.Set("Connection", "keep-alive")
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
    req.Header.Set("Cookie", cookie)
    req.Header.Set("Origin", fmt.Sprintf("http://%s", panelURL))
    req.Header.Set("Referer", fmt.Sprintf("http://%s/panel/inbounds", panelURL))
    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
    req.Header.Set("X-Requested-With", "XMLHttpRequest")
    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return "", "", "", "", err
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    if resp.StatusCode != 200 {
        return "", "", "", "", fmt.Errorf("Non-200 response: %s", string(body))
    }
    if strings.Contains(string(body), `"success":true`) {
        config := fmt.Sprintf("vless://%s%s%s", clientID, configMiddle, email)
        return clientID, email, subID, config, nil
    }
    return "", "", "", "", fmt.Errorf("Request failed: %s", string(body))
}

// --- Utility: get available panel config ---
func getAvailablePanelConfig() (PanelConfig, error) {
    row := db.QueryRow("SELECT id, panel_url, cookie, config_middle, panel_id, user_limit, used_count FROM panels WHERE user_limit > used_count ORDER BY id ASC LIMIT 1")
    var p PanelConfig
    err := row.Scan(&p.ID, &p.PanelURL, &p.Cookie, &p.ConfigMiddle, &p.PanelID, &p.UserLimit, &p.UsedCount)
    return p, err
}

// --- Get all panels ---
func getAllPanels() ([]PanelConfig, error) {
    rows, err := db.Query("SELECT id, panel_url, cookie, config_middle, panel_id, user_limit, used_count FROM panels")
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var panels []PanelConfig
    for rows.Next() {
        var p PanelConfig
        rows.Scan(&p.ID, &p.PanelURL, &p.Cookie, &p.ConfigMiddle, &p.PanelID, &p.UserLimit, &p.UsedCount)
        panels = append(panels, p)
    }
    return panels, nil
}

// --- Request panel usage ---
func getPanelUsages(panel PanelConfig) (map[string]UsageInfo, error) {
    url := fmt.Sprintf("http://%s/panel/inbound/list", panel.PanelURL)
    req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte{}))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Accept", "application/json, text/plain, */*")
    req.Header.Set("Accept-Language", "en-US,en;q=0.9,fa-IR;q=0.8,fa;q=0.7")
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
    req.Header.Set("Cookie", panel.Cookie)
    req.Header.Set("Origin", fmt.Sprintf("http://%s", panel.PanelURL))
    req.Header.Set("Referer", fmt.Sprintf("http://%s/panel/inbounds", panel.PanelURL))
    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
    req.Header.Set("X-Requested-With", "XMLHttpRequest")
    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    if resp.StatusCode != 200 {
        return nil, fmt.Errorf("Non-200 response: %s", string(body))
    }
    // Parse JSON
    var parsed struct {
        Success bool `json:"success"`
        Obj     []struct {
            ClientStats []struct {
                Email      string `json:"email"`
                Up         int64  `json:"up"`
                Down       int64  `json:"down"`
                Total      int64  `json:"total"`
                ExpiryTime int64  `json:"expiryTime"`
            } `json:"clientStats"`
        } `json:"obj"`
    }
    if err := json.Unmarshal(body, &parsed); err != nil {
        return nil, err
    }
    usages := make(map[string]UsageInfo)
    for _, inbound := range parsed.Obj {
        for _, c := range inbound.ClientStats {
            usages[c.Email] = UsageInfo{
                Email:      c.Email,
                Up:         c.Up,
                Down:       c.Down,
                Total:      c.Total,
                ExpiryTime: c.ExpiryTime,
            }
        }
    }
    return usages, nil
}

// تابع کمکی برای بررسی ادمین بودن
func isAdmin(userID int64) bool {
    for _, id := range adminIDs {
        if userID == id {
            return true
        }
    }
    return false
}


			

