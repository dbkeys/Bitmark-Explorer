package sse

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

type clientInfo struct {
	ip    string
	tabID string // sessionStorage-generated per-tab ID; empty if not provided
}

// IPStat summarises connections from a single client IP.
type IPStat struct {
	IP    string
	Conns int // total open connections from this IP
	Tabs  int // distinct tab IDs from this IP
}

// Broker manages SSE client connections and broadcasts block notifications.
// It is safe for concurrent use.
type Broker struct {
	mu      sync.Mutex
	clients map[chan struct{}]clientInfo
}

func NewBroker() *Broker {
	return &Broker{clients: make(map[chan struct{}]clientInfo)}
}

// Broadcast sends a newblock event to every connected client.
// Clients that are not ready to receive are skipped (non-blocking).
func (b *Broker) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- struct{}{}:
		default: // client lagging — skip rather than block the loop
		}
	}
}

// Connected returns aggregate totals (connections, distinct tabs, distinct IPs)
// and a per-IP breakdown sorted by IP address.
func (b *Broker) Connected() (conns, tabs, ips int, byIP []IPStat) {
	b.mu.Lock()
	defer b.mu.Unlock()

	type ipAccum struct {
		tabs map[string]struct{}
		conn int
	}
	m := make(map[string]*ipAccum, len(b.clients))
	seenTabs := make(map[string]struct{}, len(b.clients))

	for _, c := range b.clients {
		a, ok := m[c.ip]
		if !ok {
			a = &ipAccum{tabs: make(map[string]struct{})}
			m[c.ip] = a
		}
		a.conn++
		if c.tabID != "" {
			a.tabs[c.tabID] = struct{}{}
			seenTabs[c.tabID] = struct{}{}
		}
	}

	conns = len(b.clients)
	tabs = len(seenTabs)
	ips = len(m)

	byIP = make([]IPStat, 0, len(m))
	for ip, a := range m {
		byIP = append(byIP, IPStat{IP: ip, Conns: a.conn, Tabs: len(a.tabs)})
	}
	sort.Slice(byIP, func(i, j int) bool { return byIP[i].IP < byIP[j].IP })
	return
}

func (b *Broker) subscribe(info clientInfo) chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.clients[ch] = info
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

// clientIP extracts the real client IP, preferring X-Forwarded-For (set by
// Apache mod_proxy) over the TCP remote address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ServeHTTP implements http.Handler for the /events SSE endpoint.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := b.subscribe(clientInfo{
		ip:    clientIP(r),
		tabID: r.URL.Query().Get("tab"),
	})
	defer b.unsubscribe(ch)

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprintf(w, "event: newblock\ndata: {}\n\n")
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
