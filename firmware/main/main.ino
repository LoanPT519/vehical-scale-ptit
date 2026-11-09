// main.ino – Firmware trạm cân xe (ATmega32U4 / Arduino Leonardo onboard LattePanda V1)
// Board: arduino:avr:leonardo
// Luồng theo PDF: IDLE → WEIGHING → PASS/OVER → IDLE, MANUAL (joystick), ERROR (mất HX711)
//
// Đa nhiệm hợp tác (cooperative multitasking) theo nhịp millis():
//   - Không có delay() và không có thao tác chờ nào trong loop(); mỗi task chạy vài chục µs rồi trả về.
//   - Bảng TASKS: mỗi task có chu kỳ riêng, scheduler gọi khi tới hạn → các khối chạy "đồng thời".
//   - Phần cứng làm song song: Timer1 tự phát xung servo, USB CDC chạy bằng ngắt, HX711 tự đo 10 SPS.
//   - TARE / CAL cũng không chặn: gom mẫu dần trong readScale() rồi mới tính.
//   - Lệnh INFO trả về thời gian vòng loop() lớn nhất (µs) để chứng minh không có tác vụ chặn.

#include <EEPROM.h>
#include <HX711.h>

// ===== Bật/tắt từng khối để tích hợp dần (bước F1 → F4) =====
#define USE_MATRIX 1
#define USE_LEDS   1
#define USE_SERVO  1
#define USE_JOY    1

#if USE_MATRIX
#include <MD_Parola.h>
#include <MD_MAX72xx.h>
#endif
#if USE_LEDS
#include <Adafruit_NeoPixel.h>
#endif
#if USE_SERVO
#include <Servo.h>
#endif

// ---------- Chân (đã kiểm tra bằng các test t01…t05) ----------
const uint8_t PIN_HX_DT = 2, PIN_HX_SCK = 3;
const uint8_t PIN_JS_SW = 4;
const uint8_t PIN_JS_GATE = A0;          // lắp trên mô hình: gạt trái/phải nằm trên trục X (A0)
const uint8_t PIN_WS    = 6;
const uint8_t PIN_MX_DIN = 7, PIN_MX_CLK = 8, PIN_MX_CS = 10;
const uint8_t PIN_SERVO = 9;

// ---------- Tham số (mô hình nhỏ, ngưỡng demo 0.15 kg) ----------
const float    DEFAULT_TH    = 0.15f;   // kg: quá tải
const float    W_ENTER       = 0.03f;   // kg: coi là có xe
const float    W_EXIT        = 0.015f;  // kg: coi là xe đã rời (mức tối thiểu)
const float    EXIT_RATIO    = 0.2f;    // hoặc tải còn < 20 % khối lượng vừa cân (mặt cân kẹt / còn dư vài chục g)
const float    ZERO_TRACK_KG = 0.03f;   // IDLE: dư dương nhỏ hơn mức này (= W_ENTER) thì tự trừ đi
const uint16_t ZERO_TRACK_MS = 5000;
const float    STABLE_BAND   = 0.005f;  // kg: dao động cho phép khi "ổn định"
const uint16_t STABLE_MS     = 1000;
const uint16_t SETTLE_MS     = 2000;    // phát hiện xe rồi chờ xe vào hẳn, đứng yên mới bắt đầu đo
const float    NEG_ZERO_KG   = 0.003f;  // kg: âm hơn mức này khi bàn cân trống → tự lấy lại mốc 0
const uint16_t NEG_ZERO_MS   = 3000;    // phải ổn định ở mức âm ≥ 3 s mới TARE (tránh lúc đang nhấc vật)
const uint16_t LEAVE_MS      = 2000;
const uint16_t HX_TIMEOUT_MS = 1000;
const uint16_t TELEMETRY_MS  = 200;
const uint8_t  TARE_SAMPLES  = 10;      // ~1 s ở 10 SPS
const uint8_t  CAL_SAMPLES   = 20;
const uint8_t  GATE_CLOSED   = 180, GATE_OPEN = 90;
const int      JS_DEAD       = 300;     // lệch khỏi tâm bao nhiêu thì tính là gạt
const uint16_t JS_HOLD_MS    = 150;     // phải giữ hướng gạt ≥ 150 ms (lọc lò xo bật qua tâm khi thả)
const bool     JS_INVERT     = true;    // trên mô hình gạt phải cho X− → đảo dấu để phải = mở
const uint16_t LONG_PRESS_MS = 2000;
const uint16_t SCROLL_MS     = 70;      // ms / cột khi chạy chữ (nhỏ hơn = nhanh hơn)
const uint16_t SPIN_MS       = 300;     // ms / bước vòng quay LED khi chờ (lớn hơn = chậm hơn)
const uint16_t LED_REFRESH_MS = 200;    // gửi lại khung màu dù không đổi → tự sửa màu sai do nhiễu
const unsigned long SLEEP_MS = 300000UL;   // IDLE quá 5 phút không có hoạt động → giảm sáng
const int      ADDR_FACTOR   = 0, ADDR_TH = 4;   // EEPROM (factor dùng chung với t01b)

