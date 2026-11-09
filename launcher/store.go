package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite thuần Go – build CGO_ENABLED=0, không cần thư viện trên board
)

// Store – ghi sự kiện vào SQLite trên eMMC. Cột tg_* là hàng đợi (outbox) gửi Telegram:
// mất mạng thì sự kiện vẫn nằm trong DB, có mạng lại TelegramWorker gửi bù.
type Store struct {
	db  *sql.DB
	loc *time.Location
	mu  sync.Mutex
}

// Event – một lượt xe / thao tác tay / lỗi.
type Event struct {
	ID          uint
	Ts          time.Time
	WeightKg    float64
	ThresholdKg float64
	Status      string // PASS | OVER | MANUAL_ON | MANUAL_OFF | MANUAL_OPEN | MANUAL_CLOSE | ERR
	ImagePath   string // đường dẫn tương đối trong ImageDir
	TgRetries   int
}

const tsLayout = "2006-01-02T15:04:05.000"

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  ts           TEXT    NOT NULL,
  leave_ts     TEXT,
  weight_kg    REAL    NOT NULL,
  threshold_kg REAL    NOT NULL,
  status       TEXT    NOT NULL,
  image_path   TEXT    NOT NULL DEFAULT '',
  tg_sent      INTEGER NOT NULL DEFAULT 0,
  tg_retries   INTEGER NOT NULL DEFAULT 0,
  tg_next_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_tg ON events(tg_sent, status);
`

func openStore(path string, loc *time.Location) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: 1 kết nối ghi, tránh "database is locked"
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("tạo bảng: %w", err)
	}
	return &Store{db: db, loc: loc}, nil
}

// Ready – SQLite cục bộ luôn sẵn sàng (giữ hàm để TelegramWorker không phụ thuộc kiểu lưu trữ).
func (s *Store) Ready() bool { return true }

// Insert ghi 1 sự kiện; trả id (0 nếu lỗi ghi).
func (s *Store) Insert(ts time.Time, kg, th float64, status, img string) uint {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec("INSERT INTO events(ts, weight_kg, threshold_kg, status, image_path) VALUES (?,?,?,?,?)",
		ts.In(s.loc).Format(tsLayout), kg, th, status, img)
	if err != nil {
		logErr("ghi sự kiện "+status, err)
		return 0
	}
	id, _ := res.LastInsertId()
	return uint(id)
}

// SetLeave ghi giờ xe rời cân vào lượt PASS/OVER gần nhất chưa có giờ ra.
func (s *Store) SetLeave(ts time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE events SET leave_ts = ? WHERE id = (
		SELECT id FROM events WHERE status IN ('PASS','OVER') AND leave_ts IS NULL ORDER BY id DESC LIMIT 1)`,
		ts.In(s.loc).Format(tsLayout))
	if err != nil {
		logErr("ghi giờ xe rời cân", err)
	}
}

func (s *Store) parseTs(v string) time.Time {
	t, err := time.ParseInLocation(tsLayout, v, s.loc)
	if err != nil {
		t, _ = time.ParseInLocation("2006-01-02T15:04:05", v, s.loc)
	}
	return t
}

// ---- Hàng đợi Telegram (outbox) ----

const tgMaxRetries = 10

func (s *Store) PendingTelegram(now time.Time, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, ts, weight_kg, threshold_kg, status, image_path, tg_retries FROM events
		WHERE status IN ('OVER','MANUAL_ON','MANUAL_OFF') AND tg_sent = 0 AND tg_retries < ? AND tg_next_at <= ?
		ORDER BY ts, id LIMIT ?`, tgMaxRetries, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []Event
	for rows.Next() {
		var ev Event
		var ts string
		if err := rows.Scan(&ev.ID, &ts, &ev.WeightKg, &ev.ThresholdKg, &ev.Status, &ev.ImagePath, &ev.TgRetries); err != nil {
			return nil, err
		}
		ev.Ts = s.parseTs(ts)
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}

func (s *Store) MarkSent(id uint) error {
	_, err := s.db.Exec("UPDATE events SET tg_sent = 1 WHERE id = ?", id)
	return err
}

func (s *Store) MarkFailed(id uint, retries int, next time.Time) error {
	_, err := s.db.Exec("UPDATE events SET tg_retries = ?, tg_next_at = ? WHERE id = ?", retries, next.Unix(), id)
	return err
}

// ManualSession: phiên thủ công kết thúc ở sự kiện offID → (lúc bật, số lần mở cổng); ok=false nếu không tìm thấy lúc bật.
func (s *Store) ManualSession(offID uint) (on time.Time, opens int64, ok bool) {
	var onID uint
	var ts string
	err := s.db.QueryRow("SELECT id, ts FROM events WHERE status = 'MANUAL_ON' AND id < ? ORDER BY id DESC LIMIT 1",
		offID).Scan(&onID, &ts)
	if err != nil {
		return
	}
	s.db.QueryRow("SELECT COUNT(*) FROM events WHERE status = 'MANUAL_OPEN' AND id > ? AND id < ?",
		onID, offID).Scan(&opens)
	return s.parseTs(ts), opens, true
}
