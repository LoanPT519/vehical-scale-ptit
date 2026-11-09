package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TelegramWorker kiểu outbox: quét cảnh báo (OVER, bật/tắt thủ công) chưa gửi trong SQLite, gửi theo thứ tự thời gian.
// Chống bị Telegram chặn (429) và gửi bù có kiểm soát:
//   - Mỗi tin cách nhau tối thiểu sendGap (kênh giới hạn ~20 tin/phút).
//   - 429: chờ đúng retry_after Telegram trả về, không tính là lỗi của sự kiện.
//   - Telegram từ chối (400/403…): lùi lần thử 30 s → 1 h, tối đa 10 lần.
//   - Mất mạng: giữ nguyên hàng đợi, chờ tăng dần 10 s → 5 phút rồi quét lại.
const (
	tgAPI         = "https://api.telegram.org/bot%s/%s"
	sendGap       = 3 * time.Second
	scanInterval  = 2 * time.Second
	retryBase     = 30 * time.Second
	retryMax      = time.Hour
	netBackoffMax = 5 * time.Minute
	lateNote      = 60 * time.Second // gửi trễ hơn mức này thì ghi chú "gửi bù"
)

type rateLimited struct{ after time.Duration }

func (e rateLimited) Error() string { return fmt.Sprintf("429, chờ %s", e.after) }

var errRejected = errors.New("telegram từ chối")

type sysMsg struct{ text, photo string }

type TelegramWorker struct {
	store    *Store
	cfg      *Config
	client   *http.Client
	lastSend time.Time
	mu       sync.Mutex
	sys      []sysMsg // tin hệ thống (khởi động…), giữ trong RAM tới khi gửi được
}

func newTelegram(store *Store, cfg *Config) *TelegramWorker {
	// Timeout tính cả DNS + upload ảnh qua mạng yếu
	return &TelegramWorker{store: store, cfg: cfg, client: &http.Client{Timeout: 60 * time.Second}}
}

// Notify xếp hàng 1 tin hệ thống (kèm ảnh nếu có); gửi trước cảnh báo, cùng cơ chế giãn cách / chờ mạng.
func (t *TelegramWorker) Notify(text, photo string) {
	t.mu.Lock()
	t.sys = append(t.sys, sysMsg{text, photo})
	t.mu.Unlock()
}

