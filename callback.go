package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// CallbackPayload is the stable JSON contract Dujiao-Next expects on
// POST /api/v1/upstream/callback (see internal/modules/downstreamcallback).
type CallbackPayload struct {
	Event             string       `json:"event"`
	OrderID           uint         `json:"order_id"`
	OrderNo           string       `json:"order_no"`
	DownstreamOrderNo string       `json:"downstream_order_no"`
	Status            string       `json:"status"`
	Fulfillment       *Fulfillment `json:"fulfillment,omitempty"`
	Timestamp         int64        `json:"timestamp"`
}

type Fulfillment struct {
	Type         string         `json:"type"`
	Status       string         `json:"status"`
	Payload      string         `json:"payload"`
	DeliveryData map[string]any `json:"delivery_data"`
	DeliveredAt  *time.Time     `json:"delivered_at,omitempty"`
}

type Notifier struct {
	cfg    *Config
	store  *Store
	client *http.Client
}

func NewNotifier(cfg *Config, store *Store) *Notifier {
	return &Notifier{
		cfg:   cfg,
		store: store,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

var callbackBackoff = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
}

// Run drains pending delivery callbacks until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.flush(ctx)
		}
	}
}

func (n *Notifier) flush(ctx context.Context) {
	for _, order := range n.store.DueCallbacks(time.Now()) {
		if err := n.deliver(ctx, order); err != nil {
			logf("callback %s failed (attempt %d): %v", order.DownstreamOrderNo, order.CallbackAttempts+1, err)
			_, _ = n.store.Update(order.ID, func(o *Order) {
				o.CallbackAttempts++
				idx := o.CallbackAttempts - 1
				if idx >= len(callbackBackoff) {
					idx = len(callbackBackoff) - 1
				}
				o.NextCallbackAt = time.Now().Add(callbackBackoff[idx])
				o.LastError = "callback: " + err.Error()
			})
			continue
		}
		_, _ = n.store.Update(order.ID, func(o *Order) {
			o.CallbackSent = true
			o.CallbackAttempts++
		})
		logf("callback %s delivered", order.DownstreamOrderNo)
	}
}

func (n *Notifier) deliver(ctx context.Context, order Order) error {
	if strings.TrimSpace(order.CallbackURL) == "" {
		return nil
	}

	event := "order.status_changed"
	if order.Status == OrderStatusFulfilled {
		event = "order.fulfilled"
	}

	payload := CallbackPayload{
		Event:             event,
		OrderID:           order.ID,
		OrderNo:           order.LocalOrderNo,
		DownstreamOrderNo: order.DownstreamOrderNo,
		Status:            order.Status,
		Timestamp:         time.Now().Unix(),
	}
	if order.Status == OrderStatusFulfilled {
		payload.Fulfillment = &Fulfillment{
			Type:         "auto",
			Status:       "delivered",
			Payload:      order.Payload,
			DeliveryData: map[string]any{"items": order.Items},
			DeliveredAt:  order.DeliveredAt,
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	signature := Sign(n.cfg.AdapterAPISecret, http.MethodPost, CallbackPath, payload.Timestamp, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, order.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderAPIKey, n.cfg.AdapterAPIKey)
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", payload.Timestamp))
	req.Header.Set(HeaderSignature, signature)

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("callback returned status %d", resp.StatusCode)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("callback response unreadable: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("callback rejected by downstream")
	}
	return nil
}