enum State : uint8_t { S_IDLE, S_WEIGHING, S_PASS, S_OVER, S_MANUAL, S_ERROR };
const char *const STATE_NAME[] = {"IDLE", "WEIGHING", "PASS", "OVER", "MANUAL", "ERROR"};

enum Op : uint8_t { OP_NONE, OP_TARE, OP_CAL };

HX711 scale;
State state = S_IDLE;
float threshold = DEFAULT_TH;
float wFilt = 0, decidedKg = 0, stableRef = 0;
unsigned long lastSampleMs = 0, stableSince = 0, belowSince = 0, lastActivity = 0;
bool gateOpen = false;                   // cổng: true = 1 (mở), false = 2 (đóng, mặc định)

Op op = OP_NONE;                         // tác vụ cân đang gom mẫu (không chặn)
uint8_t opN = 0;
long opSum = 0;
float opKnown = 0;

unsigned long loopMaxUs = 0;             // vòng loop() lâu nhất kể từ lần INFO trước

// =====================================================================
// Serial: gửi không chặn
// =====================================================================
void sendLine(const char *s) {
  // Telemetry (W,…) đầy bộ đệm thì bỏ – 200 ms sau có dòng mới. Sự kiện / phản hồi (EV, OK, ERR…) không được
  // mất → vẫn ghi khi host đang mở cổng (DTR); host không đọc thì core USB tự bỏ sau timeout, loop() không treo
  if (Serial.availableForWrite() >= (int)strlen(s) + 2 || (s[0] != 'W' && Serial)) Serial.println(s);
}

// Ghép chuỗi bằng strcat, tự parse số: không kéo printf/strtod vào → tiết kiệm flash
void catNum(char *dst, float v, uint8_t prec) {
  char n[16];
  dtostrf(v, 0, prec, n);
  strcat(dst, n);
}

float parseNum(const char *s) {          // "4.5" → 4.5 ; chuỗi sai → NAN
  float v = 0, scaleF = 1;
  bool neg = false, dot = false, any = false;
  if (*s == '-') { neg = true; s++; }
  for (; *s; s++) {
    if (*s == '.' && !dot) { dot = true; continue; }
    if (*s < '0' || *s > '9') return NAN;
    any = true;
    if (dot) { scaleF /= 10; v += (*s - '0') * scaleF; }
    else v = v * 10 + (*s - '0');
  }
  if (!any) return NAN;
  return neg ? -v : v;
}

void sendEvent(const char *type, float kg) {      // EV,OVER,0.20
  char b[40] = "EV,";
  strcat(b, type);
  strcat(b, ",");
  catNum(b, kg, 3);
  sendLine(b);
}

// =====================================================================
// Cân: lọc median 5 + trung bình trượt 10, phát hiện ổn định
// =====================================================================
float med5[5];   uint8_t mi = 0, mn = 0;
float avg10[10]; uint8_t ai = 0, an = 0;

float median5() {
  float t[5];
  for (uint8_t i = 0; i < mn; i++) t[i] = med5[i];
  for (uint8_t i = 1; i < mn; i++) {
    float v = t[i]; int8_t j = i - 1;
    while (j >= 0 && t[j] > v) { t[j + 1] = t[j]; j--; }
    t[j + 1] = v;
  }
  return t[mn / 2];
}

