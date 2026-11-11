package main

// Streaming voice input: the microphone (or a wav file) is pushed to a
// WebSocket as 16 kHz mono PCM16 while TAB is held, and partial transcripts
// arrive as they are decoded.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/coder/websocket"
)

// wsFrame is one message from the server.
type wsFrame struct {
	Partial string  `json:"partial"`
	Final   string  `json:"final"`
	Error   string  `json:"error"`
	T       float64 `json:"t"`
}

// paced reads a wav reader at real time, so a file behaves like a microphone.
type paced struct {
	r     io.Reader
	chunk int
	// bytes per second for 16 kHz mono PCM16
	bps int
}

func (p *paced) Read(b []byte) (int, error) {
	if len(b) > p.chunk {
		b = b[:p.chunk]
	}
	n, err := p.r.Read(b)
	if n > 0 {
		time.Sleep(time.Duration(float64(n) / float64(p.bps) * float64(time.Second)))
	}
	return n, err
}

// streamer runs one push-to-talk utterance over WebSocket.
type streamer struct {
	conn      *websocket.Conn
	cancel    context.CancelFunc
	partials  chan string
	finals    chan string
	done      chan struct{}
	cmd       *exec.Cmd
	closeOnce bool
}

// startStream dials the server and starts pumping audio into it. With wav != ""
// the file (16 kHz mono PCM16 WAV) is paced out instead of the microphone.
func startStream(ctx context.Context, url, dev, wav string) (*streamer, error) {
	dctx, cancel := context.WithCancel(ctx)
	conn, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &streamer{
		conn:     conn,
		cancel:   cancel,
		partials: make(chan string, 8),
		finals:   make(chan string, 1),
		done:     make(chan struct{}),
	}

	var audio io.Reader
	if wav != "" {
		f, err := os.Open(wav)
		if err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			cancel()
			return nil, err
		}
		// skip the 44 byte RIFF header: the server wants raw PCM
		if _, err := f.Seek(44, io.SeekStart); err != nil {
			f.Close()
			conn.Close(websocket.StatusNormalClosure, "")
			cancel()
			return nil, err
		}
		audio = &paced{r: f, chunk: 1024, bps: 32000}
	} else {
		cmd := exec.CommandContext(dctx, "arecord", "-q", "-D", dev,
			"-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "raw")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			cancel()
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			cancel()
			return nil, err
		}
		s.cmd = cmd
		audio = stdout
	}

	go func() { // audio pump
		buf := make([]byte, 1024)
		for {
			n, err := audio.Read(buf)
			if n > 0 {
				ctxSend, c := context.WithTimeout(dctx, 5*time.Second)
				werr := conn.Write(ctxSend, websocket.MessageBinary, buf[:n])
				c()
				if werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}()

	go func() { // transcripts
		defer close(s.done)
		for {
			_, data, err := conn.Read(dctx)
			if err != nil {
				return
			}
			var f wsFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch {
			case f.Error != "":
				select {
				case s.partials <- "stt: " + f.Error:
				default:
				}
			case f.Final != "":
				s.finals <- f.Final
				return
			case f.Partial != "":
				select {
				case s.partials <- f.Partial:
				default:
				}
			}
		}
	}()
	return s, nil
}

// stop ends the utterance and returns the final transcript.
func (s *streamer) stop(timeout time.Duration) (string, error) {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		_ = s.cmd.Wait()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))
	select {
	case txt := <-s.finals:
		s.conn.Close(websocket.StatusNormalClosure, "")
		s.cancel()
		return txt, nil
	case <-time.After(timeout):
		s.conn.Close(websocket.StatusNormalClosure, "")
		s.cancel()
		return "", errors.New("no final transcript")
	}
}

// abort drops the connection without waiting.
func (s *streamer) abort() {
	s.cancel()
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

var _ = binary.LittleEndian
var _ = fmt.Sprintf
