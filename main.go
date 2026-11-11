package main

// czgorex: a GoRex client for the CardputerZero panel.
//
// It draws straight into /dev/fb0 (no compositor, no GPU), reads the matrix
// keyboard from evdev, and talks to the local GoRex session server through the
// gorex CLI. The bottom bar follows CardputerZero's key guideline: five 22x22
// hints centred on x = 49, 105, 161, 217, 273 at y = 153, one per key 4..8,
// each with a short tick pointing at its key.

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"golang.org/x/sys/unix"
)

const (
	w = 320
	h = 170

	barTop    = 14  // status bar height
	lineH     = 13  // text line height
	textTop   = 26  // first terminal baseline
	rows      = 8   // terminal rows between the bars
	inputY    = 133 // input baseline
	iconY     = 153 // key hint centre, per the CardputerZero guideline
	iconSize  = 22  // hint size, per the guideline
	keyBarTop = iconY - iconSize/2
)

// Hint centres from the official guideline: keys 4, 5, 6, 7, 8.
var hintX = [5]int{49, 105, 161, 217, 273}

type framebuffer struct {
	f      *os.File
	buf    []byte
	stride int
	backup []byte
}

func openFB(path string) (*framebuffer, error) {
	stride := w * 2
	if b, err := os.ReadFile("/sys/class/graphics/fb0/stride"); err == nil {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n); err == nil && n >= w*2 {
			stride = n
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	buf, err := unix.Mmap(int(f.Fd()), 0, stride*h, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, err
	}
	fb := &framebuffer{f: f, buf: buf, stride: stride, backup: make([]byte, len(buf))}
	copy(fb.backup, buf)
	return fb, nil
}

func (fb *framebuffer) close() {
	copy(fb.buf, fb.backup)
	unix.Munmap(fb.buf)
	fb.f.Close()
}

func (fb *framebuffer) blit(img *image.RGBA) {
	for y := range h {
		row := img.Pix[y*img.Stride:]
		out := fb.buf[y*fb.stride:]
		for x := range w {
			r := uint16(row[x*4])
			g := uint16(row[x*4+1])
			b := uint16(row[x*4+2])
			c := r>>3<<11 | g>>2<<5 | b>>3
			out[x*2] = byte(c)
			out[x*2+1] = byte(c >> 8)
		}
	}
}

type inputEvent struct {
	Sec, Usec  int64
	Type, Code uint16
	Value      int32
}

type keyEvent struct {
	Code uint16
	Rune rune
}

func findKeyboard() string {
	b, err := os.ReadFile("/proc/bus/input/devices")
	if err != nil {
		return ""
	}
	name, handler := "", ""
	pick := func() string {
		if strings.Contains(name, "tca8418") {
			for _, f := range strings.Fields(handler) {
				if strings.HasPrefix(f, "event") {
					return "/dev/input/" + f
				}
			}
		}
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "N: Name="):
			name = line
		case strings.HasPrefix(line, "H: Handlers="):
			handler = line
		case line == "":
			if p := pick(); p != "" {
				return p
			}
			name, handler = "", ""
		}
	}
	return pick()
}

func readKeys(path string, keys chan<- keyEvent, quit chan<- struct{}) {
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		fmt.Println("open key device:", err)
		return
	}
	defer unix.Close(fd)
	var buf [24]byte
	shift := false
	for {
		n, err := unix.Read(fd, buf[:])
		if err != nil || n != 24 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		ev := *(*inputEvent)(unsafe.Pointer(&buf[0]))
		if ev.Type != 1 {
			continue
		}
		switch ev.Code {
		case keyLeftShift, keyRightShift:
			shift = ev.Value != 0
			continue
		case keyEsc:
			if ev.Value == 1 {
				close(quit)
				return
			}
			continue
		}
		if ev.Value != 1 {
			continue
		}
		r, _ := keyRune(ev.Code, shift)
		keys <- keyEvent{Code: ev.Code, Rune: r}
	}
}

// client wraps the gorex CLI against the local session server.
type client struct {
	bin string
	env []string
}

func (c *client) run(args ...string) (string, error) {
	cmd := exec.Command(c.bin, args...)
	cmd.Env = c.env
	out, err := cmd.Output()
	return string(out), err
}

func (c *client) session() (string, error) {
	if out, err := c.run("ls"); err == nil {
		for _, line := range strings.Split(out, "\n")[1:] {
			if f := strings.Fields(line); len(f) > 0 {
				return f[0], nil
			}
		}
	}
	out, err := c.run("new", "--dir", os.Getenv("HOME"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0]), nil
}

func (c *client) capture(id string) (string, error) {
	return c.run("capture", "-s", id, "--history", "65536")
}

