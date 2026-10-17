package ws

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// MaxClients bounds memory and file descriptors if the endpoint is flooded.
	MaxClients   = 2000
	writeTimeout = 5 * time.Second
)

// client wraps a connection with the write lock gorilla/websocket requires:
// the webhook goroutine and the keep-alive pinger can otherwise write at once.
type client struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *client) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteJSON(v)
}

// Ping sends a keep-alive frame.
func (c *client) ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
}

type PaymentHub struct {
	sync.RWMutex
	clients map[string]*client
}

func NewPaymentHub() *PaymentHub {
	return &PaymentHub{clients: make(map[string]*client)}
}

// Handle is what a live connection holds on to.
type Handle struct {
	hub       *PaymentHub
	reference string
	c         *client
}

// AddClient registers conn for reference. It reports false when the hub is
// full. A second connection for the same reference replaces the first, which
// is how a client that reconnects after a network drop takes over cleanly.
func (h *PaymentHub) AddClient(reference string, conn *websocket.Conn) (*Handle, bool) {
	h.Lock()
	defer h.Unlock()
	if _, replacing := h.clients[reference]; !replacing && len(h.clients) >= MaxClients {
		return nil, false
	}
	c := &client{conn: conn}
	if old, ok := h.clients[reference]; ok {
		old.conn.Close()
	}
	h.clients[reference] = c
	return &Handle{hub: h, reference: reference, c: c}, true
}

// Remove unregisters this connection only. If a newer connection has already
// replaced it, the newer one is left alone.
func (hd *Handle) Remove() {
	hd.hub.Lock()
	defer hd.hub.Unlock()
	if hd.hub.clients[hd.reference] == hd.c {
		delete(hd.hub.clients, hd.reference)
	}
}

// Ping keeps the connection alive through idle proxies.
func (hd *Handle) Ping() error { return hd.c.ping() }

// Send writes a status to this connection.
func (hd *Handle) Send(reference, status string) error {
	return hd.c.writeJSON(map[string]string{"reference": reference, "status": status})
}

func (h *PaymentHub) NotifyClient(reference string, status string) {
	h.RLock()
	c, exists := h.clients[reference]
	h.RUnlock()

	if !exists {
		return
	}
	c.writeJSON(map[string]string{
		"reference": reference,
		"status":    status,
	})

	if status == "Completed" || status == "Failed" {
		c.conn.Close()
	}
}
