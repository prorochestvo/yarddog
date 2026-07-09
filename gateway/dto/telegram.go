package dto

// TelegramSendMessageRequest is the JSON body posted to Telegram's
// sendMessage endpoint (design §8.2). ChatID stays a string — it comes
// straight out of the manually parsed DSN, and the Bot API accepts either an
// integer or a string chat id.
type TelegramSendMessageRequest struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}
