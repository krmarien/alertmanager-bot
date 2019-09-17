package telegram

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/hako/durafmt"
	"github.com/metalmatze/alertmanager-bot/pkg/alertmanager"
	"github.com/oklog/run"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/tucnak/telebot.v2"
)

const (
	commandStart = "/start"
	commandStop  = "/stop"
	commandHelp  = "/help"
	commandChats = "/chats"

	commandStatus     = "/status"
	commandAlerts     = "/alerts"
	commandSilences   = "/silences"
	commandSilenceAdd = "/silence_add"
	commandSilence    = "/silence"
	commandSilenceDel = "/silence_del"

	responseStart = "Hey, %s! I will now keep you up to date!\n" + commandHelp
	responseStop  = "Alright, %s! I won't talk to you again.\n" + commandHelp
	responseHelp  = `
I'm a Prometheus AlertManager Bot for Telegram. I will notify you about alerts.
You can also ask me about my ` + commandStatus + `, ` + commandAlerts + ` & ` + commandSilences + `

Available commands:
` + commandStart + ` - Subscribe for alerts.
` + commandStop + ` - Unsubscribe for alerts.
` + commandStatus + ` - Print the current status.
` + commandAlerts + ` - List all alerts.
` + commandSilences + ` - List all silences.
` + commandChats + ` - List all users and group chats that subscribed.
`
)

// BotChatStore is all the Bot needs to store and read
type BotChatStore interface {
	List() ([]*telebot.Chat, error)
	Add(*telebot.Chat) error
	Remove(*telebot.Chat) error
}

// Bot runs the alertmanager telegram
type Bot struct {
	addr                string
	admins              []int // must be kept sorted
	alertmanager        *url.URL
	templates           *template.Template
	chats               BotChatStore
	logger              log.Logger
	revision            string
	startTime           time.Time
	disableNotification bool

	telegram *telebot.Bot

	commandsCounter *prometheus.CounterVec
	webhooksCounter prometheus.Counter
}

// BotOption passed to NewBot to change the default instance
type BotOption func(b *Bot)

// NewBot creates a Bot with the UserStore and telegram telegram
func NewBot(chats BotChatStore, token string, admin int, disableNotification bool, opts ...BotOption) (*Bot, error) {
	bot, err := telebot.NewBot(telebot.Settings{
		Token: token,
		Poller: &telebot.LongPoller{Timeout: 15 * time.Second},
	})

	if err != nil {
		return nil, err
	}

	commandsCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "alertmanagerbot",
		Name:      "commands_total",
		Help:      "Number of commands received by command name",
	}, []string{"command"})
	if err := prometheus.Register(commandsCounter); err != nil {
		return nil, err
	}

	b := &Bot{
		logger:             log.NewNopLogger(),
		telegram:           bot,
		chats:              chats,
		addr:               "127.0.0.1:8080",
		admins:             []int{admin},
		alertmanager:       &url.URL{Host: "localhost:9093"},
		commandsCounter:    commandsCounter,
		disableNotification:disableNotification,
		// TODO: initialize templates with default?
	}

	for _, opt := range opts {
		opt(b)
	}

	return b, nil
}

// WithLogger sets the logger for the Bot as an option
func WithLogger(l log.Logger) BotOption {
	return func(b *Bot) {
		b.logger = l
	}
}

// WithAddr sets the internal listening addr of the bot's web server receiving webhooks
func WithAddr(addr string) BotOption {
	return func(b *Bot) {
		b.addr = addr
	}
}

// WithAlertmanager sets the connection url for the Alertmanager
func WithAlertmanager(u *url.URL) BotOption {
	return func(b *Bot) {
		b.alertmanager = u
	}
}

// WithTemplates uses Alertmanager template to render messages for Telegram
func WithTemplates(t *template.Template) BotOption {
	return func(b *Bot) {
		b.templates = t
	}
}

// WithRevision is setting the Bot's revision for status commands
func WithRevision(r string) BotOption {
	return func(b *Bot) {
		b.revision = r
	}
}

// WithStartTime is setting the Bot's start time for status commands
func WithStartTime(st time.Time) BotOption {
	return func(b *Bot) {
		b.startTime = st
	}
}

// WithExtraAdmins allows the specified additional user IDs to issue admin
// commands to the bot.
func WithExtraAdmins(ids ...int) BotOption {
	return func(b *Bot) {
		b.admins = append(b.admins, ids...)
		sort.Ints(b.admins)
	}
}

// SendAdminMessage to the admin's ID with a message
func (b *Bot) SendAdminMessage(adminID int, message string) {
	to := telebot.User{ID: adminID}
	b.telegram.Send(&to, message)
}

