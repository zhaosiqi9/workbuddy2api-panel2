package proxypool

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

func (m *Manager) transportLocked(n node) (*http.Transport, error) {
	if tr := m.transports[n.ID]; tr != nil {
		return tr, nil
	}
	u, err := parseURL(n.URL)
	if err != nil {
		return nil, err
	}
	tr := m.base.Clone()
	tr.Proxy = nil
	if u.Scheme == "http" || u.Scheme == "https" {
		tr.Proxy = http.ProxyURL(u)
	} else {
		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: password}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, m.dialer)
		if err != nil {
			return nil, errors.New("create SOCKS dialer failed")
		}
		dialer := d.(proxy.ContextDialer)
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if m.dialer.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, m.dialer.Timeout)
				defer cancel()
			}
			if u.Scheme == "socks5" {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				if net.ParseIP(host) == nil {
					resolver := m.dialer.Resolver
					if resolver == nil {
						resolver = net.DefaultResolver
					}
					ips, err := resolver.LookupIPAddr(ctx, host)
					if err != nil {
						return nil, err
					}
					if len(ips) == 0 {
						return nil, errors.New("DNS returned no addresses")
					}
					address = net.JoinHostPort(ips[0].IP.String(), port)
				}
			}
			return dialer.DialContext(ctx, network, address)
		}
	}
	m.transports[n.ID] = tr
	return tr, nil
}

// Do selects once before Client.Do so redirects and the response body keep the route.
// The caller's Client is copied; shared clients and live transports are never mutated.
func (m *Manager) Do(client *http.Client, req *http.Request, uid, realm string) (*http.Response, error) {
	m.mu.Lock()
	n, proxied, err := m.selectLocked(uid, realm)
	var tr *http.Transport
	if err == nil && proxied {
		tr, err = m.transportLocked(n)
	}
	m.mu.Unlock()
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if !proxied {
		return client.Do(req)
	}
	selected := *client
	selected.Transport = tr
	resp, err := selected.Do(req)
	if err != nil {
		reason := "proxy request failed"
		if errors.Is(err, context.Canceled) {
			reason = "request canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = "request timed out"
		}
		tr.CloseIdleConnections()
		return nil, &Error{ProxyID: n.ID, Reason: reason, cause: err}
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		_ = resp.Body.Close()
		return nil, &Error{ProxyID: n.ID, Reason: "proxy authentication failed"}
	}
	resp.Body = &proxyBody{ReadCloser: resp.Body, id: n.ID, transport: tr}
	return resp, nil
}

type proxyBody struct {
	io.ReadCloser
	id        string
	transport *http.Transport
}

func (b *proxyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.transport.CloseIdleConnections()
		err = &Error{ProxyID: b.id, Reason: "proxy response interrupted", cause: err}
	}
	return n, err
}

// Probe uses a separate total deadline and never carries account headers or cookies.
func (m *Manager) Probe(ctx context.Context, id string) error {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("proxy pool is closed")
	}
	if m.checking[id] {
		m.mu.Unlock()
		return nil
	}
	var found *node
	for _, n := range m.data.Nodes {
		if n.ID == id {
			copy := n
			found = &copy
			break
		}
	}
	if found == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	tr, err := m.transportLocked(*found)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	version, settings := m.generation[id], m.data.Settings
	m.checking[id] = true
	delete(m.requested, id)
	h := m.health[id]
	h.attemptedAt = time.Now()
	m.health[id] = h
	m.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, settings.URL, nil)
	client := &http.Client{Transport: tr}
	resp, checkErr := client.Do(req)
	message := ""
	if checkErr != nil {
		message = "proxy connection failed"
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			message = "health check timed out"
		}
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			message = "health endpoint returned HTTP " + strconv.Itoa(resp.StatusCode)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.checking, id)
	if ctx.Err() != nil || m.closed || m.generation[id] != version {
		m.signal()
		return nil
	}
	h = m.health[id]
	h.checkedAt, h.latencyMS, h.lastError = time.Now(), time.Since(start).Milliseconds(), message
	if message == "" {
		h.failures = 0
		h.successes++
		if h.status != "unhealthy" || h.successes >= settings.SuccessThreshold {
			h.status = "healthy"
		}
	} else {
		h.successes = 0
		h.failures++
		if h.failures >= settings.FailureThreshold {
			h.status = "unhealthy"
			tr.CloseIdleConnections()
		}
	}
	m.health[id] = h
	return nil
}

// CheckAll shares the global eight-probe bound with the periodic checker.
func (m *Manager) CheckAll(ctx context.Context) {
	s := m.Snapshot()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				_ = m.Probe(ctx, id)
			}
		}()
	}
	for _, n := range s.Proxies {
		select {
		case jobs <- n.ID:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

// Run is blocking and must finish before Close. Disabled-by-health nodes remain due.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				_ = m.Probe(ctx, id)
			}
		}()
	}
	defer func() { close(jobs); wg.Wait() }()
	for {
		m.mu.Lock()
		var due []string
		for _, n := range m.data.Nodes {
			if !m.checking[n.ID] && (m.requested[n.ID] || (n.Enabled && time.Since(m.health[n.ID].attemptedAt) >= time.Duration(m.data.Settings.IntervalSeconds)*time.Second)) {
				due = append(due, n.ID)
			}
		}
		m.mu.Unlock()
		for _, id := range due {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}
