package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTunvn emulates the Shop Bot API so the delivered path can be verified
// without spending real money.
type fakeTunvn struct {
	buyCalls int
	items    []string
	failBuy  bool
}

func (f *fakeTunvn) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/rate", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "usdt_vnd": 25553.0})
	})
	mux.HandleFunc("/api/balance", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "user_id": 1, "username": "fake",
			"balance_vnd": 0, "balance_usdt": 100.0, "usdt_vnd": 25553.0,
		})
	})
	mux.HandleFunc("/api/products", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"products": []map[string]any{{
				"id": 97, "name": "MUSE AI 1B TOKEN", "price_vnd": 1600,
				"price_usdt": 0.06, "stock": 500, "description": "test product",
			}},
		})
	})
	mux.HandleFunc("/api/buy", func(w http.ResponseWriter, r *http.Request) {
		f.buyCalls++
		if f.failBuy {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "error": "Insufficient balance. Need 0.06 USDT, have 0.0013 USDT",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"order": map[string]any{
				"order_group": "ORD-TEST-1", "product": "MUSE AI 1B TOKEN",
				"quantity": 1, "bonus": 0, "total_items": 1,
				"total_price": 0.06, "currency": "USDT",
			},
			"items":       f.items,
			"new_balance": 99.94,
		})
	})
	return mux
}

func newDeliveredServer(t *testing.T, fake *fakeTunvn) (*Server, *httptest.Server, *Store) {
	t.Helper()

	upstream := httptest.NewServer(fake.handler())
	t.Cleanup(upstream.Close)

	cfg := &Config{
		TunvnBaseURL:     upstream.URL,
		TunvnAPIKey:      "fake-key",
		TunvnCurrency:    "usdt",
		AdapterAPIKey:    "test-key",
		AdapterAPISecret: "test-secret",
		SiteName:         "fake",
		BuyTimeout:       10 * time.Second,
		ProductsTTL:      time.Minute,
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(cfg, NewTunvnClient(cfg), store)
	ts := httptest.NewServer(server.Routes())
	t.Cleanup(ts.Close)
	return server, ts, store
}

func TestDeliveredPathExposesPayload(t *testing.T) {
	fake := &fakeTunvn{items: []string{"user@example.com | https://mail.example/x"}}
	_, ts, store := newDeliveredServer(t, fake)

	body := []byte(`{"sku_id":97,"quantity":1,"downstream_order_no":"ORD-OK-1"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/upstream/orders", strings.NewReader(string(body)))
	tsv := time.Now().Unix()
	req.Header.Set(HeaderAPIKey, "test-key")
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", tsv))
	req.Header.Set(HeaderSignature, Sign("test-secret", http.MethodPost, "/api/v1/upstream/orders", tsv, body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var created struct {
		OK      bool   `json:"ok"`
		OrderID uint   `json:"order_id"`
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !created.OK || created.Status != "delivered" {
		t.Fatalf("expected delivered order, got %+v", created)
	}
	if created.OrderNo == "" {
		t.Fatal("order_no must be populated for downstream reconciliation")
	}
	if fake.buyCalls != 1 {
		t.Fatalf("tunvn buy called %d times, want exactly 1", fake.buyCalls)
	}

	stored, found := store.GetByID(created.OrderID)
	if !found {
		t.Fatal("order missing from store")
	}
	if stored.Payload != fake.items[0] {
		t.Fatalf("payload = %q, want %q", stored.Payload, fake.items[0])
	}
	if stored.DeliveredAt == nil {
		t.Fatal("delivered_at not set")
	}

	// Replaying the same downstream order must not buy again.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/upstream/orders", strings.NewReader(string(body)))
	req2.Header.Set(HeaderAPIKey, "test-key")
	req2.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", time.Now().Unix()))
	req2.Header.Set(HeaderSignature, Sign("test-secret", http.MethodPost, "/api/v1/upstream/orders", time.Now().Unix(), body))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if fake.buyCalls != 1 {
		t.Fatalf("replay triggered a second purchase (%d calls)", fake.buyCalls)
	}

	// Order detail must expose the delivered items to the polling fallback.
	detailPath := fmt.Sprintf("/api/v1/upstream/orders/%d", created.OrderID)
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+detailPath, nil)
	tsv3 := time.Now().Unix()
	req3.Header.Set(HeaderAPIKey, "test-key")
	req3.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", tsv3))
	req3.Header.Set(HeaderSignature, Sign("test-secret", http.MethodGet, detailPath, tsv3, nil))
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()

	var detail struct {
		Status      string `json:"status"`
		Fulfillment *struct {
			Status       string `json:"status"`
			Payload      string `json:"payload"`
			DeliveryData struct {
				Items []string `json:"items"`
			} `json:"delivery_data"`
		} `json:"fulfillment"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.Fulfillment == nil || detail.Fulfillment.Payload != fake.items[0] {
		t.Fatalf("fulfillment not exposed: %+v", detail)
	}
	if len(detail.Fulfillment.DeliveryData.Items) != 1 {
		t.Fatalf("delivery_data.items missing: %+v", detail.Fulfillment)
	}
}

func TestDeterministicFailureDoesNotChargeAndHidesBalance(t *testing.T) {
	fake := &fakeTunvn{failBuy: true}
	_, ts, _ := newDeliveredServer(t, fake)

	body := []byte(`{"sku_id":97,"quantity":1,"downstream_order_no":"ORD-FAIL-1"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/upstream/orders", strings.NewReader(string(body)))
	tsv := time.Now().Unix()
	req.Header.Set(HeaderAPIKey, "test-key")
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", tsv))
	req.Header.Set(HeaderSignature, Sign("test-secret", http.MethodPost, "/api/v1/upstream/orders", tsv, body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var failure struct {
		OK        bool   `json:"ok"`
		ErrorCode string `json:"error_code"`
		Message   string `json:"error_message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&failure)

	if failure.OK {
		t.Fatal("expected failure response")
	}
	if failure.ErrorCode != "insufficient_balance" {
		t.Fatalf("error_code = %q, want insufficient_balance", failure.ErrorCode)
	}
	if strings.Contains(failure.Message, "0.0013") {
		t.Fatalf("downstream response leaked the reseller wallet balance: %q", failure.Message)
	}
	if fake.buyCalls != 1 {
		t.Fatalf("buy calls = %d", fake.buyCalls)
	}
}

func TestQuantityLimitRejectedBeforeCharging(t *testing.T) {
	fake := &fakeTunvn{items: []string{"x"}}
	_, ts, _ := newDeliveredServer(t, fake)

	body := []byte(`{"sku_id":97,"quantity":101,"downstream_order_no":"ORD-BIG-1"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/upstream/orders", strings.NewReader(string(body)))
	tsv := time.Now().Unix()
	req.Header.Set(HeaderAPIKey, "test-key")
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", tsv))
	req.Header.Set(HeaderSignature, Sign("test-secret", http.MethodPost, "/api/v1/upstream/orders", tsv, body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fake.buyCalls != 0 {
		t.Fatal("over-limit quantity must not reach the upstream API")
	}
}
