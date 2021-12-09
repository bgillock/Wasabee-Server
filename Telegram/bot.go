package wasabeetelegram

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/go-telegram-bot-api/telegram-bot-api"

	"github.com/wasabee-project/Wasabee-Server/config"
	"github.com/wasabee-project/Wasabee-Server/generatename"
	"github.com/wasabee-project/Wasabee-Server/log"
	"github.com/wasabee-project/Wasabee-Server/messaging"
	"github.com/wasabee-project/Wasabee-Server/model"
	"github.com/wasabee-project/Wasabee-Server/rocks"
	"github.com/wasabee-project/Wasabee-Server/templates"
	"github.com/wasabee-project/Wasabee-Server/v"
)

// TGConfiguration is the main configuration data for the Telegram interface
// passed to main() pre-loaded with APIKey and TemplateSet set, the rest is built when the bot starts
type TGConfiguration struct {
	APIKey      string
	HookPath    string
	TemplateSet map[string]*template.Template
	baseKbd     tgbotapi.ReplyKeyboardMarkup
	upChan      chan tgbotapi.Update
	hook        string
}

var bot *tgbotapi.BotAPI
var c TGConfiguration

// WasabeeBot is called from main() to start the bot.
func WasabeeBot(in TGConfiguration) {
	if in.APIKey == "" {
		log.Infow("startup", "subsystem", "Telegram", "message", "Telegram API key not set; not starting")
		return
	}
	c.APIKey = in.APIKey

	if in.TemplateSet == nil {
		log.Warnw("startup", "subsystem", "Telegram", "message", "the UI templates are not loaded; not starting Telegram bot")
		return
	}
	c.TemplateSet = in.TemplateSet

	keyboards(&c)

	c.HookPath = in.HookPath
	if c.HookPath == "" {
		c.HookPath = "/tg"
	}

	c.upChan = make(chan tgbotapi.Update, 10) // not using bot.ListenForWebhook() since we need our own bidirectional channel
	webhook := config.Subrouter(c.HookPath)
	webhook.HandleFunc("/{hook}", TGWebHook).Methods("POST")

	b := messaging.Bus{
		SendMessage:      SendMessage,
		SendTarget:       SendTarget,
		AddToRemote:      AddToChat,
		RemoveFromRemote: RemoveFromChat,
	}

	messaging.RegisterMessageBus("Telegram", b)

	var err error
	bot, err = tgbotapi.NewBotAPI(c.APIKey)
	if err != nil {
		log.Error(err)
		return
	}

	// bot.Debug = true
	log.Infow("startup", "subsystem", "Telegram", "message", "authorized to Telegram as "+bot.Self.UserName)
	config.TGSetBot(bot.Self.UserName, bot.Self.ID)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	webroot := config.GetWebroot()
	c.hook = generatename.GenerateName()
	t := fmt.Sprintf("%s%s/%s", webroot, c.HookPath, c.hook)
	if _, err = bot.SetWebhook(tgbotapi.NewWebhook(t)); err != nil {
		log.Error(err)
		return
	}

	i := 1
	for update := range c.upChan {
		// log.Debugf("running update: %s", update)
		if err = runUpdate(update); err != nil {
			log.Error(err)
			continue
		}
		if (i % 100) == 0 { // every 100 requests, change the endpoint; I'm _not_ paranoid.
			i = 1
			c.hook = generatename.GenerateName()
			t = fmt.Sprintf("%s%s/%s", webroot, c.HookPath, c.hook)
			_, err = bot.SetWebhook(tgbotapi.NewWebhook(t))
			if err != nil {
				log.Error(err)
			}
		}
		i++
	}
}

// Shutdown closes all the Telegram connections
// called only at server shutdown
func Shutdown() {
	log.Infow("shutdown", "subsystem", "Telegram", "message", "shutdown telegram")
	_, _ = bot.RemoveWebhook()
	bot.StopReceivingUpdates()
}

func runUpdate(update tgbotapi.Update) error {
	if update.CallbackQuery != nil {
		log.Debugw("callback", "subsystem", "Telegram", "data", update)
		msg, err := callback(&update)
		if err != nil {
			log.Error(err)
			return err
		}
		if _, err = bot.Send(msg); err != nil {
			log.Error(err)
			return err
		}
		if _, err = bot.DeleteMessage(tgbotapi.NewDeleteMessage(update.CallbackQuery.Message.Chat.ID, update.CallbackQuery.Message.MessageID)); err != nil {
			log.Error(err)
			return err
		}
		return nil
	}

	if update.Message != nil {
		if update.Message.Chat.Type == "private" {
			if err := processDirectMessage(&update); err != nil {
				log.Error(err)
			}
		} else {
			if err := processChatMessage(&update); err != nil {
				log.Error(err)
			}
		}
	}

	if update.EditedMessage != nil && update.EditedMessage.Location != nil {
		if err := liveLocationUpdate(&update); err != nil {
			log.Error(err)
		}
	}

	return nil
}

