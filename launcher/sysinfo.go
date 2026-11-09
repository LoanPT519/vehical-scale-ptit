package main

import (
	"fmt"
	"html"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------
// Tin "hệ thống đã khởi động": chỉ gửi khi LattePanda vừa bật/reboot (service restart thì không)
// ---------------------------------------------------------------------

const bootMark = "/dev/shm/vehicle-scale.booted" // /dev/shm là RAM → mất khi board reboot

func firstStartSinceBoot() bool {
	if _, err := os.Stat(bootMark); err == nil {
		return false
	}
	if f, err := os.Create(bootMark); err == nil {
		f.Close()
	}
	return true
}

// waitNTP: lúc mới bật, board chạy theo RTC (sai giờ, không có pin CMOS) tới khi có mạng.
func waitNTP(timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	for {
		out, err := exec.Command("timedatectl").Output()
		if err != nil {
			return false
		}
		if strings.Contains(string(out), "synchronized: yes") {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(3 * time.Second)
	}
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

type machine struct {
	uptime  time.Duration
	temp    float64
	hasTemp bool
	load    string
	ram     string
	disk    string
	cpus    int
}

func machineInfo() machine {
	m := machine{cpus: runtime.NumCPU()}
	for i := 0; i < 10; i++ {
		if t, err := strconv.Atoi(strings.TrimSpace(readFile(fmt.Sprintf("/sys/class/thermal/thermal_zone%d/temp", i)))); err == nil {
			if c := float64(t) / 1000; !m.hasTemp || c > m.temp {
				m.temp, m.hasTemp = c, true
			}
		}
	}
	mem := map[string]int{}
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if f := strings.Fields(v); ok && len(f) > 0 {
			n, _ := strconv.Atoi(f[0])
			mem[k] = n / 1024 // MB
		}
	}
	used, total := mem["MemTotal"]-mem["MemAvailable"], mem["MemTotal"]
	m.ram = fmt.Sprintf("%d/%d MB (%d%%)", used, total, 100*used/max(total, 1))
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) == nil {
		m.disk = fmt.Sprintf("trống %.1f / %.1f GB", float64(st.Bavail)*float64(st.Bsize)/1e9,
			float64(st.Blocks)*float64(st.Bsize)/1e9)
	}
	if f := strings.Fields(readFile("/proc/loadavg")); len(f) >= 3 {
		m.load = strings.Join(f[:3], " · ")
	}
	if f := strings.Fields(readFile("/proc/uptime")); len(f) > 0 {
		s, _ := strconv.ParseFloat(f[0], 64)
		m.uptime = time.Duration(s * float64(time.Second))
	}
	return m
}

// localIP: IP của card đang giữ default route (không gửi gói nào, chỉ hỏi bảng định tuyến)
func localIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "?"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

var netKind = [][2]string{{"wlan", "WiFi onboard"}, {"wlx", "USB WiFi"}, {"enx", "USB LAN"}, {"eth", "LAN"}}

type netAddr struct{ dev, kind, ip string }

// netIPs: mọi card mạng thật đang có IPv4 – đọc lúc gửi, không cố định (bỏ lo, docker0, br-…, veth…)
func netIPs() []netAddr {
	var res []netAddr
	ifs, _ := net.Interfaces()
	for _, it := range ifs {
		kind := ""
		for _, k := range netKind {
			if strings.HasPrefix(it.Name, k[0]) {
				kind = k[1]
				break
			}
		}
		if kind == "" {
			continue
		}
		addrs, _ := it.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				res = append(res, netAddr{it.Name, kind, ipn.IP.String()})
			}
		}
	}
	return res
}

var stateVI = map[string]string{"IDLE": "Chờ xe", "WEIGHING": "Đang cân", "PASS": "Hợp lệ", "OVER": "Quá tải",
	"MANUAL": "Điều khiển tay", "ERROR": "Lỗi cảm biến"}