// isAdminID returns whether id is one of the configured admin IDs.
func (b *Bot) isAdminID(id int) bool {
	i := sort.SearchInts(b.admins, id)
	return i < len(b.admins) && b.admins[i] == id
}

// Run the telegram and listen to messages send to the telegram
func (b *Bot) Run(ctx context.Context, webhooks <-chan notify.WebhookMessage) error {
	commandSuffix := fmt.Sprintf("@%s", b.telegram.Me.Username)

	commands := map[string]func(message *telebot.Message){
		commandStart:    b.handleStart,
		commandStop:     b.handleStop,
		commandHelp:     b.handleHelp,
		commandChats:    b.handleChats,
		commandStatus:   b.handleStatus,
		commandAlerts:   b.handleAlerts,
		commandSilences: b.handleSilences,
	}

	// init counters with 0
	for command := range commands {
		b.commandsCounter.WithLabelValues(command).Add(0)
	}

	process := func(message *telebot.Message) {
		if message.IsService() {
			return
		}

		if !b.isAdminID(message.Sender.ID) {
			b.commandsCounter.WithLabelValues("dropped").Inc()
			level.Error(b.logger).Log("msg", "dropped message from forbidden sender", "err", "dropped message from forbidden sender")
		}

		if err := b.telegram.Notify(message.Chat, telebot.Typing); err != nil {
			level.Error(b.logger).Log("msg", "Failed to notify user", "err", err)
		}

		// Remove the command suffix from the text, /help@BotName => /help
		text := strings.Replace(message.Text, commandSuffix, "", -1)
		// Only take the first part into account, /help foo => /help
		text = strings.Split(text, " ")[0]

		level.Debug(b.logger).Log("msg", "message received", "text", text)

		// Get the corresponding handler from the map by the commands text
		handler, ok := commands[text]

		if !ok {
			b.commandsCounter.WithLabelValues("incomprehensible").Inc()
			b.telegram.Send(
				message.Chat,
				"Sorry, I don't understand...",
			)
			return
		}

		b.commandsCounter.WithLabelValues(text).Inc()
		handler(message)

		return
	}

	for command,_ := range commands {
		b.telegram.Handle(command, process)
	}

	var gr run.Group
	{
		gr.Add(func() error {
			return b.sendWebhook(ctx, webhooks)
		}, func(err error) {
		})
		gr.Add(func() error {
			b.telegram.Start()
			return nil
		}, func(err error) {
		})
	}

	return gr.Run()
}

// sendWebhook sends messages received via webhook to all subscribed chats
func (b *Bot) sendWebhook(ctx context.Context, webhooks <-chan notify.WebhookMessage) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case w := <-webhooks:
			chats, err := b.chats.List()
			if err != nil {
				level.Error(b.logger).Log("msg", "failed to get chat list from store", "err", err)
				continue
			}

			data := &template.Data{
				Receiver:          w.Receiver,
				Status:            w.Status,
				Alerts:            w.Alerts,
				GroupLabels:       w.GroupLabels,
				CommonLabels:      w.CommonLabels,
				CommonAnnotations: w.CommonAnnotations,
				ExternalURL:       w.ExternalURL,
			}

			out, err := b.templates.ExecuteHTMLString(`{{ template "telegram.default" . }}`, data)
			if err != nil {
				level.Warn(b.logger).Log("msg", "failed to template alerts", "err", err)
				continue
			}

			for _, chat := range chats {
				_, err = b.telegram.Send(chat, b.truncateMessage(out), &telebot.SendOptions{ParseMode: telebot.ModeHTML, DisableNotification: b.disableNotification})
				if err != nil {
					level.Warn(b.logger).Log("msg", "failed to send message to subscribed chat", "err", err)
				}
			}
		}
	}
}

func (b *Bot) handleStart(message *telebot.Message) {
	if err := b.chats.Add(message.Chat); err != nil {
		level.Warn(b.logger).Log("msg", "failed to add chat to chat store", "err", err)
		b.telegram.Send(message.Chat, "I can't add this chat to the subscribers list.")
		return
	}

	statusBtn := telebot.ReplyButton{
		Text: commandStatus,
	}
	alertsBtn := telebot.ReplyButton{
		Text: commandAlerts,
	}
	silencesBtn := telebot.ReplyButton{
		Text: commandSilences,
	}
	helpBtn := telebot.ReplyButton{
		Text: commandHelp,
	}
	replyKeys := [][]telebot.ReplyButton{
		[]telebot.ReplyButton{statusBtn},
		[]telebot.ReplyButton{alertsBtn},
		[]telebot.ReplyButton{silencesBtn},
		[]telebot.ReplyButton{helpBtn},
	}

	b.telegram.Send(message.Chat, fmt.Sprintf(responseStart, message.Sender.FirstName), &telebot.ReplyMarkup{ReplyKeyboard: replyKeys})
	level.Info(b.logger).Log(
		"user subscribed",
		"username", message.Sender.Username,
		"user_id", message.Sender.ID,
	)
}

