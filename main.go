package main

// czgorex: a GoRex client for the CardputerZero panel.
//
// It draws straight into /dev/fb0 (no compositor, no GPU), reads the matrix
// keyboard from evdev, and talks to GoRex through the gorex CLI — the local
// session server by default, or a saved remote host picked from the overlay.
//
// Text uses the fonts the CardputerZero image ships (DejaVu Sans / Sans Mono,
// Noto Sans CJK for Chinese, Japanese and Korean) at 12px, as the app
// guideline asks, with the 7x13 bitmap face kept as a fallback.
//
// The bottom bar follows CardputerZero's key guideline: five 22x22 hints
// centred on x = 49, 105, 161, 217, 273 at y = 153, one per key 4..8, each
// with a short tick pointing at its key.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/sys/unix"
)

const (
	w = 320
	h = 170

	iconY    = 153
	iconSize = 22
	inputY   = 136 // input baseline, above the key bar
)

// Hint centres from the official guideline: keys 4, 5, 6, 7, 8.
var hintX = [5]int{49, 105, 161, 217, 273}

var (
	colBar    = color.RGBA{26, 40, 66, 255}
	colInBar  = color.RGBA{20, 30, 48, 255}
	colText   = color.RGBA{232, 238, 250, 255}
	colDim    = color.RGBA{140, 160, 190, 255}
	colAccent = color.RGBA{120, 220, 255, 255}
	colSel    = color.RGBA{36, 62, 96, 255}
)

// faces holds the fonts and the metrics the layout is derived from.
type faces struct {
	mono font.Face
	ui   font.Face
	cjk  font.Face

	cjkPath  string
	cjkSize  float64
	cjkTried bool

	cellW   int
	lineH   int // one text line: ascent + descent
	pitch   int // grid step for the terminal and the lists
	barTop  int
	textTop int
	rows    int
}