float pushSample(float kg) {
  med5[mi] = kg; mi = (mi + 1) % 5; if (mn < 5) mn++;
  avg10[ai] = median5(); ai = (ai + 1) % 10; if (an < 10) an++;
  float s = 0;
  for (uint8_t i = 0; i < an; i++) s += avg10[i];
  return s / an;
}

void resetFilter() { mi = mn = ai = an = 0; stableSince = millis(); }

bool isStable() { return an >= 10 && millis() - stableSince >= STABLE_MS; }

void startOp(Op o, float known) { op = o; opN = 0; opSum = 0; opKnown = known; }

// Kết thúc TARE/CAL sau khi đã gom đủ mẫu
void finishOp() {
  long avg = opSum / opN;
  if (op == OP_TARE) {
    scale.set_offset(avg);
    sendLine("OK,TARE");
  } else {
    float f = (float)(avg - scale.get_offset()) / opKnown;
    if (fabs(f) < 1.0f) {
      sendLine("ERR,CAL");
    } else {
      scale.set_scale(f);
      EEPROM.put(ADDR_FACTOR, f);
      char b[32] = "OK,CAL,";
      catNum(b, f, 2);
      sendLine(b);
    }
  }
  op = OP_NONE;
  resetFilter();
}

void readScale() {
  if (!scale.is_ready()) return;               // chưa có mẫu mới → trả về ngay
  long raw = scale.read();
  if (raw >= 8388607L || raw <= -8388608L) return;   // bão hòa = lỗi → để timeout báo ERROR
  lastSampleMs = millis();

  if (op != OP_NONE) {
    opSum += raw;
    if (++opN >= (op == OP_TARE ? TARE_SAMPLES : CAL_SAMPLES)) finishOp();
    if (op == OP_TARE) return;                   // offset chưa đúng → chưa cập nhật số kg
  }

  float kg = (raw - scale.get_offset()) / scale.get_scale();
  wFilt = pushSample(kg);
  if (fabs(wFilt - stableRef) > STABLE_BAND) { stableRef = wFilt; stableSince = millis(); }
}

void autoZero() {   // bù trôi khi bàn cân trống lâu + tự cân bằng khi số kg âm
  if (state != S_IDLE || op != OP_NONE || !isStable()) return;
  if (wFilt < -NEG_ZERO_KG) {                  // âm rõ (vd. TARE lúc còn vật đè, trôi nhiệt): TARE lại không chặn
    if (millis() - stableSince < NEG_ZERO_MS) return;
    char b[28] = "EV,AUTOZERO,";
    catNum(b, wFilt, 3);
    sendLine(b);
    startOp(OP_TARE, 0);
    return;
  }
  if (millis() - stableSince < ZERO_TRACK_MS) return;
  if (fabs(wFilt) < 0.001f || wFilt >= ZERO_TRACK_KG) return;   // dư dương sau khi xe rời / trôi nhiệt
  scale.set_offset(scale.get_offset() + (long)(wFilt * scale.get_scale()));
  resetFilter();
}

// =====================================================================
// Servo – Timer1 phát xung bằng phần cứng; tự detach sau 500 ms (hết rè, không tốn điện)
// =====================================================================
#if USE_SERVO
Servo gate;
unsigned long detachAt = 0;
#endif

bool servoBusy() {
#if USE_SERVO
  return gate.attached();
#else
  return false;
#endif
}

void moveGate(bool open) {
  gateOpen = open;
#if USE_SERVO
  if (!gate.attached()) gate.attach(PIN_SERVO, 500, 2400);
  gate.write(open ? GATE_OPEN : GATE_CLOSED);
  detachAt = millis() + 500;
  if (detachAt == 0) detachAt = 1;
#endif
}

void servoTask() {
#if USE_SERVO
  if (detachAt && (long)(millis() - detachAt) >= 0) { gate.detach(); detachAt = 0; }
#endif
}

