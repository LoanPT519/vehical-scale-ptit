package main

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config đọc từ biến môi trường; nếu có file .env trong thư mục chạy thì nạp thêm
// (biến môi trường thật được ưu tiên hơn file).
type Config struct {
	DataDir      string // dữ liệu trên board: SQLite + ảnh sự kiện
	DBPath       string
	ImageDir     string // ảnh sự kiện
	CameraDir    string // GStreamer ghi vòng frame camera mới nhất vào đây (RAM)
	CameraDevice string // "auto": tự dò webcam, chịu được rút/cắm lại
	SerialPort   string
	ThresholdKg  float64
	StationName  string
	TgToken      string
	TgChatID     string
	Location     *time.Location
}

// Tên khoá kiểu cũ trong .env → tên chuẩn
var envAliases = map[string]string{"teleBotToken": "TG_BOT_TOKEN", "teleChatId": "TG_CHAT_ID"}

var envLine = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*[:=]\s*["']?([^"'\n#]*)["']?`)

// loadDotEnv chấp nhận cả 2 kiểu dòng:  KEY=value   hoặc   key: "value"
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := envLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		key := m[1]
		if alias, ok := envAliases[key]; ok {
			key = alias
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, strings.TrimSpace(m[2]))
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func loadConfig() *Config {
	loadDotEnv(env("ENV_FILE", ".env"))
	loc, err := time.LoadLocation(env("TZ", "Asia/Ho_Chi_Minh"))
	if err != nil {
		log.Fatalf("TZ không hợp lệ: %v", err)
	}
	th, err := strconv.ParseFloat(env("THRESHOLD_KG", "0.15"), 64)
	if err != nil || th <= 0 {
		log.Fatalf("THRESHOLD_KG không hợp lệ")
	}
	dataDir := expandHome(env("DATA_DIR", "~/vehicle-scale-ptit/data"))
	return &Config{
		DataDir:      dataDir,
		DBPath:       expandHome(env("DB_PATH", filepath.Join(dataDir, "events.db"))),
		ImageDir:     expandHome(env("IMAGE_DIR", filepath.Join(dataDir, "images"))),
		CameraDir:    env("CAMERA_DIR", "/dev/shm/vehicle-scale-cam"),
		CameraDevice: env("CAMERA_DEVICE", "auto"),
		SerialPort:   env("SERIAL_PORT", "/dev/ttyACM0"),
		ThresholdKg:  th,
		StationName:  env("STATION_NAME", "Trạm cân PTIT"),
		TgToken:      os.Getenv("TG_BOT_TOKEN"),
		TgChatID:     os.Getenv("TG_CHAT_ID"),
		Location:     loc,
	}
}