func loadFace(path string, index int, size float64) (font.Face, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f *sfnt.Font
	if index >= 0 {
		coll, err := sfnt.ParseCollection(b)
		if err != nil {
			return nil, err
		}
		if f, err = coll.Font(index); err != nil {
			return nil, err
		}
	} else if f, err = sfnt.Parse(b); err != nil {
		return nil, err
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}

// loadFaces opens the system fonts; mono and ui are required, CJK is loaded
// the first time a wide rune is drawn.
func loadFaces(monoPath, uiPath, cjkPath string, size float64) (*faces, error) {
	mono, err := loadFace(monoPath, -1, size)
	if err != nil {
		return nil, fmt.Errorf("mono %s: %w", monoPath, err)
	}
	ui, err := loadFace(uiPath, -1, size)
	if err != nil {
		return nil, fmt.Errorf("ui %s: %w", uiPath, err)
	}
	f := &faces{mono: mono, ui: ui, cjkPath: cjkPath, cjkSize: size}

	m := mono.Metrics()
	f.lineH = int((m.Ascent + m.Descent + 32) >> 6)
	if f.lineH < 12 {
		f.lineH = 12
	}
	adv, _ := mono.GlyphAdvance('M')
	f.cellW = int((adv + 32) >> 6)
	if f.cellW < 6 {
		f.cellW = 6
	}
	// The terminal grid is tighter than the font's line box: on a 170px
	// panel the line spacing is what decides how many rows fit.
	switch {
	case f.lineH >= 16: // JetBrains Mono at 12px
		f.pitch = f.lineH - 4
	case f.lineH >= 14: // DejaVu Sans Mono at 12px
		f.pitch = f.lineH - 2
	default:
		f.pitch = f.lineH
	}
	f.barTop = 15
	f.textTop = f.barTop + f.pitch
	f.rows = (inputY - 12 - f.textTop) / f.pitch
	if f.rows < 3 {
		f.rows = 3
	}
	return f, nil
}

// wide reports a rune the mono face rarely has: CJK, kana, hangul, fullwidth.
func wide(r rune) bool { return r >= 0x2E80 }

// cjkFace loads Noto Sans CJK the first time it is needed, choosing the SC
// face from the collection when the file is a .ttc.
func (f *faces) cjkFace() font.Face {
	if f.cjk != nil || f.cjkTried {
		return f.cjk
	}
	f.cjkTried = true
	b, err := os.ReadFile(f.cjkPath)
	if err != nil {
		return nil
	}
	coll, err := sfnt.ParseCollection(b)
	if err != nil {
		return nil
	}
	best := 0
	var buf sfnt.Buffer
	for i := 0; i < coll.NumFonts() && i < 16; i++ {
		fnt, err := coll.Font(i)
		if err != nil {
			continue
		}
		name, err := fnt.Name(&buf, sfnt.NameIDFull)
		if err == nil && strings.Contains(name, "SC") {
			best = i
			break
		}
	}
	face, err := loadFace(f.cjkPath, best, f.cjkSize)
	if err != nil {
		return nil
	}
	f.cjk = face
	return f.cjk
}

// draw writes s at (x, y), switching to the CJK face for wide runes, and
// returns the x after the last glyph.
func (f *faces) draw(img *image.RGBA, c *image.Uniform, x, y int, s string, primary font.Face) int {
	if s == "" {
		return x
	}
	start := 0
	cur := primary
	flush := func(end int) {
		if end == start {
			return
		}
		d := font.Drawer{Dst: img, Src: c, Face: cur, Dot: fixed.P(x, y)}
		d.DrawString(s[start:end])
		x = d.Dot.X.Round()
		start = end
	}
	for i, r := range s {
		want := primary
		if wide(r) {
			if cj := f.cjkFace(); cj != nil {
				want = cj
			}
		}
		if want != cur {
			flush(i)
			cur = want
		}
	}
	flush(len(s))
	return x
}

func (f *faces) monoText(img *image.RGBA, c *image.Uniform, x, y int, s string) int {
	return f.draw(img, c, x, y, s, f.mono)
}

func (f *faces) uiText(img *image.RGBA, c *image.Uniform, x, y int, s string) int {
	return f.draw(img, c, x, y, s, f.ui)
}

// fit truncates s (adding "~") so it fits maxW pixels in the given face.
func (f *faces) fit(s string, primary font.Face, maxW int) string {
	if font.MeasureString(primary, s).Round() <= maxW {
		return s
	}
	runes := []rune(s)
	for n := len(runes); n > 0; n-- {
		if font.MeasureString(primary, string(runes[:n])+"~").Round() <= maxW {
			return string(runes[:n]) + "~"
		}
	}
	return ""
}

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
	Code  uint16
	Rune  rune
	Value int32 // 1 press, 0 release (TAB only)
}

func findKeyboard(needle string) string {
	b, err := os.ReadFile("/proc/bus/input/devices")
	if err != nil {
		return ""
	}
	name, handler := "", ""
	pick := func() string {
		if strings.Contains(name, needle) {
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
				keys <- keyEvent{Code: keyEsc}
			}
			continue
		case keyTab:
			// Tab is tapped for the picker and held to talk.
			keys <- keyEvent{Code: keyTab, Value: ev.Value}
			continue
		}
		if ev.Value != 1 {
			continue
		}
		r, _ := keyRune(ev.Code, shift)
		keys <- keyEvent{Code: ev.Code, Rune: r}
	}
}

// client wraps the gorex CLI, either against the local server or -S host.
type client struct {
	bin string
	env []string
	dir string
}

func (c *client) run(args ...string) (string, error) {
	cmd := exec.Command(c.bin, args...)
	cmd.Env = c.env
	out, err := cmd.Output()
	return string(out), err
}

type hostEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Network bool   `json:"network,omitempty"`
}

type sessionEntry struct {
	ID       string `json:"id"`
	Program  string `json:"program"`
	Dir      string `json:"dir"`
	Title    string `json:"title"`
	Idle     bool   `json:"idle"`
	Attached int    `json:"attached"`
}

