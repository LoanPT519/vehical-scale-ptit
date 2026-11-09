# vehical-scale-ptit

Hệ thống giám sát trọng tải xe (Trạm cân PTIT) trên **LattePanda V1**:

- `firmware/` – firmware cho **Arduino Leonardo onboard** (ATmega32U4): load cell + HX711, LED matrix MAX7219, WS2812B, servo barrier, joystick.
- `launcher/` – dịch vụ **Go** chạy native trên LattePanda: đọc sự kiện từ Arduino qua serial → chụp ảnh camera USB → lưu SQLite → gửi cảnh báo Telegram.
- `tools/` – script build/nạp/triển khai từ Mac lên board và unit systemd.

```
Load cell ─ HX711 ─┐
Joystick ──────────┤  Arduino Leonardo ──USB serial 115200──▶ launcher (Go, systemd)
Servo / MAX7219 / ─┘   (firmware/)                             ├─ Camera USB (GStreamer → /dev/shm)
WS2812B                                                        ├─ SQLite  (data/events.db + data/images/)
                                                               └─ Telegram (outbox, gửi bù khi mất mạng)
```

## Firmware (`firmware/main/main.ino`)

Cooperative scheduler theo `millis()` (không `delay`). Luồng trạng thái: `IDLE → WEIGHING → PASS/OVER → IDLE`, `MANUAL` (joystick), `ERROR`.

| Thiết bị | Chân |
|---|---|
| HX711 DT / SCK | D2 / D3 |
| Joystick SW / trục cổng | D4 / A0 |
| WS2812B | D6 |
| MAX7219 DIN / CLK / CS | D7 / D8 / D10 |
| Servo barrier | D9 (đóng 180°, mở 90°) |

Thư viện cần cài cho `arduino-cli` (core `arduino:avr`): `HX711`, `MD_Parola`, `MD_MAX72XX`, `Adafruit NeoPixel`, `Servo`.

Nạp: `firmware/upload_arduino.sh <file.hex>` chạy trên board (reset 1200 baud vào bootloader Caterina rồi nạp bằng `avrdude`).

### Giao thức serial (115200 8N1, mỗi dòng 1 bản tin)

MCU → host:

```
W,<kg>,<trạng thái>,<cổng 1=mở 2=đóng>          telemetry ~5 Hz
EV,OVER,<kg> | EV,PASS,<kg> | EV,LEAVE
EV,MANUAL,ENTER|EXIT|OPEN|CLOSE
EV,AUTOZERO,<kg> | EV,ERR,<mã>
```

Host → MCU: `PING`, `TH,<kg>` (ngưỡng quá tải), `TARE`, `CAL,<kg>`, `GATE,OPEN`, `GATE,CLOSE`, `INFO`.

## Launcher (`launcher/`)

Go, build tĩnh `CGO_ENABLED=0` (SQLite thuần Go `modernc.org/sqlite`) → chạy được trên Ubuntu 16.04 của board, không cần cài thêm thư viện.

| File | Vai trò |
|---|---|
| `main.go` | Khởi động các thành phần |
| `serial.go` | Kết nối serial termios, tự mở lại khi rút cáp / MCU reset / nạp firmware |
| `scale.go` | Xử lý dòng `W` / `EV` – chụp ảnh và ghi sự kiện |
| `camera.go` | Watchdog GStreamer: MJPEG → 4 frame mới nhất trong `/dev/shm`, tự dò lại khi rút/cắm camera |
| `store.go` | SQLite: bảng `events` + hàng đợi Telegram (`tg_sent`, `tg_retries`, `tg_next_at`) |
| `telegram.go` | Gửi cảnh báo OVER / bật-tắt thủ công kèm ảnh; giãn cách, xử lý 429, retry lùi dần |
| `sysinfo.go` | Tin báo "hệ thống đã khởi động" (chỉ khi board vừa reboot) |
| `config.go` | Đọc biến môi trường / file `.env` |

Sự kiện có chụp ảnh: `OVER`, `MANUAL_ON`, `MANUAL_OFF`, `MANUAL_OPEN`, `MANUAL_CLOSE`. Gửi Telegram: `OVER`, `MANUAL_ON`, `MANUAL_OFF`.

Yêu cầu trên board: `gstreamer1.0-tools` + plugin `v4l2src`/`multifilesink`, user thuộc nhóm `dialout` và `video`.

### Cấu hình

Copy `launcher/.env.example` thành `launcher/.env` và điền token Telegram. Các biến:

| Biến | Mặc định |
|---|---|
| `TG_BOT_TOKEN`, `TG_CHAT_ID` | trống → chỉ lưu DB |
| `THRESHOLD_KG` | `0.15` |
| `SERIAL_PORT` | `/dev/ttyACM0` |
| `CAMERA_DEVICE` | `auto` |
| `DATA_DIR` | `~/vehicle-scale-ptit/data` |
| `STATION_NAME` | `Trạm cân PTIT` |

## Triển khai (từ Mac)

```bash
./tools/deploy.sh fw         # build firmware + nạp Arduino
./tools/deploy.sh launcher   # build launcher Go + cài service vehicle-scale-launcher
./tools/deploy.sh            # cả hai
BOARD=simon@<ip> ./tools/deploy.sh
```

Trên board: `~/vehicle-scale-ptit/` (binary, `.env`, `data/`). Log: `journalctl -u vehicle-scale-launcher -f`.

> Serial và camera chỉ dùng được bởi 1 tiến trình: script deploy sẽ tắt (disable) service cũ `vehicle-scale` nếu đang chạy. Bật lại: `sudo systemctl disable --now vehicle-scale-launcher && sudo systemctl enable --now vehicle-scale`.
