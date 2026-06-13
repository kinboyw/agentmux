package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
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

const (
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

var ErrClosed = errors.New("websocket closed")

type HandshakeError struct {
	StatusCode int
	Status     string
}

func (e HandshakeError) Error() string {
	return "websocket upgrade failed: " + e.Status
}

type Conn struct {
	conn        net.Conn
	br          *bufio.Reader
	writeMask   bool
	readTimeout time.Duration
	writeMutex  sync.Mutex
}

func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("missing websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing websocket key")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	accept := acceptKey(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(response); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, br: rw.Reader}, nil
}

func Dial(ctx context.Context, rawURL string, token string) (*Conn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported websocket scheme %q", parsed.Scheme)
	}
	host := dialHost(parsed)
	dialer := net.Dialer{}
	var conn net.Conn
	conn, err = dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "wss" {
		serverName := parsed.Hostname()
		tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n"
	if token != "" {
		req += "Authorization: Bearer " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, HandshakeError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	return &Conn{conn: conn, br: br, writeMask: true}, nil
}

func dialHost(parsed *url.URL) string {
	if parsed.Port() != "" {
		return parsed.Host
	}
	if parsed.Scheme == "wss" {
		return net.JoinHostPort(parsed.Hostname(), "443")
	}
	return net.JoinHostPort(parsed.Hostname(), "80")
}

func (c *Conn) ReadText() (string, error) {
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return "", err
		}
		switch opcode {
		case opText:
			return string(payload), nil
		case opPing:
			_ = c.writeFrame(opPong, payload)
		case opClose:
			return "", ErrClosed
		}
	}
}

func (c *Conn) WriteText(text string) error {
	return c.writeFrame(opText, []byte(text))
}

func (c *Conn) WritePing(payload []byte) error {
	return c.writeFrame(opPing, payload)
}

func (c *Conn) SetReadDeadline(deadline time.Time) error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.SetReadDeadline(deadline)
}

func (c *Conn) SetReadTimeout(timeout time.Duration) error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.readTimeout = timeout
	if timeout <= 0 {
		return c.conn.SetReadDeadline(time.Time{})
	}
	return c.conn.SetReadDeadline(time.Now().Add(timeout))
}

func (c *Conn) Close() error {
	_ = c.writeFrame(opClose, nil)
	return c.conn.Close()
}

func (c *Conn) readFrame() (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.br, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	if length == 126 {
		var buf [2]byte
		if _, err := io.ReadFull(c.br, buf[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))
	} else if length == 127 {
		var buf [8]byte
		if _, err := io.ReadFull(c.br, buf[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(buf[:])
	}
	if length > 16*1024*1024 {
		return 0, nil, fmt.Errorf("frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	if c.readTimeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.readTimeout))
	}
	return opcode, payload, nil
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()

	var header []byte
	first := byte(0x80) | opcode
	maskBit := byte(0)
	if c.writeMask {
		maskBit = 0x80
	}
	length := len(payload)
	switch {
	case length < 126:
		header = []byte{first, maskBit | byte(length)}
	case length <= 65535:
		header = []byte{first, maskBit | 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:], uint16(length))
	default:
		header = []byte{first, maskBit | 127, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(header[2:], uint64(length))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if !c.writeMask {
		_, err := c.conn.Write(payload)
		return err
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	if _, err := c.conn.Write(mask[:]); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := c.conn.Write(masked)
	return err
}

func acceptKey(key string) string {
	const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	sum := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(sum[:])
}