func newUserInit(msg *tgbotapi.MessageConfig, inMsg *tgbotapi.Update) error {
	var ott model.OneTimeToken
	if inMsg.Message.IsCommand() {
		tokens := strings.Split(inMsg.Message.Text, " ")
		if len(tokens) == 2 {
			ott = model.OneTimeToken(strings.TrimSpace(tokens[1]))
		}
	} else {
		ott = model.OneTimeToken(strings.TrimSpace(inMsg.Message.Text))
	}

	log.Debugw("newUserInit", "text", inMsg.Message.Text)

	tid := model.TelegramID(inMsg.Message.From.ID)
	err := tid.InitAgent(inMsg.Message.From.UserName, ott)
	if err != nil {
		log.Error(err)
		tmp, _ := templateExecute("InitOneFail", inMsg.Message.From.LanguageCode, nil)
		msg.Text = tmp
	} else {
		tmp, _ := templateExecute("InitOneSuccess", inMsg.Message.From.LanguageCode, nil)
		msg.Text = tmp
	}
	return err
}

func newUserVerify(msg *tgbotapi.MessageConfig, inMsg *tgbotapi.Update) error {
	var authtoken string
	if inMsg.Message.IsCommand() {
		tokens := strings.Split(inMsg.Message.Text, " ")
		if len(tokens) == 2 {
			authtoken = tokens[1]
		}
	} else {
		authtoken = inMsg.Message.Text
	}
	authtoken = strings.TrimSpace(authtoken)
	tid := model.TelegramID(inMsg.Message.From.ID)
	err := tid.VerifyAgent(authtoken)
	if err != nil {
		log.Error(err)
		tmp, _ := templateExecute("InitTwoFail", inMsg.Message.From.LanguageCode, nil)
		msg.Text = tmp
	} else {
		tmp, _ := templateExecute("InitTwoSuccess", inMsg.Message.From.LanguageCode, nil)
		msg.Text = tmp
	}
	return err
}

func keyboards(c *TGConfiguration) {
	c.baseKbd = tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonLocation("Send Location"),
			tgbotapi.NewKeyboardButton("Teams"),
		),
	)
}

// SendMessage is registered with Wasabee-Server as a message bus to allow other modules to send messages via Telegram
func SendMessage(g messaging.GoogleID, message string) (bool, error) {
	gid := model.GoogleID(g)
	tgid, err := gid.TelegramID()
	if err != nil {
		log.Error(err)
		return false, err
	}
	tgid64 := int64(tgid)
	if tgid64 == 0 {
		log.Debugw("TelegramID not found", "subsystem", "Telegram", "GID", gid)
		return false, nil
	}
	msg := tgbotapi.NewMessage(tgid64, "")
	msg.Text = message
	msg.ParseMode = "HTML"

	_, err = bot.Send(msg)
	if err != nil && err.Error() != "Bad Request: chat not found" {
		log.Error(err)
		return false, err
	}
	if err != nil && err.Error() == "Bad Request: chat not found" {
		log.Debugw(err.Error(), "gid", gid, "tgid", tgid)
		return false, nil
	}

	log.Debugw("sent message", "subsystem", "Telegram", "GID", gid)
	return true, nil
}

func SendTarget(g messaging.GoogleID, target messaging.Target) error {
	gid := model.GoogleID(g)
	tgid, err := gid.TelegramID()
	if err != nil {
		log.Error(err)
		return err
	}
	tgid64 := int64(tgid)
	if tgid64 == 0 {
		log.Debugw("TelegramID not found", "subsystem", "Telegram", "GID", gid)
		return nil
	}
	msg := tgbotapi.NewMessage(tgid64, "")
	msg.ParseMode = "HTML"

	// Lng vs Lon ...
	templateData := struct {
		Name   string
		ID     string
		Lat    string
		Lon    string
		Type   string
		Sender string
	}{
		Name:   target.Name,
		ID:     target.ID,
		Lat:    target.Lat,
		Lon:    target.Lng,
		Type:   target.Type,
		Sender: target.Name,
	}

	msg.Text, err = templates.Execute("target", templateData)
	if err != nil {
		log.Error(err)
		msg.Text = fmt.Sprintf("template failed; target @ %s %s", target.Lat, target.Lng)
	}

	_, err = bot.Send(msg)
	if err != nil && err.Error() != "Bad Request: chat not found" {
		log.Error(err)
		return err
	}
	if err != nil && err.Error() == "Bad Request: chat not found" {
		log.Debugw(err.Error(), "gid", gid, "tgid", tgid)
		return err
	}

	log.Debugw("sent target", "subsystem", "Telegram", "GID", gid, "target", target)
	return nil
}

// checks rocks/v based on tgid, Inits agent if found
func firstlogin(tgid model.TelegramID, name string) (model.GoogleID, error) {
	agent, err := rocks.Search(fmt.Sprint(tgid))
	if err != nil {
		log.Error(err)
		return "", err
	}
	if agent.Gid != "" {
		gid := model.GoogleID(agent.Gid)
		if !gid.Valid() {
			if err := gid.FirstLogin(); err != nil {
				log.Error(err)
				return "", err
			}
		}
		if err := gid.SetTelegramID(tgid, name); err != nil {
			log.Error(err)
			return gid, err
		}
		// rocks success
		return gid, nil
	}

	result, err := v.TelegramSearch(tgid)
	if err != nil {
		log.Error(err)
		return "", err
	}
	if result.Gid != "" {
		log.Debugw("v is fucking useless")
		result.Gid, _ = model.GetGIDFromEnlID(result.EnlID)
	}

	if result.Gid != "" {
		gid := model.GoogleID(result.Gid)
		if !gid.Valid() {
			if err := gid.FirstLogin(); err != nil {
				log.Error(err)
				return "", err
			}
		}
		if err := gid.SetTelegramID(tgid, name); err != nil {
			log.Error(err)
			return gid, err
		}
		// v success
		return gid, nil
	}

	// not found in either service
	return "", nil
}
