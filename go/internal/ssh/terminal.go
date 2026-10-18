package ssh

import (
	"encoding/json"
	"io"
	"log"
	"time"

	"github.com/gorilla/websocket"
	gossh "golang.org/x/crypto/ssh"
)

// WSReadLimit caps a single WebSocket frame on terminal connections.
// 1 MiB comfortably accommodates large pastes; gorilla's default is
// unlimited, which allows a single frame to allocate arbitrary memory.
const WSReadLimit = 1 << 20

// WSKeepalive sends control pings and enforces a read deadline so idle or
// dead peers are detected. Browsers auto-pong, so xterm.js clients are
// unaffected. WriteControl is safe to call concurrently with other writes.
func WSKeepalive(ws *websocket.Conn) {
	const (
		pongWait   = 60 * time.Second
		pingPeriod = 25 * time.Second
	)
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	ws.SetReadDeadline(time.Now().Add(pongWait))
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for range t.C {
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}()
}

type resizeMsg struct {
	Type string `json:"type"`
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

// ProxyTerminal opens an SSH PTY and proxies it over a WebSocket connection.
func ProxyTerminal(ws *websocket.Conn, host string, port int, user, keyPath string) {
	ws.SetReadLimit(WSReadLimit)
	WSKeepalive(ws)
	client, err := Dial(host, port, user, keyPath)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nSSH connection failed: "+err.Error()+"\r\n"))
		return
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nFailed to open session: "+err.Error()+"\r\n"))
		return
	}
	defer sess.Close()

	modes := gossh.TerminalModes{
		gossh.ECHO:          1,
		gossh.TTY_OP_ISPEED: 14400,
		gossh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nPTY request failed: "+err.Error()+"\r\n"))
		return
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nStdin pipe failed: "+err.Error()+"\r\n"))
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nStdout pipe failed: "+err.Error()+"\r\n"))
		return
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nStderr pipe failed: "+err.Error()+"\r\n"))
		return
	}

	if err := sess.Shell(); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nShell start failed: "+err.Error()+"\r\n"))
		return
	}

	done := make(chan struct{})

	// SSH stdout → WebSocket
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		close(done)
	}()

	// SSH stderr → WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				ws.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	// WebSocket → SSH stdin (or resize)
	go func() {
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				sess.Close()
				return
			}
			ws.SetReadDeadline(time.Now().Add(60 * time.Second)) // keepalive deadline refresh
			if msgType == websocket.TextMessage {
				var resize resizeMsg
				if json.Unmarshal(data, &resize) == nil && resize.Type == "resize" {
					if err := sess.WindowChange(int(resize.Rows), int(resize.Cols)); err != nil {
						log.Printf("window change error: %v", err)
					}
					continue
				}
			}
			if _, err := io.Copy(stdin, newByteReader(data)); err != nil {
				log.Printf("stdin write error: %v", err)
			}
		}
	}()

	<-done
}

type byteReader struct {
	data []byte
	pos  int
}

func newByteReader(data []byte) *byteReader {
	return &byteReader{data: data}
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
