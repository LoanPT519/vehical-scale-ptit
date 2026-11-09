package main

import (
	"bytes"
	"log"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// SerialLink – kết nối serial với MCU (Leonardo, 115200 8N1 raw), tự mở lại khi rút cáp / MCU reset / nạp firmware.
type SerialLink struct {
	port      string
	onConnect func(*SerialLink)
	mu        sync.Mutex // fd + ghi
	fd        int
	buf       []byte
}

func newSerial(port string, onConnect func(*SerialLink)) *SerialLink {
	return &SerialLink{port: port, onConnect: onConnect, fd: -1}
}

func (s *SerialLink) open() {
	for {
		fd, err := unix.Open(s.port, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err == nil {
			err = setRaw115200(fd)
			if err != nil {
				unix.Close(fd)
			}
		}
		if err == nil {
			s.mu.Lock()
			s.fd, s.buf = fd, s.buf[:0]
			s.mu.Unlock()
			log.Printf("Đã mở %s", s.port)
			if s.onConnect != nil {
				s.onConnect(s)
			}
			return
		}
		log.Printf("Chưa mở được %s (%v), thử lại sau 2 s", s.port, err)
		time.Sleep(2 * time.Second)
	}
}

func setRaw115200(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag, t.Oflag, t.Lflag = 0, 0, 0
	t.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL | unix.B115200
	t.Ispeed, t.Ospeed = unix.B115200, unix.B115200
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return err
	}
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}

func (s *SerialLink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd >= 0 {
		unix.Close(s.fd)
	}
	s.fd = -1
}

// Run gọi handle cho từng dòng từ MCU; handle("") mỗi khi 1 s không có dữ liệu (để kiểm tra heartbeat).
func (s *SerialLink) Run(handle func(string)) {
	chunk := make([]byte, 512)
	for {
		if s.fd < 0 {
			s.open()
		}
		if i := bytes.IndexByte(s.buf, '\n'); i >= 0 {
			line := string(bytes.TrimSpace(s.buf[:i]))
			s.buf = s.buf[i+1:]
			handle(line)
			continue
		}
		fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 1000)
		if err == unix.EINTR {
			continue
		}
		if err == nil && n == 0 {
			handle("")
			continue
		}
		var m int
		if err == nil {
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 && fds[0].Revents&unix.POLLIN == 0 {
				err = unix.EIO
			} else {
				m, err = unix.Read(s.fd, chunk)
				if err == nil && m == 0 {
					err = unix.EIO // EOF – thiết bị đã ngắt
				}
			}
		}
		if err == unix.EAGAIN {
			continue
		}
		if err != nil {
			log.Printf("Mất kết nối serial: %v", err)
			s.close()
			time.Sleep(2 * time.Second)
			continue
		}
		s.buf = append(s.buf, chunk[:m]...)
		if len(s.buf) > 4096 { // rác không có xuống dòng → bỏ
			s.buf = s.buf[:0]
		}
	}
}

func (s *SerialLink) Send(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd < 0 {
		return
	}
	if _, err := unix.Write(s.fd, []byte(line+"\n")); err != nil {
		log.Printf("Gửi serial lỗi: %v", err)
	}
}