// =====================================================================
// WS2812B – hiệu ứng theo trạng thái, chỉ gọi show() khi màu thay đổi
// =====================================================================
#if USE_LEDS
Adafruit_NeoPixel px(4, PIN_WS, NEO_GRB + NEO_KHZ800);
#endif

bool sleeping() { return state == S_IDLE && millis() - lastActivity >= SLEEP_MS; }

void ledTask() {
#if USE_LEDS
  static uint32_t shown[4] = {1, 1, 1, 1};
  static State lastState = S_ERROR;
  uint32_t c[4];
  unsigned long now = millis();

  switch (state) {
    case S_IDLE: {                                    // vòng quay "loading" vàng, đuôi mờ dần (khác xanh lá = hợp lệ)
      if (sleeping()) {                               // ngủ: chỉ LED0 nháy mờ
        for (uint8_t i = 0; i < 4; i++) c[i] = 0;
        if (now % 3000 < 80) c[0] = px.Color(25, 17, 0);
        break;
      }
      static const uint8_t TRAIL[4] = {150, 50, 15, 3};   // đầu sáng nhất, đuôi mờ dần
      uint8_t head = (now / SPIN_MS) % 4;
      for (uint8_t i = 0; i < 4; i++)
        c[i] = px.Color(TRAIL[(head + 4 - i) % 4], TRAIL[(head + 4 - i) % 4] * 2 / 3, 0);  // vàng; (head - i) mod 4 = LED i cách đầu bao xa
      break;
    }
    case S_WEIGHING: {                                // xanh lơ chạy nhanh: đang cân (vàng đã dùng cho chờ xe)
      uint8_t k = (now / 150) % 4;
      for (uint8_t i = 0; i < 4; i++) c[i] = i == k ? px.Color(0, 130, 150) : px.Color(0, 15, 18);
      break;
    }
    case S_PASS:
      for (uint8_t i = 0; i < 4; i++) c[i] = px.Color(0, 150, 0);
      break;
    case S_OVER: {                                    // đỏ nhấp nháy 2 Hz
      bool on = (now / 250) % 2 == 0;
      for (uint8_t i = 0; i < 4; i++) c[i] = on ? px.Color(180, 0, 0) : 0;
      break;
    }
    case S_MANUAL:                                    // xanh dương; cổng mở thì 2 LED giữa xanh lá
      for (uint8_t i = 0; i < 4; i++)
        c[i] = (gateOpen && (i == 1 || i == 2)) ? px.Color(0, 150, 0) : px.Color(0, 0, 160);
      break;
    case S_ERROR: {                                   // tím nháy chậm
      bool on = (now / 500) % 2 == 0;
      for (uint8_t i = 0; i < 4; i++) c[i] = on ? px.Color(120, 0, 120) : 0;
      break;
    }
  }

  // show() tắt ngắt ~120 µs → khi servo đang chạy chỉ cập nhật lúc đổi trạng thái để servo không rung
  bool changed = state != lastState;
  if (servoBusy() && !changed) return;
  bool diff = changed;
  for (uint8_t i = 0; i < 4; i++) if (c[i] != shown[i]) diff = true;
  // Gửi lại khung đúng định kỳ: nhiễu xung trên dây DIN (servo, dây dài) làm LED nhận sai màu
  // thì tối đa LED_REFRESH_MS sau sẽ được sửa lại, không bị giữ màu sai tới lần đổi trạng thái sau
  static unsigned long lastShow = 0;
  if (!diff && now - lastShow < LED_REFRESH_MS) return;
  for (uint8_t i = 0; i < 4; i++) { px.setPixelColor(i, c[i]); shown[i] = c[i]; }
  px.show();
  lastShow = now;
  lastState = state;
#endif
}

unsigned long weighStart = 0;   // lúc phát hiện xe (vào WEIGHING)
bool settled = false;           // đã hết thời gian chờ ổn định SETTLE_MS

void setState(State s) {
  state = s;
  belowSince = 0;
  lastActivity = millis();
}

