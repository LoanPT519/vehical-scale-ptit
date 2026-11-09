package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Scale xử lý dòng từ MCU: W (telemetry 5 Hz) → Live; EV (sự kiện) → chụp ảnh + ghi SQLite (+ outbox Telegram).
//
//	W,<kg>,<trạng thái>,<cổng 1=mở 2=đóng>
//	EV,OVER,<kg> | EV,PASS,<kg> | EV,LEAVE | EV,MANUAL,ENTER|EXIT|OPEN|CLOSE | EV,AUTOZERO,<kg> | EV,ERR,<mã>
type Scale struct {
	cfg   *Config
	live  *Live
	cam   *Camera
	store *Store

	lastW     time.Time
	lastKg    float64
	lastState string
	lastGate  string
	warned    bool
}

func newScale(cfg *Config, live *Live, cam *Camera, store *Store) *Scale {
	return &Scale{cfg: cfg, live: live, cam: cam, store: store, lastW: time.Now()}
}

func parseKg(parts []string, i int) (float64, bool) {
	if i >= len(parts) {
		return 0, false
	}
	v, err := strconv.ParseFloat(parts[i], 64)
	return v, err == nil
}

// takePhoto copy frame camera mới nhất ra thư mục ảnh; trả đường dẫn tương đối trong ImageDir ("" nếu không có).
func (s *Scale) takePhoto(now time.Time) string {
	rel := filepath.Join(now.Format("2006/01/02"), fmt.Sprintf("%s_%03d.jpg", now.Format("150405"), now.Nanosecond()/1e6))
	if err := s.cam.Snapshot(filepath.Join(s.cfg.ImageDir, rel)); err != nil {
		log.Printf("Lỗi lưu ảnh (%v) – lưu sự kiện không ảnh", err)
		return ""
	}
	return rel
}

func idStr(id uint) string {
	if id == 0 {
		return "(lỗi ghi DB)"
	}
	return fmt.Sprintf("#%d", id)
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
	th := s.cfg.ThresholdKg
	switch {
	case ev == "OVER":
		kg, ok := parseKg(parts, 2)
		if !ok {
			kg = s.lastKg
		}
		img := s.takePhoto(now)
		id := s.store.Insert(now, kg, th, "OVER", img)
		log.Printf("%s QUÁ TẢI %.3f kg – ảnh: %s", idStr(id), kg, img)
	case ev == "PASS":
		kg, ok := parseKg(parts, 2)
		if !ok {
			kg = s.lastKg
		}
		id := s.store.Insert(now, kg, th, "PASS", "")
		log.Printf("%s PASS %.3f kg", idStr(id), kg)
	case ev == "MANUAL" && (arg == "ENTER" || arg == "EXIT"):
		img := s.takePhoto(now) // bật/tắt chế độ thủ công → lưu + cảnh báo Telegram
		status, vi := "MANUAL_ON", "BẬT"
		if arg == "EXIT" {
			status, vi = "MANUAL_OFF", "TẮT"
		}
		id := s.store.Insert(now, s.lastKg, th, status, img)
		log.Printf("%s %s chế độ thủ công – ảnh: %s", idStr(id), vi, img)
	case ev == "MANUAL" && (arg == "OPEN" || arg == "CLOSE"):
		img := s.takePhoto(now) // chụp cả lúc mở và lúc đóng cổng bằng tay
		s.store.Insert(now, s.lastKg, th, "MANUAL_"+arg, img)
		log.Printf("Điều khiển tay: %s – ảnh: %s", arg, img)
	case ev == "LEAVE":
		s.store.SetLeave(now)
		log.Printf("Xe rời cân")
	case ev == "AUTOZERO":
		log.Printf("Cân bị âm (%s kg) – MCU tự lấy lại mốc 0", arg)
	case ev == "ERR":
		s.store.Insert(now, 0, th, "ERR", "")
		log.Printf("MCU báo lỗi: %s", line)
	default:
		log.Printf("Sự kiện: %s", line)
	}
}
