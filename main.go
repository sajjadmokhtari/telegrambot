package main

import (
    "database/sql"
    _ "github.com/mattn/go-sqlite3"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"
    "math/rand"

    "github.com/google/uuid"
    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
)

var db *sql.DB

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
    adminID       int64
}

const (
    adminID int64 = 1150702474 // 👈 آیدی عددی خودت رو وارد کن
    botToken      = "8024742298:AAHP1jBKaTMk9j0ophnn83pQvdBft5yAZwU" // 👈 توکن واقعی رباتت اینجا
    cardNumber    = "5859-8312-4246-5762"
    cardHolder    = "علی اسماعیلی"
)

// ساختار فیش در انتظار بررسی
type PendingReceipt struct {
    ID      string
    UserID  int64
    Amount  int
    PhotoID string
}

// نگهداری فیش‌های در انتظار بررسی
var pendingReceipts = make(map[string]PendingReceipt)

// وضعیت مرحله‌ای شارژ دستی
var adminManualChargeState = struct {
    Step int
    TargetUserID int64
}{Step: 0, TargetUserID: 0}

// وضعیت اطلاع‌رسانی همگانی
var adminBroadcastState = struct {
    Waiting bool
}{Waiting: false}

// وضعیت افزودن پنل
var adminPanelState = struct {
    Step int
    Cookie string
    Link   string
    ID     string
}{Step: 0, Cookie: "", Link: "", ID: ""}

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
        price INTEGER
    );`
    _, err = db.Exec(createServices)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول سرویس‌ها: %v", err)
    }
    // جدول اشتراک‌های خریداری‌شده
    createSubscriptions := `CREATE TABLE IF NOT EXISTS subscriptions (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER,
        service_id INTEGER,
        description TEXT,
        price INTEGER,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );`
    _, err = db.Exec(createSubscriptions)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول اشتراک‌ها: %v", err)
    }
    // جدول تنظیمات پنل
    createPanelSettings := `CREATE TABLE IF NOT EXISTS panel_settings (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        cookie TEXT NOT NULL,
        link TEXT NOT NULL,
        panel_id TEXT NOT NULL,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );`
    _, err = db.Exec(createPanelSettings)
    if err != nil {
        log.Fatalf("خطا در ساخت جدول تنظیمات پنل: %v", err)
    }
    // مهاجرت: افزودن ستون‌های جدید اگر وجود ندارند
    db.Exec("ALTER TABLE services ADD COLUMN description TEXT;")
    db.Exec("ALTER TABLE services ADD COLUMN price INTEGER;")
}

// ارسال فایل دیتابیس به ادمین
func sendDBBackupToAdmin(bot *tgbotapi.BotAPI, adminID int64) {
    doc := tgbotapi.NewDocumentUpload(adminID, "botdata.db")
    doc.Caption = "📦 بکاپ هفتگی دیتابیس ربات"
    bot.Send(doc)
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
    initDB()
    bot := NewTelegramBot()

    // بکاپ هفتگی دیتابیس
    go func() {
        for {
            sendDBBackupToAdmin(bot.bot, bot.adminID)
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
        adminID:    adminID,
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
    if userID == t.adminID && adminManualChargeState.Step > 0 {
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

    // اطلاع‌رسانی همگانی: اگر ادمین در حالت انتظار پیام است
    if userID == t.adminID && adminBroadcastState.Waiting {
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
    if userID == t.adminID && (message.Text == "شارژ دستی کاربر" || message.Text == "/manual_charge") {
        adminManualChargeState.Step = 1
        adminManualChargeState.TargetUserID = 0
        t.bot.Send(tgbotapi.NewMessage(chatID, "🔢 آیدی عددی کاربر را وارد کنید:"))
        return
    }

    // شروع اطلاع‌رسانی همگانی
    if userID == t.adminID && message.Text == "اطلاع رسانی همگانی" {
        adminBroadcastState.Waiting = true
        t.bot.Send(tgbotapi.NewMessage(chatID, "✏️ لطفاً پیام مورد نظر برای ارسال به همه کاربران را وارد کنید:"))
        return
    }

    // منطق حذف سرویس توسط ادمین
    if userID == t.adminID && strings.HasPrefix(message.Text, "حذف سرویس ") {
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

    // فرآیند مرحله‌ای افزودن پنل توسط ادمین
    if userID == t.adminID && adminPanelState.Step > 0 {
        switch adminPanelState.Step {
        case 1:
            // دریافت کوکی
            adminPanelState.Cookie = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "🔗 لطفاً لینک پنل را وارد کنید:"))
            adminPanelState.Step = 2
            return
        case 2:
            // دریافت لینک
            adminPanelState.Link = message.Text
            t.bot.Send(tgbotapi.NewMessage(chatID, "🆔 لطفاً آیدی پنل را وارد کنید:"))
            adminPanelState.Step = 3
            return
        case 3:
            // دریافت آیدی و ذخیره در دیتابیس
            adminPanelState.ID = message.Text
            
            // حذف تنظیمات قبلی (فقط یک پنل فعال)
            db.Exec("DELETE FROM panel_settings")
            
            // ذخیره تنظیمات جدید
            _, err := db.Exec("INSERT INTO panel_settings (cookie, link, panel_id) VALUES (?, ?, ?)", 
                adminPanelState.Cookie, adminPanelState.Link, adminPanelState.ID)
            if err != nil {
                t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در ذخیره تنظیمات پنل: "+err.Error()))
            } else {
                msg := tgbotapi.NewMessage(chatID, "✅ تنظیمات پنل با موفقیت ذخیره شد!")
                msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
                    tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به پنل ادمین", "back_to_admin_panel"),
                    ),
                )
                t.bot.Send(msg)
            }
            adminPanelState.Step = 0
            adminPanelState.Cookie = ""
            adminPanelState.Link = ""
            adminPanelState.ID = ""
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
        if userID == t.adminID {
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
            tgbotapi.NewInlineKeyboardButtonData("⚙️ افزودن پنل", "add_panel"),
            tgbotapi.NewInlineKeyboardButtonData("📢 اطلاع رسانی همگانی", "broadcast"),
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
    adminMsg := tgbotapi.NewPhotoShare(t.adminID, fileID)
    adminMsg.Caption = caption
    adminMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("✅ تایید", "approve_"+receiptID),
            tgbotapi.NewInlineKeyboardButtonData("❌ رد", "reject_"+receiptID),
        ),
    )
    t.bot.Send(adminMsg)
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
        t.showAllUsersInfo(chatID)
    case "back_to_menu":
        t.showUserMenu(chatID)
    case "start_add_service":
        t.bot.Send(tgbotapi.NewMessage(chatID, "لطفاً پیام 'شروع افزودن سرویس' را ارسال کنید."))
    case "delete_service":
        t.showServicesForAdminDelete(chatID)
    case "add_panel":
        adminPanelState.Step = 1
        adminPanelState.Cookie = ""
        adminPanelState.Link = ""
        adminPanelState.ID = ""
        t.bot.Send(tgbotapi.NewMessage(chatID, "🍪 لطفاً کوکی پنل را وارد کنید:"))
    case "broadcast":
        adminBroadcastState.Waiting = true
        t.bot.Send(tgbotapi.NewMessage(chatID, "✏️ لطفاً پیام مورد نظر برای ارسال به همه کاربران را وارد کنید:"))
    case "back_to_admin_panel":
        t.showAdminMenu(chatID)
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
    msg := fmt.Sprintf("👤 اطلاعات حساب شما:\n\n📌 نام: %s\n💰 موجودی: %d تومان", 
        t.users[userID], balance)
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
    rows, err := db.Query("SELECT description, price, created_at FROM subscriptions WHERE user_id = ? ORDER BY created_at DESC", chatID)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت اشتراک‌ها."))
        return
    }
    defer rows.Close()
    var result string
    var count int
    for rows.Next() {
        var desc string
        var price int
        var created string
        rows.Scan(&desc, &price, &created)
        count++
        result += fmt.Sprintf("%d. %s\n💰 قیمت: %d تومان\n📅 خرید: %s\n\n", count, desc, price, created)
    }
    if count == 0 {
        result = "📦 شما هیچ اشتراک فعالی ندارید."
    }
    msg := tgbotapi.NewMessage(chatID, result)
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
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

// نمایش اطلاعات تمام کاربران
func (t *TelegramBot) showAllUsersInfo(chatID int64) {
    var info string
    for uid, name := range t.users {
        info += fmt.Sprintf("👤 %s (%d): %d تومان\n", name, uid, t.balances[uid])
    }
    if info == "" {
        info = "⚠️ هیچ کاربری ثبت نشده."
    }
    t.bot.Send(tgbotapi.NewMessage(chatID, info))
}

// نمایش سرویس‌ها به کاربر
func (t *TelegramBot) showServicesForUser(chatID int64, userID int64) {
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
        t.bot.Send(tgbotapi.NewMessage(chatID, "🛒 فعلاً پلنی برای خرید فعال نیست!"))
        return
    }
    // ساخت دکمه‌ها
    var btns [][]tgbotapi.InlineKeyboardButton
    for _, s := range services {
        btns = append(btns, tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData(s.Desc, fmt.Sprintf("service_%d", s.ID)),
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
    var desc string
    err = db.QueryRow("SELECT price, description FROM services WHERE id = ?", serviceID).Scan(&price, &desc)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در دریافت اطلاعات سرویس یا سرویس حذف شده است."))
        return
    }
    
    // بررسی وجود تنظیمات پنل
    var panelExists bool
    err = db.QueryRow("SELECT COUNT(*) FROM panel_settings").Scan(&panelExists)
    if err != nil || !panelExists {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ تنظیمات پنل یافت نشد. لطفاً با ادمین تماس بگیرید."))
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
    // کم کردن مبلغ سرویس از موجودی کاربر
    t.balances[userID] -= price
    // ثبت تراکنش منفی در دیتابیس
    db.Exec("INSERT INTO charges (user_id, amount) VALUES (?, ?)", userID, -price)
    // ثبت اشتراک جدید برای کاربر
    db.Exec("INSERT INTO subscriptions (user_id, service_id, description, price) VALUES (?, ?, ?, ?)", userID, serviceID, desc, price)
    // استخراج تعداد ماه و گیگ از description (مثلاً "3 ماهه 100 گیگ")
    months, gb := extractMonthAndGB(desc)
    // فراخوانی تابع addClient برای تولید لینک
    link, err := addClient(gb, months)
    if err != nil {
        t.bot.Send(tgbotapi.NewMessage(chatID, "❌ خطا در تولید لینک: "+err.Error()))
        return
    }
    // ارسال لینک به کاربر
    msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("✅ سرویس با موفقیت فعال شد!\n\n🔗 لینک کانفیگ شما:\n`%s`", link))
    msg.ParseMode = "Markdown"
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
}

// تابع تولید لینک از پنل
func addClient(gb int, days int) (string, error) {
    // دریافت تنظیمات پنل از دیتابیس
    var cookie, link, panelID string
    err := db.QueryRow("SELECT cookie, link, panel_id FROM panel_settings ORDER BY id DESC LIMIT 1").Scan(&cookie, &link, &panelID)
    if err != nil {
        return "", fmt.Errorf("تنظیمات پنل یافت نشد. لطفاً ابتدا پنل را تنظیم کنید")
    }
    
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
    data.Set("id", panelID)
    data.Set("settings", string(settingsJson))

    req, err := http.NewRequest("POST", link+"/panel/inbound/addClient", strings.NewReader(data.Encode()))
    if err != nil {
        return "", err
    }

    req.Header.Set("Accept", "application/json, text/plain, */*")
    req.Header.Set("Accept-Language", "en-US,en;q=0.9,fa-IR;q=0.8,fa;q=0.7")
    req.Header.Set("Connection", "keep-alive")
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
    req.Header.Set("Cookie", cookie)
    req.Header.Set("Origin", link)
    req.Header.Set("Referer", link+"/panel/inbounds")
    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
    req.Header.Set("X-Requested-With", "XMLHttpRequest")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    if resp.StatusCode != 200 {
        return "", fmt.Errorf("Non-200 response: %s", string(body))
    }

    if strings.Contains(string(body), `"success":true`) {
        // generate config link - extract domain from panel link
        domain := extractDomainFromLink(link)
        config := fmt.Sprintf("vless://%s@%s:8080?type=ws&path=%%2F&host=&security=none#%%40aliping_shop%%20%%7C-%s", clientID, domain, email)
        return config, nil
    }

    return "", fmt.Errorf("Request failed: %s", string(body))
}

// تابع استخراج دامنه از لینک پنل
func extractDomainFromLink(link string) string {
    // حذف پروتکل
    if strings.HasPrefix(link, "http://") {
        link = strings.TrimPrefix(link, "http://")
    } else if strings.HasPrefix(link, "https://") {
        link = strings.TrimPrefix(link, "https://")
    }
    
    // حذف مسیر اضافی
    if strings.Contains(link, "/") {
        link = strings.Split(link, "/")[0]
    }
    
    return link
}

// تابع تولید ایمیل تصادفی
func randomEmail(length int) string {
    const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
    rand.Seed(time.Now().UnixNano())

    result := make([]byte, length)
    for i := range result {
        result[i] = charset[rand.Intn(len(charset))]
    }
    return string(result)
}

// تابع استخراج تعداد ماه و گیگ از description
func extractMonthAndGB(desc string) (int, int) {
    // مثال ورودی: "3 ماهه 100 گیگ"
    var months, gb int
    // جستجو برای عدد ماه
    for _, word := range strings.Fields(desc) {
        if strings.Contains(word, "ماه") {
            fmt.Sscanf(word, "%d", &months)
        }
        if strings.Contains(word, "گیگ") {
            fmt.Sscanf(word, "%d", &gb)
        }
    }
    if months == 0 {
        months = 1 // پیش‌فرض یک ماه
    }
    if gb == 0 {
        gb = 10 // پیش‌فرض 10 گیگ
    }
    return months, gb
}

// تایید فیش توسط ادمین با receiptID
func (t *TelegramBot) approveReceiptByID(receiptID string, callback *tgbotapi.CallbackQuery) {
    receipt, ok := pendingReceipts[receiptID]
    if !ok {
        t.bot.Send(tgbotapi.NewMessage(t.adminID, "❌ این فیش قبلاً بررسی شده یا وجود ندارد."))
        return
    }
    t.balances[receipt.UserID] += receipt.Amount
    // اطلاع‌رسانی به کاربر
    msg := tgbotapi.NewMessage(receipt.UserID, 
        fmt.Sprintf("✅ فیش شما تأیید شد و %d تومان به حساب‌تان افزوده شد.", receipt.Amount))
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
    t.bot.Send(tgbotapi.NewMessage(t.adminID, 
        fmt.Sprintf("🟢 فیش کاربر %d تأیید شد و %d تومان شارژ شد.", receipt.UserID, receipt.Amount)))
    // حذف فیش از map
    delete(pendingReceipts, receiptID)
}

// رد فیش توسط ادمین با receiptID
func (t *TelegramBot) rejectReceiptByID(receiptID string, callback *tgbotapi.CallbackQuery) {
    receipt, ok := pendingReceipts[receiptID]
    if !ok {
        t.bot.Send(tgbotapi.NewMessage(t.adminID, "❌ این فیش قبلاً بررسی شده یا وجود ندارد."))
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
    t.bot.Send(tgbotapi.NewMessage(t.adminID, 
        fmt.Sprintf("🔴 فیش کاربر %d رد شد.", receipt.UserID)))
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


			