// =====================================================================
// LED matrix – mỗi trạng thái một dòng chữ chạy (không dấu, font ASCII)
// =====================================================================
#if USE_MATRIX
MD_Parola mx(MD_MAX72XX::FC16_HW, PIN_MX_DIN, PIN_MX_CLK, PIN_MX_CS, 4);
char msg[72];
#endif

void fmtKg(char *out, float kg) {
  if (kg > -0.0005f && kg < 0.0005f) kg = 0;    // tránh "-0.000"
  if (kg > 99.99f) kg = 99.99f;
  if (kg < -9.99f) kg = -9.99f;
  dtostrf(kg, 0, 3, out);
  strcat(out, "kg");
}

#if USE_MATRIX
void buildMsg() {
  char num[14];
  switch (state) {
    case S_IDLE:
      strcpy_P(msg, PSTR("TRAM CAN PTIT * XIN MOI XE TIEP THEO VAO"));
      break;
    case S_WEIGHING:
      strcpy_P(msg, PSTR("DANG TRONG QUA TRINH CAN... "));
      fmtKg(num, wFilt);
      strcat(msg, num);
      break;
    case S_PASS:
      strcpy_P(msg, PSTR("HOP LE "));
      fmtKg(num, decidedKg);
      strcat(msg, num);
      strcat_P(msg, PSTR(" * MOI XE DI QUA"));
      break;
    case S_OVER:
      strcpy_P(msg, PSTR("QUA TAI "));
      fmtKg(num, decidedKg);
      strcat(msg, num);
      strcat_P(msg, PSTR(" > "));
      fmtKg(num, threshold);
      strcat(msg, num);
      strcat_P(msg, PSTR(" * XE KHONG DUOC QUA"));
      break;
    case S_MANUAL:
      strcpy_P(msg, gateOpen ? PSTR("CHE DO TAY * CONG MO") : PSTR("CHE DO TAY * CONG DONG"));
      break;
    case S_ERROR:
      strcpy_P(msg, PSTR("LOI CAM BIEN CAN * VUI LONG KIEM TRA"));
      break;
  }
}
#endif

void displayTask() {
#if USE_MATRIX
  static State shownState = S_ERROR;
  static bool shownGate = false, shownSleep = false, first = true;

  bool slp = sleeping();
  if (slp != shownSleep) { shownSleep = slp; mx.setIntensity(slp ? 0 : 2); }

  // Đổi trạng thái → bỏ dòng đang chạy, hiện ngay dòng mới
  bool restart = first || state != shownState || (state == S_MANUAL && gateOpen != shownGate);
  if (restart) { first = false; shownState = state; shownGate = gateOpen; mx.displayClear(); }

  // displayAnimate() tự canh thời gian từng cột; trả true khi chạy xong 1 lượt → dựng lại (cập nhật số kg)
  if (mx.displayAnimate() || restart) {
    buildMsg();
    mx.displayText(msg, PA_LEFT, SCROLL_MS, 0, PA_SCROLL_LEFT, PA_SCROLL_LEFT);
  }
#endif
}

// =====================================================================
// Joystick – long-press vào/ra MANUAL, gạt trái/phải đóng/mở cổng
// =====================================================================
#if USE_JOY
int jsCenter = 512;
#endif

void joyTask() {
#if USE_JOY
  static unsigned long pressStart = 0, lastChange = 0, dirSince = 0;
  static bool lastRaw = false, pressed = false, fired = false;
  static int8_t dir = 0;

  bool raw = digitalRead(PIN_JS_SW) == LOW;
  if (raw != lastRaw) { lastRaw = raw; lastChange = millis(); }
  if (millis() - lastChange > 30 && raw != pressed) {        // chống dội 30 ms
    pressed = raw;
    pressStart = millis();
    fired = false;
    lastActivity = millis();
  }
  if (pressed && !fired && millis() - pressStart >= LONG_PRESS_MS) {
    fired = true;
    if (state == S_IDLE) {
      setState(S_MANUAL);
      sendLine("EV,MANUAL,ENTER");
    } else if (state == S_MANUAL) {
      moveGate(false);
      setState(S_IDLE);
      sendLine("EV,MANUAL,EXIT");
    }
  }

  if (state != S_MANUAL) { dir = 0; return; }
  int v = analogRead(PIN_JS_GATE) - jsCenter;
  if (JS_INVERT) v = -v;
  int8_t d = v > JS_DEAD ? 1 : (v < -JS_DEAD ? -1 : 0);
  if (d != dir) { dir = d; dirSince = millis(); return; }
  if (d == 0 || millis() - dirSince < JS_HOLD_MS) return;     // chưa giữ đủ lâu → bỏ qua
  if (d > 0 && !gateOpen) {
    moveGate(true);
    sendLine("EV,MANUAL,OPEN");
  } else if (d < 0 && gateOpen) {
    moveGate(false);
    sendLine("EV,MANUAL,CLOSE");
  }
#endif
}