func (c *client) hosts() ([]hostEntry, error) {
	out, err := c.run("hosts", "--json")
	if err != nil {
		return nil, err
	}
	var hosts []hostEntry
	if err := json.Unmarshal([]byte(out), &hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// addHost records a host in hosts.json. The address is the tailcat address a
// remote server prints with `gorex remote address`; the other side must
// approve this machine's key (its `gorex remote pending` / `approve KEY`).
func (c *client) addHost(name, address string) error {
	type savedHost struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Address string `json:"address"`
		Network bool   `json:"network,omitempty"`
	}
	type hostsFile struct {
		Hosts []savedHost `json:"hosts"`
	}
	path := filepath.Join(c.dir, "hosts.json")
	f := hostsFile{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("hosts.json is not valid: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var idb [4]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return err
	}
	f.Hosts = append(f.Hosts, savedHost{ID: hex.EncodeToString(idb[:]), Name: name, Address: address})
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// sessionArgs puts -S before the command when a remote host is selected.
func sessionArgs(host string, args ...string) []string {
	if host == "" {
		return args
	}
	return append([]string{"-S", host}, args...)
}

func (c *client) sessions(host string) ([]sessionEntry, error) {
	out, err := c.run(sessionArgs(host, "ls", "--json")...)
	if err != nil {
		return nil, err
	}
	var list []sessionEntry
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (c *client) capture(host, id string) (string, error) {
	return c.run(sessionArgs(host, "capture", "-s", id, "--history", "65536")...)
}

func (c *client) send(host, id, text string) error {
	_, err := c.run(sessionArgs(host, "send", "-s", id, "--enter", text)...)
	return err
}

// recorder captures push-to-talk audio with arecord: 16 kHz mono WAV.
type recorder struct {
	cmd     *exec.Cmd
	path    string
	started time.Time
	stderr  *bytes.Buffer
}

// startRecorder records 16 kHz mono from dev ("plughw:1,0" is the ES8389;
// ALSA's "default" has no capture slave on this image).
func startRecorder(dir, dev string) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "voice-"+time.Now().Format("20060102-150405")+".wav")
	cmd := exec.Command("arecord", "-q", "-D", dev, "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "wav", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &recorder{cmd: cmd, path: path, started: time.Now(), stderr: &stderr}, nil
}

// copyRecorder stands in for the microphone with a wav file, for tests that
// must not depend on the room.
func copyRecorder(dir, src string) (*recorder, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "voice-"+time.Now().Format("20060102-150405")+".wav")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return &recorder{path: path, started: time.Now()}, nil
}

// stop ends the recording and reports why it holds no audio.
func (r *recorder) stop() error {
	if r.cmd == nil {
		return nil // a copied file: nothing to stop
	}
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(os.Interrupt) // arecord finalises the wav header
		_ = r.cmd.Wait()
	}
	if st, err := os.Stat(r.path); err != nil || st.Size() <= 44 {
		msg := strings.TrimSpace(r.stderr.String())
		if msg == "" {
			msg = "no audio recorded"
		}
		return errors.New(msg)
	}
	return nil
}

// transcribe runs the speech-to-text command with the wav path at $1 and in
// $VOICE_WAV, so a pipeline can use either (whisper "$VOICE_WAV",
// wc -c < "$1", ...); the text comes on stdout.
func transcribe(cmdStr, path string) (string, error) {
	cmd := exec.Command("sh", "-c", cmdStr, "czgorex", path)
	cmd.Env = append(os.Environ(), "VOICE_WAV="+path)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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
	mu      sync.Mutex
	host    string // "" = this machine's server
	session string
	buf     []rune
	lines   []string
	offset  int
	status  string

	overlay bool
	hosts   []hostEntry
	sels    []sessionEntry
	selHost int
	selSes  int
	focus   int // 0 hosts, 1 sessions
	adding  bool
	addBuf  []rune

	rec       *recorder
	holdTimer *time.Timer

	sttLocal string
	sttCloud string
	sttPick  int // 0 local, 1 cloud
	sttNames [2]string

	f  *faces
	bg *image.RGBA
}

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
	if a.overlay {
		a.drawOverlay(img)
	} else {
		a.drawTerm(img)
	}
	return img
}

