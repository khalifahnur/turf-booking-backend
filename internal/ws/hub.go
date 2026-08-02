package ws

import (
	"sync"

	"github.com/gorilla/websocket"
)

type PaymentHub struct {
	sync.RWMutex
	clients map[string]*websocket.Conn
}

func NewPaymentHub() *PaymentHub {
	return &PaymentHub{
		clients: make(map[string]*websocket.Conn),
	}
}

func (h *PaymentHub) AddClient(reference string, conn *websocket.Conn) {
	h.Lock()
	defer h.Unlock()
	h.clients[reference] = conn
}

func (h *PaymentHub) RemoveClient(reference string) {
	h.Lock()
	defer h.Unlock()
	delete(h.clients, reference)
}

func (h *PaymentHub) NotifyClient(reference string, status string) {
	h.RLock()
	conn, exists := h.clients[reference]
	h.RUnlock()

	if exists {
		conn.WriteJSON(map[string]string{
			"reference": reference,
			"status":    status,
		})

		if status == "Completed" || status == "Failed" {
			conn.Close()
		}
	}
}