// =====================================================================
// Lệnh từ host
// =====================================================================
char rx[32];
uint8_t rxLen = 0;

void handleCmd(char *c) {
  lastActivity = millis();
  if (!strcmp(c, "PING")) { sendLine("PONG"); return; }

  if (!strncmp(c, "TH,", 3)) {
    float v = parseNum(c + 3);
    if (v >= 0.01f && v < 20.0f) {        // NAN cũng bị loại ở đây
      threshold = v;
      EEPROM.put(ADDR_TH, threshold);
      char b[24] = "OK,TH,";
      catNum(b, v, 3);
      sendLine(b);
    } else sendLine("ERR,TH");
    return;
  }

  if (!strcmp(c, "TARE")) {
    if ((state != S_IDLE && state != S_MANUAL) || op != OP_NONE) { sendLine("ERR,STATE"); return; }
    startOp(OP_TARE, 0);                  // trả OK,TARE khi gom đủ mẫu (~1 s), không chặn
    return;
  }

  if (!strncmp(c, "CAL,", 4)) {           // vào MANUAL, đặt vật chuẩn, rồi CAL,<kg>
    float known = parseNum(c + 4);
    if (state != S_MANUAL || op != OP_NONE) { sendLine("ERR,STATE"); return; }
    if (!(known > 0)) { sendLine("ERR,CAL"); return; }
    startOp(OP_CAL, known);
    return;
  }

  if (!strcmp(c, "GATE,OPEN"))  { moveGate(true);  sendLine("OK,GATE,OPEN");  return; }
  if (!strcmp(c, "GATE,CLOSE")) { moveGate(false); sendLine("OK,GATE,CLOSE"); return; }

  if (!strcmp(c, "INFO")) {               // INFO,<ngưỡng>,<factor>,<state>,<loop max µs>
    char b[56] = "INFO,";
    catNum(b, threshold, 3);
    strcat(b, ",");
    catNum(b, scale.get_scale(), 2);
    strcat(b, ",");
    strcat(b, STATE_NAME[state]);
    strcat(b, ",");
    ultoa(loopMaxUs, b + strlen(b), 10);
    sendLine(b);
    loopMaxUs = 0;
    return;
  }

  sendLine("ERR,CMD");
}

void cmdTask() {
  while (Serial.available()) {
    char c = Serial.read();
    if (c == '\r') continue;
    if (c == '\n') {
      rx[rxLen] = 0;
      if (rxLen) handleCmd(rx);
      rxLen = 0;
    } else if (rxLen < sizeof(rx) - 1) {
      rx[rxLen++] = c;
    } else {
      rxLen = 0;                                   // dòng quá dài → bỏ
    }
  }
}

