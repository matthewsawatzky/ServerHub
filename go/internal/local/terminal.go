package local

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	sshclient "serverhub/internal/ssh"
)

type termResizeMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// ProxyLocalTerminal opens a local PTY shell and proxies it over a WebSocket.
func ProxyLocalTerminal(ws *websocket.Conn) {
	ws.SetReadLimit(sshclient.WSReadLimit)
	sshclient.WSKeepalive(ws)

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte("\r\nFailed to start terminal: "+err.Error()+"\r\n"))
		return
	}
	defer func() {
		ptmx.Close()
		cmd.Wait()
	}()

	done := make(chan struct{})

	// PTY output → WebSocket
	go func() {
		defer close(done)
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}()

	// WebSocket input → PTY (or window resize)
	go func() {
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				cmd.Process.Kill()
				return
			}
			ws.SetReadDeadline(time.Now().Add(60 * time.Second)) // keepalive deadline refresh
			if msgType == websocket.TextMessage {
				var rm termResizeMsg
				if json.Unmarshal(data, &rm) == nil && rm.Type == "resize" {
					pty.Setsize(ptmx, &pty.Winsize{
						Rows: rm.Rows,
						Cols: rm.Cols,
					})
					continue
				}
			}
			io.Copy(ptmx, newLocalByteReader(data))
		}
	}()

	<-done
}

type localByteReader struct {
	data []byte
	pos  int
}

func newLocalByteReader(data []byte) *localByteReader { return &localByteReader{data: data} }

func (r *localByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
