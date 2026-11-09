// Launcher trạm cân PTIT – chạy native bằng systemd trên LattePanda.
//
//	serial MCU (Leonardo) → Scale (sự kiện + ảnh camera) → Store (SQLite) → TelegramWorker (outbox)
//	Camera: watchdog GStreamer ghi frame vào /dev/shm, sự kiện chỉ việc copy frame mới nhất
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"
)

func logErr(what string, err error) { log.Printf("Lỗi %s: %v", what, err) }

func main() {
	log.SetFlags(0) // journald đã gắn thời gian
	cfg := loadConfig()
	if err := os.MkdirAll(cfg.ImageDir, 0o755); err != nil {
		log.Fatalf("thư mục ảnh: %v", err)
	}

	store, err := openStore(cfg.DBPath, cfg.Location)
	if err != nil {
		log.Fatalf("SQLite %s: %v", cfg.DBPath, err)
	}

	cam := newCamera(cfg.CameraDevice, cfg.CameraDir)
	go cam.Run()

	live := newLive()
	if cfg.TgToken != "" && cfg.TgChatID != "" {
		tg := newTelegram(store, cfg)
		go tg.Run()
		if firstStartSinceBoot() {
			time.AfterFunc(20*time.Second, func() { sendStartup(cfg, tg, live, cam) })
		} else {
			log.Printf("Launcher khởi động lại (board không reboot) – không gửi tin khởi động")
		}
	} else {
		log.Printf("Chưa cấu hình Telegram – chỉ lưu DB")
	}

	scale := newScale(cfg, live, cam, store)
	link := newSerial(cfg.SerialPort, func(l *SerialLink) { l.Send(fmt.Sprintf("TH,%.3f", cfg.ThresholdKg)) })
	log.Printf("Ngưỡng quá tải: %.3f kg, DB: %s, ảnh: %s", cfg.ThresholdKg, cfg.DBPath, cfg.ImageDir)
	go link.Run(scale.Handle)

	// Dừng gọn: đóng DB (GStreamer tự nhận SIGTERM qua Pdeathsig khi tiến trình thoát)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("Đang dừng launcher")
	store.db.Close()
}