// =====================================================================
// State machine
// =====================================================================
void stateTask() {
  unsigned long now = millis();
  bool hxDead = now - lastSampleMs > HX_TIMEOUT_MS;

  if (hxDead && state != S_ERROR && state != S_MANUAL) {
    moveGate(false);
    setState(S_ERROR);
    sendLine("EV,ERR,HX711");
    return;
  }
  if (op == OP_TARE) return;                       // đang lấy mốc 0 → chưa quyết định gì

  switch (state) {
    case S_ERROR:
      if (!hxDead) { resetFilter(); setState(S_IDLE); }
      break;

    case S_IDLE:
      if (wFilt > W_ENTER) { setState(S_WEIGHING); weighStart = now; settled = false; }
      break;

    case S_WEIGHING:
      if (wFilt < W_EXIT) { setState(S_IDLE); break; }
      if (!settled) {                              // đang chờ xe ổn định – chưa đo
        if (now - weighStart < SETTLE_MS) break;
        settled = true;
        resetFilter();                             // bỏ số liệu lúc xe đang lăn vào, đo lại từ đầu
        break;
      }
      if (isStable()) {
        decidedKg = wFilt;
        if (decidedKg > threshold) {
          moveGate(false);
          setState(S_OVER);
          sendEvent("OVER", decidedKg);
        } else {
          moveGate(true);
          setState(S_PASS);
          sendEvent("PASS", decidedKg);
        }
      }
      break;

    case S_PASS:
    case S_OVER:
      if (wFilt < max(W_EXIT, decidedKg * EXIT_RATIO)) {
        if (!belowSince) belowSince = now;
        else if (now - belowSince >= LEAVE_MS) {
          moveGate(false);
          sendLine("EV,LEAVE");
          setState(S_IDLE);
        }
      } else {
        belowSince = 0;
      }
      break;

    case S_MANUAL:
      break;
  }
}

void telemetryTask() {             // W,<kg>,<state>,<cổng: 1 = mở, 2 = đóng>
  char b[40] = "W,";
  catNum(b, wFilt, 3);
  strcat(b, ",");
  strcat(b, STATE_NAME[state]);
  strcat(b, gateOpen ? ",1" : ",2");
  sendLine(b);
}

// =====================================================================
// Scheduler: chu kỳ 0 = chạy mỗi vòng (task tự kiểm tra có việc hay không)
// =====================================================================
struct Task {
  void (*fn)();
  uint16_t periodMs;
  unsigned long last;
};

Task TASKS[] = {
  {readScale,     0,            0},   // HX711 báo sẵn sàng ~10 lần/s
  {cmdTask,       0,            0},   // lệnh từ host
  {displayTask,   0,            0},   // Parola tự canh nhịp chạy chữ
  {joyTask,       10,           0},
  {stateTask,     10,           0},
  {servoTask,     10,           0},
  {ledTask,       20,           0},   // 50 Hz hiệu ứng LED
  {telemetryTask, TELEMETRY_MS, 0},   // W,<kg>,<state> 5 lần/s
  {autoZero,      1000,         0},
};
const uint8_t NUM_TASKS = sizeof(TASKS) / sizeof(TASKS[0]);

// =====================================================================
void setup() {
  Serial.begin(115200);              // KHÔNG while(!Serial): phải chạy được khi chưa có host
  pinMode(PIN_JS_SW, INPUT_PULLUP);

#if USE_LEDS
  px.begin();
  px.setBrightness(80);
  px.show();
#endif
#if USE_MATRIX
  mx.begin();
  mx.setIntensity(2);
#endif
#if USE_JOY
  jsCenter = analogRead(PIN_JS_GATE);   // lúc khởi động không ai chạm joystick
#endif
  moveGate(false);

  float f;
  EEPROM.get(ADDR_FACTOR, f);
  if (isnan(f) || f == 0) f = 1.0f;  // chưa hiệu chuẩn → số đọc là raw, phải chạy t01b trước
  EEPROM.get(ADDR_TH, threshold);
  if (isnan(threshold) || threshold < 0.01f || threshold >= 20.0f) threshold = DEFAULT_TH;

  scale.begin(PIN_HX_DT, PIN_HX_SCK);
  scale.set_scale(f);
  startOp(OP_TARE, 0);               // lấy mốc 0 không chặn – bàn cân phải trống khi khởi động

  lastSampleMs = lastActivity = millis();
  setState(S_IDLE);
}

void loop() {
  unsigned long t0 = micros();
  unsigned long now = millis();
  for (uint8_t i = 0; i < NUM_TASKS; i++) {
    Task &t = TASKS[i];
    if (t.periodMs && now - t.last < t.periodMs) continue;
    t.last = now;
    t.fn();
  }
  unsigned long dt = micros() - t0;
  if (dt > loopMaxUs) loopMaxUs = dt;
}
