#!/bin/bash
# Nạp file .hex vào Arduino Leonardo onboard LattePanda (bootloader Caterina).
#
# Dùng:
#   ./upload_arduino.sh <file.hex>          # nạp hex
#   ./upload_arduino.sh --backup <out.hex>  # chỉ đọc firmware hiện tại ra file
#
# Cơ chế: mở cổng ở 1200 baud rồi đóng -> Leonardo reset vào bootloader (~8s),
# xuất hiện cổng /dev/ttyACM* mới -> avrdude (programmer avr109) nạp qua cổng đó.
set -euo pipefail

PORT=${PORT:-/dev/ttyACM0}
MODE=write
if [ "${1:-}" = "--backup" ]; then MODE=read; shift; fi
HEX=${1:?Thiếu đường dẫn file .hex}

# Tìm tty của thiết bị USB đang ở chế độ bootloader (Caterina: 2341:0036).
# Bootloader thường dùng lại đúng tên /dev/ttyACM0 nên phải dò theo USB PID.
find_bootloader_tty() {
  for d in /sys/bus/usb/devices/*; do
    [ "$(cat "$d/idVendor" 2>/dev/null)" = 2341 ] || continue
    [ "$(cat "$d/idProduct" 2>/dev/null)" = 0036 ] || continue
    for t in "$d"/*/tty/ttyACM*; do
      [ -e "$t" ] && { echo "/dev/$(basename "$t")"; return 0; }
    done
  done
  return 1
}

echo ">> Reset $PORT vào bootloader (1200 baud touch)"
stty -F "$PORT" 1200 hupcl 2>/dev/null || true

BOOT=""
for _ in $(seq 1 50); do
  sleep 0.1
  if BOOT=$(find_bootloader_tty) && [ -w "$BOOT" ]; then break; fi
  BOOT=""
done
[ -z "$BOOT" ] && { echo "!! Không thấy cổng bootloader (USB 2341:0036)"; exit 1; }
sleep 0.3 # chờ udev gán quyền cho cổng
echo ">> Bootloader tại $BOOT"

if [ "$MODE" = read ]; then
  avrdude -q -p atmega32u4 -c avr109 -b 57600 -P "$BOOT" -D -U "flash:r:$HEX:i"
  echo ">> Đã sao lưu flash vào $HEX"
else
  avrdude -q -p atmega32u4 -c avr109 -b 57600 -P "$BOOT" -D -U "flash:w:$HEX:i"
  echo ">> Nạp xong $HEX"
fi
