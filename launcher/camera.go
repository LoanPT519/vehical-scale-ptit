package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Camera chạy nền: GStreamer lấy thẳng JPEG từ webcam (MJPEG, không giải mã → gần như không tốn CPU)
// và ghi vòng 4 frame mới nhất vào RAM (/dev/shm). Khi có sự kiện chỉ việc copy frame mới nhất
// → không có độ trễ mở camera.
type Camera struct {
	want, dir     string
	width, height int
	fps           int
	device        string
	cmd           *exec.Cmd
	exited        chan struct{}
	online        atomic.Bool
}

func newCamera(device, dir string) *Camera {
	return &Camera{want: device, dir: dir, width: 640, height: 480, fps: 5}
}

func (c *Camera) Online() bool { return c.online.Load() }

// findCamera: ưu tiên đường dẫn cố định theo serial (/dev/v4l/by-id) vì rút/cắm lại có thể đổi /dev/videoN.
func findCamera() string {
	if m, _ := filepath.Glob("/dev/v4l/by-id/*-video-index0"); len(m) > 0 {
		sort.Strings(m)
		return m[0]
	}
	devs, _ := filepath.Glob("/dev/video*")
	sort.Strings(devs)
	for _, d := range devs {
		b, err := os.ReadFile("/sys/class/video4linux/" + filepath.Base(d) + "/index")
		if err == nil && strings.TrimSpace(string(b)) == "0" { // index 0 = luồng hình
			return d
		}
	}
	return ""
}

func (c *Camera) resolve() string {
	if c.want != "" && c.want != "auto" {
		if _, err := os.Stat(c.want); err == nil {
			return c.want
		}
		return ""
	}
	return findCamera()
}

func (c *Camera) start() error {
	// Dọn pipeline cũ còn sót (vd service cũ) đang giữ camera / ghi vào cùng thư mục
	exec.Command("pkill", "-f", "multifilesink location="+c.dir).Run()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(c.dir, "cam_*.jpg"))
	for _, f := range old {
		os.Remove(f)
	}
	c.cmd = exec.Command("gst-launch-1.0", "-q", "v4l2src", "device="+c.device,
		"!", fmt.Sprintf("image/jpeg,width=%d,height=%d,framerate=%d/1", c.width, c.height, c.fps),
		"!", "multifilesink", "location="+filepath.Join(c.dir, "cam_%06d.jpg"), "max-files=4")
	// Service chết (kể cả kill -9) thì GStreamer cũng nhận SIGTERM → không còn pipeline mồ côi giữ camera
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := c.cmd.Start(); err != nil {
		return err
	}
	c.exited = make(chan struct{})
	go func(cmd *exec.Cmd, done chan struct{}) { cmd.Wait(); close(done) }(c.cmd, c.exited)
	return nil
}

func (c *Camera) stop() {
	if c.cmd == nil {
		return
	}
	select {
	case <-c.exited:
	default:
		c.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.exited:
		case <-time.After(3 * time.Second):
			c.cmd.Process.Kill()
			<-c.exited
		}
	}
	c.cmd = nil
}

type frame struct {
	path string
	mod  time.Time
}

// frames: các frame trong thư mục, cũ → mới
func frames(dir string) []frame {
	matches, _ := filepath.Glob(filepath.Join(dir, "cam_*.jpg"))
	var fs []frame
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil { // có thể vừa bị multifilesink xoá
			fs = append(fs, frame{m, st.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.Before(fs[j].mod) })
	return fs
}

func (c *Camera) setOnline(on bool, reason string) {
	if c.online.Swap(on) != on { // chỉ log khi đổi trạng thái
		if on {
			log.Printf("Camera online: %s", c.device)
		} else {
			log.Printf("Camera offline (%s) – tự thử kết nối lại mỗi 3 s", reason)
		}
	}
}

// Run – watchdog: chưa cắm camera → chờ; pipeline chết hoặc 5 s không có frame mới
// → dò lại thiết bị (có thể đã đổi tên sau khi cắm lại) và chạy lại.
func (c *Camera) Run() {
	var started time.Time
	for {
		if c.cmd == nil {
			c.device = c.resolve()
			if c.device == "" {
				c.setOnline(false, "không thấy camera USB")
				time.Sleep(3 * time.Second)
				continue
			}
			if err := c.start(); err != nil {
				c.setOnline(false, "không chạy được gst-launch: "+err.Error())
				c.cmd = nil
				time.Sleep(3 * time.Second)
				continue
			}
			started = time.Now()
		}
		time.Sleep(time.Second)
		fs := frames(c.dir)
		newest := started
		if len(fs) > 0 {
			newest = fs[len(fs)-1].mod
		}
		dead := false
		select {
		case <-c.exited:
			dead = true
		default:
		}
		if dead || time.Since(newest) > 5*time.Second {
			reason := "không có frame mới"
			if dead {
				reason = "pipeline dừng"
			}
			c.setOnline(false, reason)
			c.stop()
			time.Sleep(2 * time.Second)
		} else if len(fs) > 0 && time.Since(newest) < 2*time.Second {
			c.setOnline(true, "")
		}
	}
}

// latestFrame: frame hoàn chỉnh mới nhất (frame cuối có thể đang ghi dở → lấy frame áp chót)
func latestFrame(dir string) (frame, bool) {
	fs := frames(dir)
	switch {
	case len(fs) >= 2:
		return fs[len(fs)-2], true
	case len(fs) == 1:
		return fs[0], true
	}
	return frame{}, false
}

// Snapshot copy frame mới nhất (≤ 2 s tuổi) ra dest.
func (c *Camera) Snapshot(dest string) error {
	f, ok := latestFrame(c.dir)
	if !ok || time.Since(f.mod) > 2*time.Second {
		return fmt.Errorf("không có frame camera mới")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	src, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