func (t *TelegramWorker) call(method string, fields map[string]string, photo string) error {
	if wait := time.Until(t.lastSend.Add(sendGap)); wait > 0 {
		time.Sleep(wait)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	if photo != "" {
		f, err := os.Open(photo)
		if err != nil {
			return err
		}
		part, _ := mw.CreateFormFile("photo", filepath.Base(photo))
		_, err = io.Copy(part, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	mw.Close()
	t.lastSend = time.Now()
	resp, err := t.client.Post(fmt.Sprintf(tgAPI, t.cfg.TgToken, method), mw.FormDataContentType(), &body)
	if err != nil { // lỗi mạng – thông báo lỗi của net/http chứa URL có token bot → che đi trước khi log
		return errors.New(strings.ReplaceAll(err.Error(), t.cfg.TgToken, "***"))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusTooManyRequests {
		var r struct {
			Parameters struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		after := 30
		if json.Unmarshal(raw, &r) == nil && r.Parameters.RetryAfter > 0 {
			after = r.Parameters.RetryAfter
		}
		return rateLimited{time.Duration(after) * time.Second}
	}
	if resp.StatusCode/100 != 2 {
		log.Printf("Telegram trả lỗi %d: %.200s", resp.StatusCode, raw)
		return errRejected
	}
	return nil
}

func (t *TelegramWorker) sendText(text, photo string) error {
	if photo != "" {
		if _, err := os.Stat(photo); err == nil {
			return t.call("sendPhoto", map[string]string{"chat_id": t.cfg.TgChatID, "caption": text, "parse_mode": "HTML"}, photo)
		}
	}
	return t.call("sendMessage", map[string]string{"chat_id": t.cfg.TgChatID, "text": text, "parse_mode": "HTML"}, "")
}

func fmtTs(ts time.Time) string { return ts.Format("2006-01-02 15:04:05") }

func lateLine(ts time.Time) []string {
	if late := time.Since(ts); late > lateNote {
		return []string{fmt.Sprintf("⏳ <i>Gửi bù, trễ %d phút</i>", int(late.Minutes()))}
	}
	return nil
}

func (t *TelegramWorker) overLines(ev *Event) []string {
	over, pct := ev.WeightKg-ev.ThresholdKg, 0.0
	if ev.ThresholdKg > 0 {
		pct = 100 * over / ev.ThresholdKg
	}
	return []string{
		fmt.Sprintf("🚨 <b>CẢNH BÁO QUÁ TẢI – %s</b>", html.EscapeString(strings.ToUpper(t.cfg.StationName))),
		"",
		fmt.Sprintf("⚖️ Khối lượng: <b>%.3f kg</b>", ev.WeightKg),
		fmt.Sprintf("🎯 Ngưỡng: <b>%.3f kg</b>", ev.ThresholdKg),
		fmt.Sprintf("📈 Vượt ngưỡng: <b>+%.3f kg (+%.0f%%)</b>", over, pct),
		fmt.Sprintf("🕒 Thời gian: <b>%s</b>", fmtTs(ev.Ts)),
		fmt.Sprintf("🆔 Mã sự kiện: <b>#%d</b>", ev.ID),
		"🚧 Xử lý: <b>🔒 Giữ cổng đóng</b>",
	}
}

func (t *TelegramWorker) manualLines(ev *Event) []string {
	st := html.EscapeString(strings.ToUpper(t.cfg.StationName))
	if ev.Status == "MANUAL_ON" {
		return []string{
			fmt.Sprintf("🕹 <b>BẬT CHẾ ĐỘ THỦ CÔNG – %s</b>", st),
			"",
			fmt.Sprintf("🕒 Thời gian: <b>%s</b>", fmtTs(ev.Ts)),
			fmt.Sprintf("⚖️ Cân lúc bật: <b>%.3f kg</b>", ev.WeightKg),
			fmt.Sprintf("🆔 Mã sự kiện: <b>#%d</b>", ev.ID),
			"⚠️ <i>Cân tự động tạm dừng – cổng điều khiển bằng joystick</i>",
		}
	}
	lines := []string{
		fmt.Sprintf("✅ <b>TẮT CHẾ ĐỘ THỦ CÔNG – %s</b>", st),
		"",
		fmt.Sprintf("🕒 Thời gian: <b>%s</b>", fmtTs(ev.Ts)),
	}
	if on, opens, ok := t.store.ManualSession(ev.ID); ok {
		dur := int(ev.Ts.Sub(on).Seconds())
		lines = append(lines,
			fmt.Sprintf("⏱ Thời lượng: <b>%d phút %02d giây</b>", dur/60, dur%60),
			fmt.Sprintf("🚪 Số lần mở cổng: <b>%d</b>", opens))
	}
	return append(lines,
		fmt.Sprintf("🆔 Mã sự kiện: <b>#%d</b>", ev.ID),
		"🔄 <i>Cổng đã đóng – trở lại cân tự động</i>")
}

func (t *TelegramWorker) sendEvent(ev *Event) error {
	var lines []string
	if ev.Status == "OVER" {
		lines = t.overLines(ev)
	} else {
		lines = t.manualLines(ev)
	}
	lines = append(lines, lateLine(ev.Ts)...)
	img := ""
	if ev.ImagePath != "" {
		img = filepath.Join(t.cfg.ImageDir, ev.ImagePath)
		if _, err := os.Stat(img); err != nil {
			img = ""
		}
	}
	if img == "" {
		lines = append(lines, "📷 <i>Không có ảnh</i>")
	}
	return t.sendText(strings.Join(lines, "\n"), img)
}

// sendPending: trả lỗi mạng / 429 / DB để vòng ngoài tạm dừng; lỗi từ chối thì lùi riêng sự kiện đó.
func (t *TelegramWorker) sendPending() error {
	for {
		t.mu.Lock()
		if len(t.sys) == 0 {
			t.mu.Unlock()
			break
		}
		m := t.sys[0]
		t.mu.Unlock()
		err := t.sendText(m.text, m.photo)
		if err != nil && !errors.Is(err, errRejected) {
			return err // giữ tin, thử lại sau
		}
		if err == nil {
			log.Printf("Đã gửi tin hệ thống lên Telegram")
		}
		t.mu.Lock()
		t.sys = t.sys[1:] // bị từ chối thì bỏ
		t.mu.Unlock()
	}
	if !t.store.Ready() {
		return nil
	}
	evs, err := t.store.PendingTelegram(time.Now(), 10)
	if err != nil {
		return err
	}
	for i := range evs {
		ev := &evs[i]
		t0 := time.Now()
		err := t.sendEvent(ev)
		switch {
		case err == nil:
			if err := t.store.MarkSent(ev.ID); err != nil {
				return err
			}
			log.Printf("Đã gửi cảnh báo %s #%d (%.1f s)", ev.Status, ev.ID, time.Since(t0).Seconds())
		case errors.Is(err, errRejected):
			delay := retryBase << ev.TgRetries
			if delay > retryMax || delay <= 0 {
				delay = retryMax
			}
			t.store.MarkFailed(ev.ID, ev.TgRetries+1, time.Now().Add(delay))
			log.Printf("Cảnh báo #%d gửi lỗi lần %d – thử lại sau %s", ev.ID, ev.TgRetries+1, delay)
		default:
			return err
		}
	}
	return nil
}

func (t *TelegramWorker) Run() {
	netBackoff := 10 * time.Second
	for {
		pause := scanInterval
		err := t.sendPending()
		var rl rateLimited
		switch {
		case err == nil:
			netBackoff = 10 * time.Second
		case errors.As(err, &rl):
			log.Printf("Telegram giới hạn tốc độ (429) – tạm dừng %s", rl.after)
			pause = rl.after + time.Second
		default:
			log.Printf("Chưa gửi được cảnh báo (mạng?): %v – thử lại sau %s", err, netBackoff)
			pause = netBackoff
			netBackoff *= 2
			if netBackoff > netBackoffMax {
				netBackoff = netBackoffMax
			}
		}
		time.Sleep(pause)
	}
}
