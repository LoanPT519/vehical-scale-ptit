package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Scale xử lý dòng từ MCU: W (telemetry 5 Hz) → Live; EV (sự kiện) → chụp ảnh + đẩy cảnh báo vào hàng đợi Telegram.
// Không lưu DB: PASS / LEAVE / thao tác cổng / lỗi chỉ ghi log.
//
//	W,<kg>,<trạng thái>,<cổng 1=mở 2=đóng>
//	EV,OVER,<kg> | EV,PASS,<kg> | EV,LEAVE | EV,MANUAL,ENTER|EXIT|OPEN|CLOSE | EV,AUTOZERO,<kg> | EV,ERR,<mã>
type Scale struct {
	cfg  *Config
	live *Live
	cam  *Camera
	tg   *TelegramWorker // nil nếu chưa cấu hình Telegram

	lastW     time.Time
	lastKg    float64
	lastState string
	lastGate  string
	warned    bool

	manualOn time.Time // phiên thủ công đang mở (để báo thời lượng, số lần mở cổng khi tắt)
	opens    int
}

func newScale(cfg *Config, live *Live, cam *Camera, tg *TelegramWorker) *Scale {
	return &Scale{cfg: cfg, live: live, cam: cam, tg: tg, lastW: time.Now()}
}

func parseKg(parts []string, i int) (float64, bool) {
	if i >= len(parts) {
		return 0, false
	}
	v, err := strconv.ParseFloat(parts[i], 64)
	return v, err == nil
}

// takePhoto copy frame camera mới nhất ra thư mục ảnh; trả đường dẫn tuyệt đối ("" nếu không có).
func (s *Scale) takePhoto(now time.Time) string {
	path := filepath.Join(s.cfg.ImageDir, now.Format("2006/01/02"),
		fmt.Sprintf("%s_%03d.jpg", now.Format("150405"), now.Nanosecond()/1e6))
	if err := s.cam.Snapshot(path); err != nil {
		log.Printf("Lỗi lưu ảnh (%v) – cảnh báo không kèm ảnh", err)
		return ""
	}
	return path
}

func (s *Scale) Handle(line string) {
	if line == "" { // 1 s không có dữ liệu
		if time.Since(s.lastW) > 3*time.Second && !s.warned {
			log.Printf("Không nhận được W từ MCU quá 3 s")
			s.warned = true
		}
		return
	}
	parts := strings.Split(line, ",")
	switch parts[0] {
	case "W":
		s.lastW, s.warned = time.Now(), false
		if kg, ok := parseKg(parts, 1); ok {
			s.lastKg = kg
		}
		st := ""
		if len(parts) > 2 {
			st = parts[2]
		}
		if st != s.lastState { // log chuyển trạng thái để đo thời gian
			log.Printf("MCU state: %s → %s (%.3f kg)", s.lastState, st, s.lastKg)
			s.lastState = st
		}
		gate := ""
		if len(parts) > 3 {
			gate = parts[3]
		}
		if gate != s.lastGate {
			log.Printf("Cổng: %s", map[string]string{"1": "1 - MỞ", "2": "2 - ĐÓNG"}[gate])
			s.lastGate = gate
		}
		g := 2
		if gate == "1" {
			g = 1
		}
		s.live.update(s.lastKg, st, g)
		return
	case "EV":
	default:
		log.Printf("MCU: %s", line) // OK,... / ERR,... / PONG / INFO,...
		return
	}

	ev := ""
	if len(parts) > 1 {
		ev = parts[1]
	}
	arg := ""
	if len(parts) > 2 {
		arg = parts[2]
	}
	now := time.Now().In(s.cfg.Location)
	switch {
	case ev == "OVER":
		kg, ok := parseKg(parts, 2)
		if !ok {
			kg = s.lastKg
		}
		img := s.takePhoto(now)
		s.tg.Push(Alert{Kind: "OVER", Ts: now, WeightKg: kg, Photo: img})
		log.Printf("QUÁ TẢI %.3f kg – ảnh: %s", kg, img)
	case ev == "PASS":
		kg, ok := parseKg(parts, 2)
		if !ok {
			kg = s.lastKg
		}
		log.Printf("PASS %.3f kg", kg)
	case ev == "MANUAL" && arg == "ENTER":
		img := s.takePhoto(now)
		s.manualOn, s.opens = now, 0
		s.tg.Push(Alert{Kind: "MANUAL_ON", Ts: now, WeightKg: s.lastKg, Photo: img})
		log.Printf("BẬT chế độ thủ công – ảnh: %s", img)
	case ev == "MANUAL" && arg == "EXIT":
		img := s.takePhoto(now)
		s.tg.Push(Alert{Kind: "MANUAL_OFF", Ts: now, WeightKg: s.lastKg, Photo: img,
			ManualOn: s.manualOn, Opens: s.opens})
		s.manualOn, s.opens = time.Time{}, 0
		log.Printf("TẮT chế độ thủ công – ảnh: %s", img)
	case ev == "MANUAL" && (arg == "OPEN" || arg == "CLOSE"):
		if arg == "OPEN" {
			s.opens++
		}
		log.Printf("Điều khiển tay: %s", arg)
	case ev == "LEAVE":
		log.Printf("Xe rời cân")
	case ev == "AUTOZERO":
		log.Printf("Cân bị âm (%s kg) – MCU tự lấy lại mốc 0", arg)
	case ev == "ERR":
		log.Printf("MCU báo lỗi: %s", line)
	default:
		log.Printf("Sự kiện: %s", line)
	}
}