func (a *app) drawTerm(img *image.RGBA) {
	f := a.f
	white := image.NewUniform(colText)
	dim := image.NewUniform(colDim)
	accent := image.NewUniform(colAccent)

	draw.Draw(img, image.Rect(0, 0, w, f.barTop), image.NewUniform(colBar), image.Point{}, draw.Src)
	short := a.session
	if len(short) > 8 {
		short = short[:8]
	}
	where := "GoRex " + short
	if a.host != "" {
		where = a.host + " " + short
	}
	if a.status != "" {
		where += " " + a.status
	}
	right := fmt.Sprintf("%d/%d %s", a.offset, len(a.lines), time.Now().Format("15:04"))
	rw := font.MeasureString(f.ui, right).Round()
	f.uiText(img, dim, w-rw-4, f.barTop-3, right)
	if a.sttLocal != "" || a.sttCloud != "" {
		tag := "L"
		if a.sttPick == 1 || a.sttLocal == "" {
			tag = "C"
		}
		f.uiText(img, dim, w-rw-20, f.barTop-3, tag)
	}
	if a.rec != nil {
		rect(img, w-rw-30, 4, w-rw-25, 9, color.RGBA{240, 80, 80, 255})
	}
	f.uiText(img, white, 3, f.barTop-3, f.fit(where, f.ui, w-rw-12))

	end := len(a.lines) - a.offset
	if end > len(a.lines) {
		end = len(a.lines)
	}
	start := end - f.rows
	if start < 0 {
		start = 0
	}
	for i, line := range a.lines[start:end] {
		f.monoText(img, white, 4, f.textTop+i*f.pitch, f.fit(line, f.mono, w-8))
	}

	inset := inputY - f.pitch
	draw.Draw(img, image.Rect(0, inset, w, inputY+3), image.NewUniform(colInBar), image.Point{}, draw.Src)
	f.monoText(img, accent, 3, inputY-3, f.fit("> "+string(a.buf), f.mono, w-8))

	kinds := [5]int{hintUp, hintPageUp, hintEnter, hintPageDown, hintDown}
	for i, cx := range hintX {
		drawHint(img, cx, iconY, kinds[i], accent)
		rect(img, cx-1, iconY+iconSize/2+1, cx+2, h-1, accent.C)
	}
}

// drawOverlay is the host/session picker.
func (a *app) drawOverlay(img *image.RGBA) {
	f := a.f
	white := image.NewUniform(colText)
	dim := image.NewUniform(colDim)
	accent := image.NewUniform(colAccent)

	draw.Draw(img, image.Rect(0, 0, w, f.barTop), image.NewUniform(colBar), image.Point{}, draw.Src)
	f.uiText(img, white, 3, f.barTop-3, "Hosts and sessions")
	closeW := font.MeasureString(f.ui, "TAB:close").Round()
	f.uiText(img, dim, w-closeW-4, f.barTop-3, "TAB:close")

	rect(img, 0, f.barTop, 110, h, color.RGBA{14, 22, 38, 255})
	f.uiText(img, accent, 3, f.textTop, "this machine")
	for i, host := range a.hosts {
		y := f.textTop + (i+1)*f.pitch
		if y > h-24 {
			break
		}
		label := host.Name
		if host.Network {
			label += " (net)"
		}
		if a.focus == 0 && i+1 == a.selHost {
			rect(img, 0, y-f.pitch+2, 110, y+3, colSel)
		}
		f.uiText(img, white, 3, y, f.fit(label, f.ui, 104))
	}

	if a.focus == 1 && len(a.sels) == 0 && a.status != "" {
		f.uiText(img, dim, 116, f.textTop, f.fit(a.status, f.ui, w-120))
	}
	for i, s := range a.sels {
		y := f.textTop + i*f.pitch
		if y > h-24 {
			break
		}
		if a.focus == 1 && i == a.selSes {
			rect(img, 112, y-f.pitch+2, w, y+3, colSel)
		}
		label := s.Program + " " + s.ID[:min(6, len(s.ID))]
		if s.Attached > 0 {
			label += "*"
		}
		x := f.uiText(img, white, 116, y, label)
		f.uiText(img, dim, x+6, y, f.fit(s.Dir, f.ui, w-x-10))
	}

	if a.adding {
		rect(img, 0, h-22, w, h, colInBar)
		f.monoText(img, accent, 3, h-7, f.fit("add: "+string(a.addBuf), f.mono, w-8))
		return
	}
	f.uiText(img, dim, 3, h-6, "4/8 mv  6 ok  5 pane  7 sync  0 add")
}

func (a *app) scroll(delta int) {
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

func (a *app) page(delta int) { a.scroll(delta * a.f.rows) }

func rect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(c), image.Point{}, draw.Src)
}

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