func (b *Bot) handleStop(message *telebot.Message) {
	if err := b.chats.Remove(message.Chat); err != nil {
		level.Warn(b.logger).Log("msg", "failed to remove chat from chat store", "err", err)
		b.telegram.Send(message.Chat, "I can't remove this chat from the subscribers list.")
		return
	}

	b.telegram.Send(message.Chat, fmt.Sprintf(responseStop, message.Sender.FirstName))
	level.Info(b.logger).Log(
		"user unsubscribed",
		"username", message.Sender.Username,
		"user_id", message.Sender.ID,
	)
}

func (b *Bot) handleHelp(message *telebot.Message) {
	b.telegram.Send(message.Chat, responseHelp)
}

func (b *Bot) handleChats(message *telebot.Message) {
	chats, err := b.chats.List()
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to list chats from chat store", "err", err)
		b.telegram.Send(message.Chat, "I can't list the subscribed chats.")
		return
	}

	list := ""
	for _, chat := range chats {
		level.Info(b.logger).Log("msg", fmt.Sprintf("%+v", chat))
		if chat.Type != telebot.ChatPrivate {
			list = list + fmt.Sprintf("@%s\n", chat.Title)
		} else {
			list = list + fmt.Sprintf("@%s\n", chat.Username)
		}
	}

	b.telegram.Send(message.Chat, "Currently these chat have subscribed:\n"+list)
}

func (b *Bot) handleStatus(message *telebot.Message) {
	s, err := alertmanager.Status(b.logger, b.alertmanager.String())
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to get status", "err", err)
		b.telegram.Send(message.Chat, fmt.Sprintf("failed to get status... %v", err))
		return
	}

	uptime := durafmt.Parse(time.Since(s.Data.Uptime))
	uptimeBot := durafmt.Parse(time.Since(b.startTime))

	b.telegram.Send(
		message.Chat,
		fmt.Sprintf(
			"*AlertManager*\nVersion: %s\nUptime: %s\n*AlertManager Bot*\nVersion: %s\nUptime: %s",
			s.Data.VersionInfo.Version,
			uptime,
			b.revision,
			uptimeBot,
		),
		&telebot.SendOptions{ParseMode: telebot.ModeMarkdown},
	)
}

func (b *Bot) handleAlerts(message *telebot.Message) {
	alerts, err := alertmanager.ListAlerts(b.logger, b.alertmanager.String())
	if err != nil {
		b.telegram.Send(message.Chat, fmt.Sprintf("failed to list alerts... %v", err))
		return
	}

	if len(alerts) == 0 {
		b.telegram.Send(message.Chat, "No alerts right now! yes 🎉")
		return
	}

	out, err := b.tmplAlerts(alerts...)
	if err != nil {
		return
	}

	_, err = b.telegram.Send(message.Chat, b.truncateMessage(out), &telebot.SendOptions{
		ParseMode: telebot.ModeHTML,
	})
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to send message", "err", err)
	}
}

func (b *Bot) handleSilences(message *telebot.Message) {
	silences, err := alertmanager.ListSilences(b.logger, b.alertmanager.String())
	if err != nil {
		b.telegram.Send(message.Chat, fmt.Sprintf("failed to list silences... %v", err))
		return
	}

	if len(silences) == 0 {
		b.telegram.Send(message.Chat, "No silences right now.")
		return
	}

	var out string
	for _, silence := range silences {
		out = out + alertmanager.SilenceMessage(silence) + "\n"
	}

	b.telegram.Send(message.Chat, out, &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
}

func (b *Bot) tmplAlerts(alerts ...*types.Alert) (string, error) {
	data := b.templates.Data("default", nil, alerts...)

	out, err := b.templates.ExecuteHTMLString(`{{ template "telegram.default" . }}`, data)
	if err != nil {
		return "", err
	}

	return out, nil
}

// Truncate very big message
func (b *Bot) truncateMessage(str string) string {
	truncateMsg := str
	if len(str) > 4095 { // telegram API can only support 4096 bytes per message
		level.Warn(b.logger).Log("msg", "Message is bigger than 4095, truncate...")
		// find the end of last alert, we do not want break the html tags
		i := strings.LastIndex(str[0:4080], "\n\n") // 4080 + "\n<b>[SNIP]</b>" == 4095
		if i > 1 {
			truncateMsg = str[0:i] + "\n<b>[SNIP]</b>"
		} else {
			truncateMsg = "Message is too long... can't send.."
			level.Warn(b.logger).Log("msg", "truncateMessage: Unable to find the end of last alert.")
		}
		return truncateMsg
	}
	return truncateMsg
}
