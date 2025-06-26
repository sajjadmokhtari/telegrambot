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
    msg := tgbotapi.NewMessage(chatID, "📦 شما در حال حاضر اشتراک فعالی ندارید.")
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
    err = db.QueryRow("SELECT price FROM services WHERE id = ?", serviceID).Scan(&price)
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
    // اگر موجودی کافی بود، فعلاً فقط پیام موفقیت (در آینده منطق خرید اضافه می‌شود)
    t.bot.Send(tgbotapi.NewMessage(chatID, "✅ درخواست خرید سرویس ثبت شد. (در حال بررسی...)"))
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


			