const (
	hintUp = iota
	hintDown
	hintPageUp
	hintPageDown
	hintEnter
)

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
		for i := range 4 {
			rect(img, cx-9+i, cy+3-i, cx-8+i, cy+4+i, col)
		}
	}
}

// firstExisting returns the first path that exists, or "".
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func bitmapFaces() *faces {
	f := &faces{mono: basicfont.Face7x13, ui: basicfont.Face7x13}
	m := f.mono.Metrics()
	f.lineH = int((m.Ascent + m.Descent + 32) >> 6)
	f.cellW = 7
	// The terminal grid is tighter than the font's line box: on a 170px
	// panel the line spacing is what decides how many rows fit.
	switch {
	case f.lineH >= 16: // JetBrains Mono at 12px
		f.pitch = f.lineH - 4
	case f.lineH >= 14: // DejaVu Sans Mono at 12px
		f.pitch = f.lineH - 2
	default:
		f.pitch = f.lineH
	}
	f.barTop = 15
	f.textTop = f.barTop + f.pitch
	f.rows = (inputY - 12 - f.textTop) / f.pitch
	return f
}

func main() {
	fbPath := flag.String("fb", "/dev/fb0", "framebuffer device")
	keyPath := flag.String("key", "", "evdev device; empty finds the tca8418 keyboard")
	shot := flag.String("shot", "", "write a PNG of each frame here")
	bin := flag.String("gorex", "", "gorex executable; empty uses ~/.local/bin/gorex")
	session := flag.String("session", "", "session id; empty uses the first one or creates it")
	secs := flag.Int("secs", 0, "exit after this many seconds (0 = run until ESC)")
	bench := flag.Int("bench", 0, "render N frames, print the average, then exit")
	fontSize := flag.Float64("fontsize", 12, "text size in pixels")
	monoPath := flag.String("mono", "", "terminal font; empty prefers JetBrains Mono, then DejaVu Sans Mono")
	uiPath := flag.String("ui-font", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "UI font")
	cjkPath := flag.String("cjk-font", "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", "CJK font")
	useBitmap := flag.Bool("bitmap", false, "use the 7x13 bitmap face instead of the system fonts")
	startOverlay := flag.Bool("overlay", false, "open the host picker at startup")
	fontCheck := flag.Bool("fontcheck", false, "report the fonts and the layout, then exit")
	stt := flag.String("stt", "", `speech-to-text command; the wav path arrives as "$1", the text comes on stdout`)
	sttLocal := flag.String("stt-local", "", "on-machine speech-to-text command ($1 / $VOICE_WAV)")
	sttCloud := flag.String("stt-cloud", "", "network speech-to-text command ($1 / $VOICE_WAV)")
	sttDefault := flag.String("stt-default", "local", "engine to use first: local or cloud")
	voiceSend := flag.Bool("voicesend", false, "send recognised speech to the session instead of the input line")
	voiceDir := flag.String("voice-dir", "", "where recordings go (default ~/.local/share/czgorex)")
	voiceTest := flag.Float64("voicetest", 0, "record N seconds, transcribe, print and exit")
	mic := flag.String("mic", "plughw:1,0", "ALSA capture device (ES8389)")
	fakeWav := flag.String("fakewav", "", "use this wav instead of the microphone (testing)")
	flag.Parse()

	if *stt != "" && *sttLocal == "" {
		*sttLocal = *stt
	}

	recordings := *voiceDir
	if recordings == "" {
		recordings = filepath.Join(os.Getenv("HOME"), ".local", "share", "czgorex")
	}

	startCapture := func(dir string) (*recorder, error) {
		if *fakeWav != "" {
			return copyRecorder(dir, *fakeWav)
		}
		return startRecorder(dir, *mic)
	}

	if *voiceTest > 0 {
		rec, err := startCapture(recordings)
		if err != nil {
			fmt.Println("record:", err)
			os.Exit(1)
		}
		time.Sleep(time.Duration(*voiceTest * float64(time.Second)))
		if err := rec.stop(); err != nil {
			fmt.Println("record:", err)
			os.Exit(1)
		}
		fmt.Printf("wav: %s (%.1fs)\n", rec.path, time.Since(rec.started).Seconds())
		engine := *stt
		if engine == "" {
			engine = *sttLocal
			if strings.EqualFold(*sttDefault, "cloud") {
				engine = *sttCloud
			}
		}
		if engine == "" {
			fmt.Println("no stt command: recorded only")
			return
		}
		txt, err := transcribe(engine, rec.path)
		if err != nil {
			fmt.Println("stt:", err)
		}
		fmt.Println("text:", txt)
		return
	}

	if *fontCheck {
		mono := *monoPath
		if mono == "" {
			mono = firstExisting(
				"/usr/share/fonts/truetype/jetbrains-mono/JetBrainsMono-Regular.ttf",
				filepath.Join(os.Getenv("HOME"), ".local/share/fonts/JetBrainsMono-Regular.ttf"),
				"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
			)
		}
		f, err := loadFaces(mono, *uiPath, *cjkPath, *fontSize)
		if err != nil {
			fmt.Println("fonts:", err)
			os.Exit(1)
		}
		fmt.Printf("mono=%s ui=%s cjk=%s size=%.0f\n", mono, *uiPath, *cjkPath, *fontSize)
		fmt.Printf("line=%d pitch=%d cell=%d rows=%d\n", f.lineH, f.pitch, f.cellW, f.rows)
		if f.cjkFace() != nil {
			fmt.Println("cjk: loaded")
		} else {
			fmt.Println("cjk: unavailable")
		}
		return
	}

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
	if key != "" && !strings.HasPrefix(key, "/") {
		// a device name rather than a path, useful with a virtual keyboard
		if key = findKeyboard(key); key == "" {
			fmt.Println("no input device named", *keyPath)
			os.Exit(1)
		}
	}
	if key == "" {
		if key = findKeyboard("tca8418"); key == "" {
			fmt.Println("no keyboard device found; pass -key")
			os.Exit(1)
		}
	}

	mono := *monoPath
	if mono == "" {
		mono = firstExisting(
			"/usr/share/fonts/truetype/jetbrains-mono/JetBrainsMono-Regular.ttf",
			filepath.Join(os.Getenv("HOME"), ".local/share/fonts/JetBrainsMono-Regular.ttf"),
			"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
		)
	}
	fc := bitmapFaces()
	if !*useBitmap && mono != "" {
		if loaded, err := loadFaces(mono, *uiPath, *cjkPath, *fontSize); err != nil {
			fmt.Println("fonts:", err, "- using the bitmap face")
		} else {
			fc = loaded
			fmt.Printf("font: %s %.0fpx cell=%d line=%d pitch=%d rows=%d\n",
				filepath.Base(mono), *fontSize, fc.cellW, fc.lineH, fc.pitch, fc.rows)
		}
	}

	exe := *bin
	if exe == "" {
		exe = filepath.Join(os.Getenv("HOME"), ".local", "bin", "gorex")
	}
	dir := filepath.Join(os.Getenv("HOME"), ".gorex")
	c := &client{bin: exe, env: append(os.Environ(), "GOREX_DIR="+dir), dir: dir}

	a := &app{status: "connecting", f: fc, sttLocal: *sttLocal, sttCloud: *sttCloud}
	a.sttNames = [2]string{"local", "cloud"}
	if strings.EqualFold(*sttDefault, "cloud") && a.sttCloud != "" {
		a.sttPick = 1
	}
	if a.sttLocal == "" && a.sttCloud != "" {
		a.sttPick = 1
	}
	if *session != "" {
		a.session = *session
	} else if list, err := c.sessions(""); err == nil && len(list) > 0 {
		a.session = list[0].ID
	} else if out, err := c.run("new", "--dir", os.Getenv("HOME")); err == nil {
		a.session = strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0])
	} else {
		a.status = "no session"
	}
	fmt.Println("session:", a.session)

	keys := make(chan keyEvent, 64)
	quit := make(chan struct{})
	voiceStart := make(chan string, 1)
	voiceStop := make(chan struct{}, 1)
	voiceDone := make(chan string, 1)
	go readKeys(key, keys, quit)

	screens := make(chan []string, 4)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
			}
			a.mu.Lock()
			host, id := a.host, a.session
			a.mu.Unlock()
			if id == "" {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			out, err := c.capture(host, id)
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

	if *bench > 0 {
		start := time.Now()
		for range *bench {
			fb.blit(a.draw())
		}
		avg := time.Since(start) / time.Duration(*bench)
		fmt.Printf("bench: %d frames, %v per frame (%.0f fps)\n", *bench, avg, float64(time.Second)/float64(avg))
		return
	}

	deadline := time.Now().Add(time.Duration(*secs) * time.Second)
	tick := time.NewTicker(time.Second)
	voiceTick := time.NewTicker(200 * time.Millisecond)
	defer voiceTick.Stop()
	render := func() {
		img := a.draw()
		fb.blit(img)
		if *shot != "" {
			if fh, err := os.Create(*shot); err == nil {
				png.Encode(fh, img)
				fh.Close()
			}
			os.WriteFile(*shot+".fb", fb.buf, 0o644)
		}
	}

	loadSessions := func() {
		host := ""
		if a.selHost > 0 && a.selHost-1 < len(a.hosts) {
			host = a.hosts[a.selHost-1].Name
		}
		list, err := c.sessions(host)
		if err != nil {
			a.sels = nil
			a.status = "host unreachable"
			return
		}
		a.sels = list
		a.selSes = 0
		a.status = ""
	}
	openOverlay := func() {
		hosts, err := c.hosts()
		if err != nil {
			hosts = nil
		}
		a.hosts = hosts
		a.overlay = true
		a.adding = false
		a.addBuf = nil
		a.focus = 0
		a.selHost = 0
		loadSessions()
	}

	toggleOverlay := func() {
		if a.overlay {
			a.overlay = false
		} else {
			openOverlay()
		}
	}

	render()
	if *startOverlay {
		openOverlay()
		render()
	}
	fmt.Println("UI up")

	for {
		select {
		case k := <-keys:
			if k.Code == keyEsc {
				if a.adding {
					a.adding = false
					a.addBuf = nil
					render()
					continue
				}
				if a.overlay {
					a.overlay = false
					render()
					continue
				}
				fmt.Println("ESC")
				return
			}
			if k.Code == keyTab {
				if k.Value == 1 {
					// A tap opens the picker; a hold is push-to-talk.
					a.holdTimer = time.AfterFunc(350*time.Millisecond, func() {
						select {
						case voiceStart <- recordings:
						default:
						}
					})
				} else {
					if a.holdTimer != nil {
						a.holdTimer.Stop()
						a.holdTimer = nil
					}
					select {
					case voiceStop <- struct{}{}:
					default:
					}
				}
				continue
			}
			if a.adding {
				switch k.Code {
				case keyEnter:
					fields := strings.Fields(string(a.addBuf))
					a.adding = false
					a.addBuf = nil
					if len(fields) >= 2 {
						name := fields[0]
						addr := fields[len(fields)-1]
						if err := c.addHost(name, addr); err != nil {
							a.status = "add failed"
						} else {
							a.status = "host added"
							hosts, _ := c.hosts()
							a.hosts = hosts
						}
					}
				case keyBackspace:
					if len(a.addBuf) > 0 {
						a.addBuf = a.addBuf[:len(a.addBuf)-1]
					}
				default:
					if k.Rune != 0 && len(a.addBuf) < 120 {
						a.addBuf = append(a.addBuf, k.Rune)
					}
				}
				render()
				continue
			}
			if a.overlay {
				switch {
				case k.Code == keyEnter:
					if a.focus == 0 {
						loadSessions()
						a.focus = 1
					} else if a.selSes < len(a.sels) {
						a.mu.Lock()
						a.host = ""
						if a.selHost > 0 {
							a.host = a.hosts[a.selHost-1].Name
						}
						a.session = a.sels[a.selSes].ID
						a.lines, a.offset, a.status = nil, 0, ""
						a.mu.Unlock()
						a.overlay = false
					}
				case k.Code == 5: // KEY_4 up
					if a.focus == 0 {
						if a.selHost > 0 {
							a.selHost--
						}
					} else if a.selSes > 0 {
						a.selSes--
					}
				case k.Code == 9: // KEY_8 down
					if a.focus == 0 {
						if a.selHost < len(a.hosts) {
							a.selHost++
						}
					} else if a.selSes < len(a.sels)-1 {
						a.selSes++
					}
				case k.Code == 6: // KEY_5
					a.focus = 0
					loadSessions()
				case k.Code == 8: // KEY_7 refresh
					loadSessions()
				case k.Code == 11: // KEY_0 add host
					a.adding = true
					a.addBuf = nil
				}
				render()
				continue
			}

			empty := len(a.buf) == 0
			switch {
			case k.Code == keyEnter:
				line := strings.TrimSpace(string(a.buf))
				a.buf = nil
				if line != "" {
					a.status = "sent"
					a.mu.Lock()
					host, id := a.host, a.session
					a.mu.Unlock()
					go func() { _ = c.send(host, id, line) }()
				}
			case k.Code == keyBackspace:
				if len(a.buf) > 0 {
					a.buf = a.buf[:len(a.buf)-1]
				}
			case empty && k.Code >= 5 && k.Code <= 9: // KEY_4..KEY_8
				switch k.Code {
				case 5:
					a.scroll(1)
				case 6:
					a.page(1)
				case 7:
					line := strings.TrimSpace(string(a.buf))
					a.buf = nil
					if line != "" {
						a.status = "sent"
						a.mu.Lock()
						host, id := a.host, a.session
						a.mu.Unlock()
						go func() { _ = c.send(host, id, line) }()
					}
				case 8:
					a.page(-1)
				case 9:
					a.scroll(-1)
				}
			case empty && k.Code == 10: // KEY_9: cycle local/cloud
				switch {
				case a.sttLocal == "" && a.sttCloud == "":
					a.status = "no stt engine"
				case a.sttCloud == "":
					a.sttPick = 0
					a.status = "stt: local"
				case a.sttLocal == "":
					a.sttPick = 1
					a.status = "stt: cloud"
				default:
					a.sttPick = 1 - a.sttPick
					a.status = "stt: " + a.sttNames[a.sttPick]
				}
			case empty && (k.Code == 33 || k.Code == 45 || k.Code == 44 || k.Code == 46):
				// F up, X down, Z page up, C page down
				switch k.Code {
				case 33:
					a.scroll(1)
				case 45:
					a.scroll(-1)
				case 44:
					a.page(1)
				case 46:
					a.page(-1)
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
			a.lines = lines
			a.status = ""
			a.scroll(0)
			render()
		case dir := <-voiceStart:
			if a.rec == nil {
				r, err := startCapture(dir)
				if err != nil {
					a.status = "mic: " + err.Error()
				} else {
					a.rec = r
					a.status = "REC"
				}
				render()
			}
		case <-voiceStop:
			if a.rec == nil {
				a.status = ""
				toggleOverlay()
				render()
				continue
			}
			r := a.rec
			a.rec = nil
			recErr := r.stop()
			if recErr != nil {
				a.status = "mic: " + recErr.Error()
				render()
				continue
			}
			if time.Since(r.started) < 300*time.Millisecond {
				os.Remove(r.path) // too short to be speech: a tap after all
				a.status = ""
				toggleOverlay()
				render()
				continue
			}
			if a.sttLocal == "" && a.sttCloud == "" {
				a.status = "voice saved"
				render()
				continue
			}
			a.status = "listening…"
			render()
			go func(path string) {
				a.mu.Lock()
				cmds := [2]string{a.sttLocal, a.sttCloud}
				pick := a.sttPick
				a.mu.Unlock()
				txt, first := "", cmds[pick]
				if first != "" {
					txt, _ = transcribe(first, path)
				}
				if txt == "" {
					other := cmds[1-pick]
					if other != "" {
						txt, _ = transcribe(other, path)
					}
				}
				voiceDone <- txt
			}(r.path)
		case txt := <-voiceDone:
			if txt == "" {
				a.status = "stt: no text"
				render()
				continue
			}
			a.status = ""
			if *voiceSend {
				a.mu.Lock()
				host, id := a.host, a.session
				a.mu.Unlock()
				go func() { _ = c.send(host, id, txt) }()
			} else {
				if len(a.buf) > 0 {
					a.buf = append(a.buf, ' ')
				}
				a.buf = append(a.buf, []rune(txt)...)
			}
			render()
		case <-voiceTick.C:
			if a.rec != nil {
				a.status = fmt.Sprintf("REC %.1fs", time.Since(a.rec.started).Seconds())
				render()
			}
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
