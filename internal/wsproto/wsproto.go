package wsproto

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn is a minimal RFC6455 connection supporting text/binary JSON messages.
type Conn struct {
	conn     net.Conn
	br       *bufio.Reader
	isClient bool
	mu       sync.Mutex
}

func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !headerContainsToken(r.Header, "Connection", "Upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		return nil, errors.New("missing websocket upgrade headers")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("unsupported websocket version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("server does not support hijacking")
	}

	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack failed: %w", err)
	}

	resp := bytes.Buffer{}
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	resp.WriteString("Upgrade: websocket\r\n")
	resp.WriteString("Connection: Upgrade\r\n")
	resp.WriteString("Sec-WebSocket-Accept: " + computeAccept(key) + "\r\n")
	resp.WriteString("\r\n")
	if _, err := rw.Write(resp.Bytes()); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("failed to write upgrade response: %w", err)
	}
	if err := rw.Flush(); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("failed to flush upgrade response: %w", err)
	}

	return &Conn{conn: nc, br: rw.Reader, isClient: false}, nil
}

func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("invalid ws URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	d := net.Dialer{}
	var nc net.Conn
	if u.Scheme == "wss" {
		td := tls.Dialer{
			NetDialer: &d,
			Config:    &tls.Config{ServerName: u.Hostname()},
		}
		nc, err = td.DialContext(ctx, "tcp", host)
	} else {
		nc, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}

	key, err := randomB64(16)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	req := bytes.Buffer{}
	req.WriteString("GET " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("\r\n")
	if _, err := nc.Write(req.Bytes()); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("handshake write failed: %w", err)
	}

	br := bufio.NewReader(nc)
	httpReq := &http.Request{Method: http.MethodGet}
	resp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("handshake response failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = nc.Close()
		return nil, fmt.Errorf("unexpected status during websocket handshake: %s", resp.Status)
	}
	if !headerContainsToken(resp.Header, "Connection", "Upgrade") || !headerContainsToken(resp.Header, "Upgrade", "websocket") {
		_ = nc.Close()
		return nil, errors.New("invalid upgrade response headers")
	}
	if strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Accept")) != computeAccept(key) {
		_ = nc.Close()
		return nil, errors.New("invalid Sec-WebSocket-Accept")
	}

	return &Conn{conn: nc, br: br, isClient: true}, nil
}

func (c *Conn) Close() error {
	return c.conn.Close()
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *Conn) WriteJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(0x1, payload)
}

func (c *Conn) ReadJSON(v any) error {
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return err
		}
		switch opcode {
		case 0x1, 0x2:
			return json.Unmarshal(payload, v)
		case 0x8:
			return io.EOF
		case 0x9:
			if err := c.writeFrame(0xA, payload); err != nil {
				return err
			}
		case 0xA:
			continue
		default:
			return fmt.Errorf("unsupported websocket opcode %d", opcode)
		}
	}
}

func (c *Conn) readFrame() (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.br, header); err != nil {
		return 0, nil, err
	}

	if (header[0] & 0x80) == 0 {
		return 0, nil, errors.New("fragmented frames are not supported")
	}
	opcode := header[0] & 0x0F

	masked := (header[1] & 0x80) != 0
	payloadLen := uint64(header[1] & 0x7F)
	if payloadLen == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = uint64(binary.BigEndian.Uint16(ext))
	} else if payloadLen == 127 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = binary.BigEndian.Uint64(ext)
	}
	if payloadLen > 16*1024*1024 {
		return 0, nil, errors.New("payload too large")
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, int(payloadLen))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	return opcode, payload, nil
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	head := make([]byte, 0, 14)
	head = append(head, 0x80|(opcode&0x0F))

	payloadLen := len(payload)
	maskBit := byte(0)
	if c.isClient {
		maskBit = 0x80
	}

	switch {
	case payloadLen < 126:
		head = append(head, maskBit|byte(payloadLen))
	case payloadLen <= 65535:
		head = append(head, maskBit|126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(payloadLen))
		head = append(head, ext...)
	default:
		head = append(head, maskBit|127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(payloadLen))
		head = append(head, ext...)
	}

	if c.isClient {
		maskKey := make([]byte, 4)
		if _, err := rand.Read(maskKey); err != nil {
			return err
		}
		head = append(head, maskKey...)
		masked := make([]byte, payloadLen)
		copy(masked, payload)
		for i := range masked {
			masked[i] ^= maskKey[i%4]
		}
		if _, err := c.conn.Write(head); err != nil {
			return err
		}
		_, err := c.conn.Write(masked)
		return err
	}

	if _, err := c.conn.Write(head); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

func computeAccept(key string) string {
	h := sha1.New() //nolint:gosec // Required by RFC6455.
	_, _ = h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerContainsToken(h http.Header, name, token string) bool {
	v := h.Get(name)
	if v == "" {
		return false
	}
	token = strings.ToLower(token)
	for _, part := range strings.Split(v, ",") {
		if strings.ToLower(strings.TrimSpace(part)) == token {
			return true
		}
	}
	return false
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