func (c *client) send(id, text string) error {
	_, err := c.run("send", "-s", id, "--enter", text)
	return err
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[@-_]`)

func screenLines(raw string) []string {
	clean := ansi.ReplaceAllString(strings.ReplaceAll(raw, "\r\n", "\n"), "")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	lines := strings.Split(clean, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type app struct {
	buf    []rune
	lines  []string
	offset int // lines scrolled back from the tail
	status string

	session string
	bg      *image.RGBA
}

var (
	colBG     = color.RGBA{10, 16, 30, 255}
	colBar    = color.RGBA{26, 40, 66, 255}
	colInBar  = color.RGBA{20, 30, 48, 255}
	colText   = color.RGBA{232, 238, 250, 255}
	colDim    = color.RGBA{140, 160, 190, 255}
	colAccent = color.RGBA{120, 220, 255, 255}
)

func (a *app) draw() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if a.bg == nil {
		a.bg = image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			row := a.bg.Pix[y*a.bg.Stride:]
			for x := range w {
				row[x*4+0] = uint8(8 + y/6)
				row[x*4+1] = uint8(14 + y/8)
				row[x*4+2] = uint8(26 + x/16)
				row[x*4+3] = 255
			}
		}
	}
	copy(img.Pix, a.bg.Pix)

	white := image.NewUniform(colText)
	dim := image.NewUniform(colDim)
	accent := image.NewUniform(colAccent)

	// status bar
	draw.Draw(img, image.Rect(0, 0, w, barTop), image.NewUniform(colBar), image.Point{}, draw.Src)
	short := a.session
	if len(short) > 8 {
		short = short[:8]
	}
	left := "GoRex " + short
	if a.status != "" {
		left += " " + a.status
	}
	text(img, white, 3, 11, truncate(left, 28))
	right := fmt.Sprintf("%d/%d %s", a.offset, len(a.lines), time.Now().Format("15:04"))
	text(img, dim, w-len(right)*7-3, 11, right)

	// terminal window
	end := len(a.lines) - a.offset
	if end > len(a.lines) {
		end = len(a.lines)
	}
	start := end - rows
	if start < 0 {
		start = 0
	}
	for i, line := range a.lines[start:end] {
		text(img, white, 4, textTop+i*lineH, truncate(line, 45))
	}

	// input line
	draw.Draw(img, image.Rect(0, inputY-11, w, inputY+4), image.NewUniform(colInBar), image.Point{}, draw.Src)
	text(img, accent, 3, inputY, truncate("> "+string(a.buf), 44))

	// key hints: five 22x22 icons over keys 4..8, each with a tick below
	a.drawHints(img, accent)
	return img
}

// drawHints follows the CardputerZero guideline: icons 22x22 centred at
// y = 153 on x = 49, 105, 161, 217, 273, with a short vertical tick from the
// bottom of each icon down to the screen edge.
func (a *app) drawHints(img *image.RGBA, accent *image.Uniform) {
	kinds := [5]int{hintUp, hintPageUp, hintEnter, hintPageDown, hintDown}
	for i, cx := range hintX {
		drawHint(img, cx, iconY, kinds[i], accent)
		// tick pointing at the key below
		rect(img, cx-1, iconY+iconSize/2+1, cx+2, h-1, accent.C)
	}
}

const (
	hintUp = iota
	hintDown
	hintPageUp
	hintPageDown
	hintEnter
)

func rect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(c), image.Point{}, draw.Src)
}

// tri draws a filled isosceles triangle: apex at (cx, y) for dir < 0 or the
// flat edge there for dir > 0, of the given width and height.
func tri(img *image.RGBA, cx, y, width, height, dir int, c color.Color) {
	for i := range height {
		half := (width / 2) * (i + 1) / height
		yy := y + i
		if dir > 0 {
			yy = y + height - 1 - i
		}
		rect(img, cx-half, yy, cx+half+1, yy+1, c)
	}
}

// drawHint paints one 22x22 glyph centred on (cx, cy).
func drawHint(img *image.RGBA, cx, cy, kind int, c *image.Uniform) {
	col := c.C
	// half-open rectangles put a 22px box at x0..x0+21; +1 keeps the
	// painted centre on the guideline's x instead of half a pixel left of it.
	x0, y0 := cx-iconSize/2+1, cy-iconSize/2
	rect(img, x0, y0, x0+iconSize, y0+iconSize, color.RGBA{18, 28, 46, 255})
	switch kind {
	case hintUp:
		tri(img, cx, cy-8, 14, 7, -1, col)
		rect(img, cx-1, cy-2, cx+2, cy+8, col)
	case hintDown:
		tri(img, cx, cy-5, 14, 7, 1, col)
		rect(img, cx-1, cy-9, cx+2, cy-4, col)
	case hintPageUp:
		rect(img, cx-7, cy-9, cx+8, cy-7, col)
		tri(img, cx, cy-5, 12, 6, -1, col)
		rect(img, cx-1, cy, cx+2, cy+8, col)
	case hintPageDown:
		rect(img, cx-7, cy+7, cx+8, cy+9, col)
		tri(img, cx, cy-4, 12, 6, 1, col)
		rect(img, cx-1, cy-9, cx+2, cy-3, col)
	case hintEnter:
		rect(img, cx-7, cy+2, cx+7, cy+4, col)
		rect(img, cx+5, cy-6, cx+7, cy+4, col)
		for i := range 4 { // arrowhead pointing left
			rect(img, cx-9+i, cy+3-i, cx-8+i, cy+4+i, col)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "~"
	}
	return s
}

func text(img *image.RGBA, c *image.Uniform, x, y int, s string) {
	d := &font.Drawer{Dst: img, Src: c, Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func main() {
	fbPath := flag.String("fb", "/dev/fb0", "framebuffer device")
	keyPath := flag.String("key", "", "evdev device; empty finds the tca8418 keyboard")
	shot := flag.String("shot", "", "write a PNG of each frame here")
	bin := flag.String("gorex", "", "gorex executable; empty uses ~/.local/bin/gorex")
	session := flag.String("session", "", "session id; empty uses the first one or creates it")
	secs := flag.Int("secs", 0, "exit after this many seconds (0 = run until ESC)")
	flag.Parse()

	lock, err := os.OpenFile("/tmp/czgorex.lock", os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		fmt.Println("lock:", err)
		os.Exit(1)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		fmt.Println("another czgorex already owns the panel")
		os.Exit(1)
	}
	defer lock.Close()

	fb, err := openFB(*fbPath)
	if err != nil {
		fmt.Println("open framebuffer:", err)
		os.Exit(1)
	}
	defer fb.close()

	key := *keyPath
	if key == "" {
		if key = findKeyboard(); key == "" {
			fmt.Println("no keyboard device found; pass -key")
			os.Exit(1)
		}
	}

	exe := *bin
	if exe == "" {
		exe = filepath.Join(os.Getenv("HOME"), ".local", "bin", "gorex")
	}
	c := &client{
		bin: exe,
		env: append(os.Environ(), "GOREX_DIR="+filepath.Join(os.Getenv("HOME"), ".gorex")),
	}
	id := *session
	if id == "" {
		if id, err = c.session(); err != nil {
			fmt.Println("session:", err)
			os.Exit(1)
		}
	}
	fmt.Println("session:", id)

	a := &app{session: id, status: "connecting"}

	keys := make(chan keyEvent, 64)
	quit := make(chan struct{})
	go readKeys(key, keys, quit)

	screens := make(chan []string, 4)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
			}
			out, err := c.capture(id)
			if err != nil {
				screens <- nil
			} else {
				screens <- screenLines(out)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	deadline := time.Now().Add(time.Duration(*secs) * time.Second)
	tick := time.NewTicker(time.Second)
	render := func() {
		img := a.draw()
		fb.blit(img)
		if *shot != "" {
			if f, err := os.Create(*shot); err == nil {
				png.Encode(f, img)
				f.Close()
			}
			os.WriteFile(*shot+".fb", fb.buf, 0o644)
		}
	}

	scroll := func(delta int) {
		a.offset += delta
		max := len(a.lines) - 1
		if max < 0 {
			max = 0
		}
		if a.offset > max {
			a.offset = max
		}
		if a.offset < 0 {
			a.offset = 0
		}
	}
	page := func(delta int) { scroll(delta * rows) }

	render()
	fmt.Println("UI up")

	for {
		select {
		case k := <-keys:
			// Keys 4..8 drive the bottom bar while the input line is empty;
			// with text in it they type digits. F/Z/X/C act as arrows when
			// not typing, as the guideline recommends.
			empty := len(a.buf) == 0
			switch {
			case k.Code == keyEnter:
				line := strings.TrimSpace(string(a.buf))
				a.buf = nil
				if line != "" {
					a.status = "sent"
					g, s := c, id
					go func() { _ = g.send(s, line) }()
				}
			case k.Code == keyBackspace:
				if len(a.buf) > 0 {
					a.buf = a.buf[:len(a.buf)-1]
				}
			case empty && k.Code >= 5 && k.Code <= 9: // KEY_4..KEY_8
				switch k.Code {
				case 5:
					scroll(1)
				case 6:
					page(1)
				case 7:
					line := strings.TrimSpace(string(a.buf))
					a.buf = nil
					if line != "" {
						a.status = "sent"
						g, s := c, id
						go func() { _ = g.send(s, line) }()
					}
				case 8:
					page(-1)
				case 9:
					scroll(-1)
				}
			case empty && (k.Code == 33 || k.Code == 45 || k.Code == 44 || k.Code == 46):
				// F up, X down, Z page up, C page down
				switch k.Code {
				case 33:
					scroll(1)
				case 45:
					scroll(-1)
				case 44:
					page(1)
				case 46:
					page(-1)
				}
			case k.Rune != 0:
				if len(a.buf) < 44 {
					a.buf = append(a.buf, k.Rune)
				}
			}
			render()
		case lines := <-screens:
			if lines == nil {
				a.status = "no session"
				render()
				continue
			}
			a.status = ""
			a.lines = lines
			scroll(0) // clamp the offset to the new length
			render()
		case <-tick.C:
			if *secs > 0 && time.Now().After(deadline) {
				fmt.Println("exiting after", *secs, "s")
				return
			}
			render()
		case <-quit:
			fmt.Println("ESC")
			return
		case <-sig:
			fmt.Println("signal")
			return
		}
	}
}
