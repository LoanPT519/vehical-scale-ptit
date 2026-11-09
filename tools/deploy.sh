#!/usr/bin/env bash
# Triển khai từ Mac lên LattePanda qua SSH.
# Dùng (từ thư mục gốc repo):
#   ./tools/deploy.sh fw         # build firmware bằng arduino-cli + nạp Arduino Leonardo onboard
#   ./tools/deploy.sh launcher   # build launcher Go (linux/amd64) + cài systemd
#   ./tools/deploy.sh all        # cả hai (mặc định)
# Đổi board: BOARD=simon@<ip> ./tools/deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."
WHAT=${1:-all}
BOARD=${BOARD:-simon@192.168.0.114}
RDIR=vehicle-scale-ptit
SVC=vehicle-scale-launcher
FQBN=arduino:avr:leonardo
ENV_FILE=${ENV_FILE:-launcher/.env}     # token Telegram (không commit)

# Lọc 3 dòng cảnh báo "post-quantum" của SSH, giữ nguyên exit code của lệnh từ xa
ssh_q() { ssh "$BOARD" "$@" 2> >(grep -v -e WARNING -e 'This session' -e 'server may need' >&2); }

# Chỉ 1 tiến trình được giữ /dev/ttyACM0 và camera → dừng launcher (và service cũ nếu còn chạy)
stop_launcher() {
  ssh_q "sudo systemctl stop $SVC 2>/dev/null
    if systemctl is-active --quiet vehicle-scale; then
      echo '>> Tắt service cũ vehicle-scale (bật lại: sudo systemctl enable --now vehicle-scale)'
      sudo systemctl disable --now vehicle-scale >/dev/null 2>&1
    fi
    pkill -f '[m]ultifilesink location=/dev/shm/vehicle-scale-cam'; true"
}

start_launcher() {
  if ssh_q "test -x ~/$RDIR/vehicle-scale-launcher"; then
    ssh_q "sudo systemctl start $SVC && systemctl is-active $SVC"
  fi
}

deploy_fw() {
  arduino-cli compile --fqbn "$FQBN" --output-dir firmware/main/build firmware/main | grep -E 'Sketch uses|Global'
  ssh_q "mkdir -p ~/$RDIR/firmware"
  scp -q firmware/main/build/main.ino.hex firmware/upload_arduino.sh "$BOARD:$RDIR/firmware/" 2>/dev/null
  stop_launcher
  ssh_q "cd ~/$RDIR/firmware && bash upload_arduino.sh main.ino.hex" | grep -E '>>|verified|error'
}

deploy_launcher() {
  echo ">> Build launcher (linux/amd64)"
  (cd launcher && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/vehicle-scale-launcher .)
  ssh_q "mkdir -p ~/$RDIR/data"
  scp -q launcher/bin/vehicle-scale-launcher "$BOARD:$RDIR/vehicle-scale-launcher.new" 2>/dev/null
  scp -q tools/$SVC.service "$BOARD:$RDIR/" 2>/dev/null
  if [ -f "$ENV_FILE" ]; then
    ssh_q "umask 077; cat > ~/$RDIR/.env" < "$ENV_FILE"
  else
    echo ">> Không có $ENV_FILE – launcher chạy không có Telegram (chỉ ghi log)"
  fi
  stop_launcher
  ssh_q "set -e; cd ~/$RDIR
    mv -f vehicle-scale-launcher.new vehicle-scale-launcher && chmod 755 vehicle-scale-launcher
    sudo cp $SVC.service /etc/systemd/system/$SVC.service
    sudo systemctl daemon-reload && sudo systemctl enable $SVC >/dev/null 2>&1"
}

case "$WHAT" in
  fw)       deploy_fw ;;
  launcher) deploy_launcher ;;
  all)      deploy_fw; deploy_launcher ;;
  *)        echo "Dùng: $0 [fw|launcher|all]"; exit 1 ;;
esac
start_launcher
