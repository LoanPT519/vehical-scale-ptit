package main

import (
	"bytes"
	"encoding/json"
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

// TelegramWorker: hàng đợi cảnh báo trong RAM, giới hạn theo QUEUE_SIZE.
//   - Mỗi cảnh báo gửi đúng 1 lần, lỗi (mất mạng, Telegram từ chối) thì bỏ – không lưu, không gửi lại.
//   - Hàng đợi đầy: bỏ cảnh báo cũ nhất để nhận cảnh báo mới.
//   - Mỗi tin cách nhau tối thiểu sendGap (kênh giới hạn ~20 tin/phút); 429 thì chờ retry_after rồi gửi tiếp.
const (
	tgAPI    = "https://api.telegram.org/bot%s/%s"
	sendGap  = 3 * time.Second
	lateNote = 60 * time.Second // nằm trong hàng đợi lâu hơn mức này thì ghi chú "gửi trễ"
)

type rateLimited struct{ after time.Duration }

func (e rateLimited) Error() string { return fmt.Sprintf("429, chờ %s", e.after) }

// Alert – 1 tin cần gửi (cảnh báo sự kiện hoặc tin hệ thống đã soạn sẵn).
type Alert struct {
	Kind     string // OVER | MANUAL_ON | MANUAL_OFF | SYSTEM
	Ts       time.Time
	WeightKg float64
	Text     string    // SYSTEM: nội dung HTML đã soạn
	Photo    string    // đường dẫn ảnh tuyệt đối, "" nếu không có
	ManualOn time.Time // MANUAL_OFF: lúc bật chế độ thủ công (zero nếu không biết)
	Opens    int       // MANUAL_OFF: số lần mở cổng trong phiên
}

type TelegramWorker struct {
	cfg      *Config
	client   *http.Client
	lastSend time.Time

	mu     sync.Mutex
	queue  []Alert
	notify chan struct{}
}

func newTelegram(cfg *Config) *TelegramWorker {
	// Timeout tính cả DNS + upload ảnh qua mạng yếu
	return &TelegramWorker{cfg: cfg, client: &http.Client{Timeout: 60 * time.Second},
		notify: make(chan struct{}, 1)}
}

// Push xếp 1 cảnh báo vào hàng đợi; t == nil (chưa cấu hình Telegram) thì bỏ qua.
func (t *TelegramWorker) Push(a Alert) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if len(t.queue) >= t.cfg.QueueSize {
		old := t.queue[0]
		t.queue = t.queue[1:]
		log.Printf("Hàng đợi Telegram đầy (%d) – bỏ cảnh báo %s lúc %s", t.cfg.QueueSize, old.Kind, fmtTs(old.Ts))
	}
	t.queue = append(t.queue, a)
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

func (t *TelegramWorker) pop() (Alert, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) == 0 {
		return Alert{}, false
	}
	a := t.queue[0]
	t.queue = t.queue[1:]
	return a, true
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
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), t.cfg.TgToken, "***"))
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
		return fmt.Errorf("Telegram trả lỗi %d: %.200s", resp.StatusCode, raw)
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

func (t *TelegramWorker) station() string {
	return html.EscapeString(strings.ToUpper(t.cfg.StationName))
}

func (t *TelegramWorker) overLines(a *Alert) []string {
	th := t.cfg.ThresholdKg
	over, pct := a.WeightKg-th, 100*(a.WeightKg-th)/th
	return []string{
		fmt.Sprintf("🚨 <b>CẢNH BÁO QUÁ TẢI – %s</b>", t.station()),
		"",
		fmt.Sprintf("⚖️ Khối lượng: <b>%.3f kg</b>", a.WeightKg),
		fmt.Sprintf("🎯 Ngưỡng: <b>%.3f kg</b>", th),
		fmt.Sprintf("📈 Vượt ngưỡng: <b>+%.3f kg (+%.0f%%)</b>", over, pct),
		fmt.Sprintf("🕒 Thời gian: <b>%s</b>", fmtTs(a.Ts)),
		"🚧 Xử lý: <b>🔒 Giữ cổng đóng</b>",
	}
}

func (t *TelegramWorker) manualLines(a *Alert) []string {
	if a.Kind == "MANUAL_ON" {
		return []string{
			fmt.Sprintf("🕹 <b>BẬT CHẾ ĐỘ THỦ CÔNG – %s</b>", t.station()),
			"",
			fmt.Sprintf("🕒 Thời gian: <b>%s</b>", fmtTs(a.Ts)),
			fmt.Sprintf("⚖️ Cân lúc bật: <b>%.3f kg</b>", a.WeightKg),
			"⚠️ <i>Cân tự động tạm dừng – cổng điều khiển bằng joystick</i>",
		}
	}
	lines := []string{
		fmt.Sprintf("✅ <b>TẮT CHẾ ĐỘ THỦ CÔNG – %s</b>", t.station()),
		"",
		fmt.Sprintf("🕒 Thời gian: <b>%s</b>", fmtTs(a.Ts)),
	}
	if !a.ManualOn.IsZero() {
		dur := int(a.Ts.Sub(a.ManualOn).Seconds())
		lines = append(lines,
			fmt.Sprintf("⏱ Thời lượng: <b>%d phút %02d giây</b>", dur/60, dur%60),
			fmt.Sprintf("🚪 Số lần mở cổng: <b>%d</b>", a.Opens))
	}
	return append(lines, "🔄 <i>Cổng đã đóng – trở lại cân tự động</i>")
}

func (t *TelegramWorker) message(a *Alert) string {
	if a.Kind == "SYSTEM" {
		return a.Text
	}
	var lines []string
	if a.Kind == "OVER" {
		lines = t.overLines(a)
	} else {
		lines = t.manualLines(a)
	}
	if late := time.Since(a.Ts); late > lateNote {
		lines = append(lines, fmt.Sprintf("⏳ <i>Gửi trễ %d phút</i>", int(late.Minutes())))
	}
	if a.Photo == "" {
		lines = append(lines, "📷 <i>Không có ảnh</i>")
	}
	return strings.Join(lines, "\n")
}

// Run gửi lần lượt từng cảnh báo trong hàng đợi, mỗi cảnh báo 1 lần.
func (t *TelegramWorker) Run() {
	for {
		a, ok := t.pop()
		if !ok {
			<-t.notify
			continue
		}
		text := t.message(&a)
		t0 := time.Now()
		err := t.sendText(text, a.Photo)
		var rl rateLimited
		for i := 0; i < 3 && err != nil && asRateLimited(err, &rl); i++ {
			log.Printf("Telegram giới hạn tốc độ (429) – chờ %s", rl.after)
			time.Sleep(rl.after + time.Second)
			err = t.sendText(text, a.Photo)
		}
		if err != nil {
			log.Printf("Không gửi được cảnh báo %s lúc %s: %v – bỏ qua", a.Kind, fmtTs(a.Ts), err)
			continue
		}
		log.Printf("Đã gửi cảnh báo %s (%.1f s)", a.Kind, time.Since(t0).Seconds())
	}
}

func asRateLimited(err error, rl *rateLimited) bool {
	r, ok := err.(rateLimited)
	if ok {
		*rl = r
	}
	return ok
}
