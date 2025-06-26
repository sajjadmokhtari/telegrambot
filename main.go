package main

import (
    "fmt"
    "log"
    "strconv"
    "strings"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
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
    adminID       int64
}

const (
    adminID int64 = 1150702474 // 👈 آیدی عددی خودت رو وارد کن
    botToken      = "8024742298:AAHP1jBKaTMk9j0ophnn83pQvdBft5yAZwU" // 👈 توکن واقعی رباتت اینجا
    cardNumber    = "5859-8312-4246-5762"
    cardHolder    = "علی اسماعیلی"
)

// تابع اصلی
func main() {
    bot := NewTelegramBot()
    bot.Start()
}

// ایجاد نمونه جدید ربات
func NewTelegramBot() *TelegramBot {
    bot, err := tgbotapi.NewBotAPI(botToken)
    if err != nil {
        log.Panic(err)
    }

    return &TelegramBot{
        bot:        bot,
        balances:   make(map[int64]int),
        users:      make(map[int64]string),
        userStates: make(map[int64]*UserState),
        adminID:    adminID,
    }
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
    // chatID := message.Chat.ID
    userID := int64(message.From.ID)
    
    // ثبت اطلاعات کاربر
    t.registerUser(userID, message.From.FirstName+" "+message.From.LastName)

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

    // ارسال فیش برای بررسی ادمین
    t.sendReceiptToAdmin(message, userID, amount)
}

// ارسال فیش برای بررسی ادمین
func (t *TelegramBot) sendReceiptToAdmin(message *tgbotapi.Message, userID int64, amount int) {
    photos := *message.Photo
    lastPhoto := photos[len(photos)-1]

    caption := fmt.Sprintf("🧾 فیش جدید:\n👤 %s (%d)\n💰 مبلغ: %d تومان", 
        t.users[userID], userID, amount)

    adminMsg := tgbotapi.NewPhotoShare(t.adminID, lastPhoto.FileID)
    adminMsg.Caption = caption
    adminMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("✅ تایید", fmt.Sprintf("approve_%d", userID)),
            tgbotapi.NewInlineKeyboardButtonData("❌ رد", fmt.Sprintf("reject_%d", userID)),
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
        t.showSubscriptionInfo(chatID)
    case "my_subscriptions":
        t.showMySubscriptions(chatID)
    case "tutorials":
        t.showTutorials(chatID)
    case "manual_charge":
        t.showManualChargeFormat(chatID)
    case "user_info":
        t.showAllUsersInfo(chatID)
    case "back_to_menu":
        t.showUserMenu(chatID)
    default:
        // بررسی تایید یا رد فیش
        if strings.HasPrefix(data, "approve_") {
            t.approveReceipt(data, callback)
        } else if strings.HasPrefix(data, "reject_") {
            t.rejectReceipt(data, callback)
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

// نمایش فرمت شارژ دستی
func (t *TelegramBot) showManualChargeFormat(chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "✏️ فرمت شارژ دستی:\n\n`شارژ <UserID> <مبلغ>`")
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

// تایید فیش توسط ادمین
func (t *TelegramBot) approveReceipt(data string, callback *tgbotapi.CallbackQuery) {
    uidStr := strings.TrimPrefix(data, "approve_")
    uid, _ := strconv.ParseInt(uidStr, 10, 64)
    
    state := t.userStates[uid]
    amount := state.PendingAmount
    
    if amount > 0 {
        t.balances[uid] += amount
        state.PendingAmount = 0

        // اطلاع‌رسانی به کاربر با دکمه بازگشت به منو
        msg := tgbotapi.NewMessage(uid, 
            fmt.Sprintf("✅ فیش شما تأیید شد و %d تومان به حساب‌تان افزوده شد.", amount))
        msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
            tgbotapi.NewInlineKeyboardRow(
                tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
            ),
        )
        t.bot.Send(msg)
        
        // ویرایش پیام ادمین و حذف دکمه‌ها
        editMsg := tgbotapi.NewEditMessageReplyMarkup(callback.Message.Chat.ID, 
            int(callback.Message.MessageID), 
            tgbotapi.NewInlineKeyboardMarkup())
        t.bot.Send(editMsg)
        
        // اطلاع‌رسانی به ادمین
        t.bot.Send(tgbotapi.NewMessage(t.adminID, 
            fmt.Sprintf("🟢 فیش کاربر %d تأیید شد و %d تومان شارژ شد.", uid, amount)))
    }
}

// رد فیش توسط ادمین
func (t *TelegramBot) rejectReceipt(data string, callback *tgbotapi.CallbackQuery) {
    uidStr := strings.TrimPrefix(data, "reject_")
    uid, _ := strconv.ParseInt(uidStr, 10, 64)
    
    state := t.userStates[uid]
    state.PendingAmount = 0

    // اطلاع‌رسانی به کاربر با دکمه بازگشت به منو
    msg := tgbotapi.NewMessage(uid, 
        "❌ فیش شما رد شد. لطفاً بررسی کرده و مجدداً ارسال نمایید.")
    msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("🏠 بازگشت به منو", "back_to_menu"),
        ),
    )
    t.bot.Send(msg)
    
    // ویرایش پیام ادمین و حذف دکمه‌ها
    editMsg := tgbotapi.NewEditMessageReplyMarkup(callback.Message.Chat.ID, 
        int(callback.Message.MessageID), 
        tgbotapi.NewInlineKeyboardMarkup())
    t.bot.Send(editMsg)
    
    // اطلاع‌رسانی به ادمین
    t.bot.Send(tgbotapi.NewMessage(t.adminID, 
        fmt.Sprintf("🔴 فیش کاربر %d رد شد.", uid)))
}


			

