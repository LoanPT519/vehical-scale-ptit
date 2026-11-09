// Launcher trạm cân PTIT – chạy native bằng systemd trên LattePanda.
//
//	serial MCU (Leonardo) → Scale (phát hiện sự kiện + ảnh camera) → TelegramWorker (hàng đợi RAM, giới hạn QUEUE_SIZE)
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

func main() {
	log.SetFlags(0) // journald đã gắn thời gian
	cfg := loadConfig()
	if err := os.MkdirAll(cfg.ImageDir, 0o755); err != nil {
		log.Fatalf("thư mục ảnh: %v", err)
	}

	cam := newCamera(cfg.CameraDevice, cfg.CameraDir)
	go cam.Run()

	live := newLive()
	var tg *TelegramWorker
	if cfg.TgToken != "" && cfg.TgChatID != "" {
		tg = newTelegram(cfg)
		go tg.Run()
		if firstStartSinceBoot() {
			time.AfterFunc(20*time.Second, func() { sendStartup(cfg, tg, live, cam) })
		} else {
			log.Printf("Launcher khởi động lại (board không reboot) – không gửi tin khởi động")
		}
	} else {
		log.Printf("Chưa cấu hình Telegram – chỉ ghi log")
	}

	scale := newScale(cfg, live, cam, tg)
	link := newSerial(cfg.SerialPort, func(l *SerialLink) { l.Send(fmt.Sprintf("TH,%.3f", cfg.ThresholdKg)) })
	log.Printf("Ngưỡng quá tải: %.3f kg, hàng đợi: %d, ảnh: %s", cfg.ThresholdKg, cfg.QueueSize, cfg.ImageDir)
	go link.Run(scale.Handle)

	// GStreamer tự nhận SIGTERM qua Pdeathsig khi tiến trình thoát
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("Đang dừng launcher")
}
