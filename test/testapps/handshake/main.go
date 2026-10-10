// Command handshake is a test helper that speaks the external-application
// wire protocol directly instead of through sdk.Serve, so tests can probe
// the runtime's handshake validation. It resolves the channel and token
// from GOSTALGIA_IPC_FD / GOSTALGIA_ENDPOINT / GOSTALGIA_APP_TOKEN like a
// real application. Modes (argv[1]):
//
//	ok N         - auth, then app/ready with N valid routes, then serve
//	routes N     - same as ok (N may exceed runtime limits)
//	badname      - app/ready with invalid route names
//	badtoken [d] - auth with a wrong token after d ms (default 200)
//	emptytoken   - auth with an empty token
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type wireMsg struct {
	ID     int64           `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func dial() (net.Conn, error) {
	if os.Getenv("GOSTALGIA_IPC_FD") != "" {
		f := os.NewFile(3, "gostalgia-ipc")
		conn, err := net.FileConn(f)
		_ = f.Close()
		return conn, err
	}
	ep := os.Getenv("GOSTALGIA_ENDPOINT")
	switch {
	case strings.HasPrefix(ep, "unix://"):
		return net.Dial("unix", strings.TrimPrefix(ep, "unix://"))
	case strings.HasPrefix(ep, "tcp://"):
		return net.Dial("tcp", strings.TrimPrefix(ep, "tcp://"))
	}
	return nil, fmt.Errorf("handshake: unsupported endpoint %q", ep)
}

func writeMsg(conn net.Conn, v any) {
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

func main() {
	mode := "ok"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	conn, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "handshake: dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	token := os.Getenv("GOSTALGIA_APP_TOKEN")
	appID := os.Getenv("GOSTALGIA_APP_ID")

	sendAuth := func(tok string) bool {
		params, _ := json.Marshal(map[string]string{"token": tok})
		writeMsg(conn, wireMsg{ID: 1, Method: "auth", Params: params})
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false
		}
		var resp wireMsg
		if json.Unmarshal(bytes.TrimSpace(line), &resp) != nil {
			return false
		}
		return resp.OK
	}

	sendReady := func(routes []string) bool {
		params, _ := json.Marshal(map[string]any{
			"protocol_version": 1,
			"app_id":           appID,
			"routes":           routes,
		})
		writeMsg(conn, wireMsg{ID: 2, Method: "app/ready", Params: params})
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false
		}
		var resp wireMsg
		if json.Unmarshal(bytes.TrimSpace(line), &resp) != nil {
			return false
		}
		if !resp.OK {
			fmt.Fprintln(os.Stderr, "handshake: ready rejected:", resp.Error)
		}
		return resp.OK
	}

	switch mode {
	case "badtoken":
		delay := 200
		if len(os.Args) > 2 {
			delay, _ = strconv.Atoi(os.Args[2])
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
		_ = sendAuth("wrong-token-deadbeef")
		time.Sleep(500 * time.Millisecond)
	case "emptytoken":
		_ = sendAuth("")
		time.Sleep(500 * time.Millisecond)
	default:
		if !sendAuth(token) {
			fmt.Fprintln(os.Stderr, "handshake: auth failed")
			os.Exit(2)
		}
		var routes []string
		switch mode {
		case "badname":
			routes = []string{"../escape", "UPPER", "with space", "", "a/b"}
		default:
			n := 0
			if len(os.Args) > 2 {
				n, _ = strconv.Atoi(os.Args[2])
			}
			for i := 0; i < n; i++ {
				routes = append(routes, fmt.Sprintf("r%d", i))
			}
		}
		if !sendReady(routes) {
			time.Sleep(500 * time.Millisecond)
			os.Exit(3)
		}
		// Serve trivially: answer every request with ok until the channel dies.
		for {
			l, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var m wireMsg
			if json.Unmarshal(bytes.TrimSpace(l), &m) != nil {
				continue
			}
			if m.Method != "" {
				writeMsg(conn, wireMsg{ID: m.ID, OK: true})
			}
		}
	}
}