func startupMessage(cfg *Config, live *Live, cam *Camera) string {
	m := machineInfo()
	s := live.snapshot()
	on := func(ok bool) string {
		if ok {
			return "✅ <b>Online</b>"
		}
		return "❌ <b>Offline</b>"
	}
	gate := map[int]string{1: "🔓 MỞ", 2: "🔒 ĐÓNG"}[s.Gate]
	if gate == "" {
		gate = "?"
	}
	kg := "–"
	if s.McuOnline {
		kg = fmt.Sprintf("%.3f kg", s.Kg)
	}
	temp := "?"
	if m.hasTemp {
		dot := "🔴"
		if m.temp < 65 {
			dot = "🟢"
		} else if m.temp < 80 {
			dot = "🟡"
		}
		temp = fmt.Sprintf("%.0f °C %s", m.temp, dot)
	}
	state := stateVI[s.State]
	if state == "" {
		state = s.State
	}
	lines := []string{
		fmt.Sprintf("🔄 <b>%s – HỆ THỐNG ĐÃ KHỞI ĐỘNG</b>", html.EscapeString(strings.ToUpper(cfg.StationName))),
		fmt.Sprintf("🕒 Thời gian: <b>%s</b>", time.Now().In(cfg.Location).Format("02/01/2006 15:04:05")),
		"⚡ Sự kiện: <b>LattePanda vừa khởi động lại</b>",
		fmt.Sprintf("⏱ Board đã chạy: <b>%d phút</b>", int(m.uptime.Minutes())),
		"",
		"📟 <b>TRẠM CÂN</b>",
		"🔌 MCU: " + on(s.McuOnline),
		"📷 Camera: " + on(cam.Online()),
		fmt.Sprintf("🚦 Trạng thái: <b>%s</b>", state),
		fmt.Sprintf("🚧 Cổng: <b>%s</b>", gate),
		fmt.Sprintf("⚖️ Cân hiện tại: <b>%s</b>", kg),
		fmt.Sprintf("🎯 Ngưỡng quá tải: <b>%.3f kg</b>", cfg.ThresholdKg),
		"",
		"🖥 <b>MÁY CHỦ (LattePanda)</b>",
		fmt.Sprintf("🌡 Nhiệt độ SoC: <b>%s</b>", temp),
		fmt.Sprintf("⚙️ Tải CPU: <b>%s</b> (%d nhân)", m.load, m.cpus),
		fmt.Sprintf("🧠 RAM: <b>%s</b>", m.ram),
		fmt.Sprintf("💾 Ổ đĩa: <b>%s</b>", m.disk),
	}
	mainIP := localIP()
	ips := netIPs()
	if len(ips) == 0 {
		lines = append(lines, "🌐 IP: "+mainIP)
	}
	for _, a := range ips {
		star := ""
		if a.ip == mainIP {
			star = " ⭐"
		}
		lines = append(lines, fmt.Sprintf("🌐 %s (%s): %s%s", a.kind, a.dev, a.ip, star))
	}
	return strings.Join(lines, "\n")
}

func sendStartup(cfg *Config, tg *TelegramWorker, live *Live, cam *Camera) {
	synced := waitNTP(2 * time.Minute)
	if !synced {
		log.Printf("Chưa đồng bộ NTP sau 120 s – gửi tin khởi động với giờ hiện tại của board")
	}
	photo := filepath.Join(cfg.ImageDir, "system", "boot_"+time.Now().In(cfg.Location).Format("20060102_150405")+".jpg")
	if err := cam.Snapshot(photo); err != nil {
		log.Printf("Không chụp được ảnh khởi động: %v", err)
		photo = ""
	}
	text := startupMessage(cfg, live, cam)
	if !synced {
		text = strings.Replace(text, "\n", "\n⚠️ <i>Giờ chưa đồng bộ NTP – thời gian có thể sai</i>\n", 1)
	}
	tg.Notify(text, photo)
}
