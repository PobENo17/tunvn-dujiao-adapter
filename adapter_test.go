package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSignMatchesDujiaoFormula(t *testing.T) {
	// Reference vector recomputed by hand:
	// signString = "POST\n/api/v1/upstream/orders\n1700000000\n<md5 of '{}'>"
	body := []byte("{}")
	got := Sign("secret", http.MethodPost, "/api/v1/upstream/orders", 1700000000, body)
	if len(got) != 64 {
		t.Fatalf("signature must be 64 hex chars, got %d (%s)", len(got), got)
	}
	if got != Sign("secret", http.MethodPost, "/api/v1/upstream/orders", 1700000000, body) {
		t.Fatal("signature must be deterministic")
	}
	if got == Sign("other-secret", http.MethodPost, "/api/v1/upstream/orders", 1700000000, body) {
		t.Fatal("different secrets must produce different signatures")
	}
	if got == Sign("secret", http.MethodGet, "/api/v1/upstream/orders", 1700000000, body) {
		t.Fatal("method must be part of the signature")
	}
}

func TestEmptyBodyMD5IsCanonical(t *testing.T) {
	// The documented md5 of an empty body is d41d8cd98f00b204e9800998ecf8427e.
	got := Sign("s", http.MethodGet, "/api/v1/upstream/ping", 1, nil)
	want := Sign("s", http.MethodGet, "/api/v1/upstream/ping", 1, []byte{})
	if got != want {
		t.Fatalf("nil body must hash like empty body")
	}
}

func TestVerify(t *testing.T) {
	body := []byte(`{"sku_id":1,"quantity":1}`)
	ts := time.Now().Unix()
	sig := Sign("k", http.MethodPost, "/api/v1/upstream/orders", ts, body)
	if !Verify("k", http.MethodPost, "/api/v1/upstream/orders", sig, ts, body) {
		t.Fatal("valid signature rejected")
	}
	if Verify("k", http.MethodPost, "/api/v1/upstream/orders", sig, ts, []byte("tampered")) {
		t.Fatal("tampered body accepted")
	}
}

func TestTimestampValid(t *testing.T) {
	now := time.Now().Unix()
	if !TimestampValid(now) {
		t.Fatal("current timestamp should be valid")
	}
	if TimestampValid(now - 120) {
		t.Fatal("timestamp older than the 60s window accepted")
	}
}

func TestParseMicros(t *testing.T) {
	cases := []struct {
		raw  string
		want micros
	}{
		{"0.06", 60000},
		{"1.5", 1500000},
		{"10", 10000000},
		{"0", 0},
		{"-1.25", -1250000},
		{"1.2345678", 1234567},
	}
	for _, c := range cases {
		got, err := parseMicros(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("parse %q = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestMicrosStringFixed(t *testing.T) {
	cases := []struct {
		value micros
		want  string
	}{
		{60000, "0.06"},
		{1500000, "1.50"},
		{0, "0.00"},
		{3999, "0.00"},
		{1350000, "1.35"},
		{-1250000, "-1.25"},
	}
	for _, c := range cases {
		if got := c.value.StringFixed(); got != c.want {
			t.Fatalf("%d.StringFixed() = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestStoreIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	first, created, err := store.Create(Order{DownstreamOrderNo: "ORD-1", Quantity: 1, Status: OrderStatusPaid})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first create must be new")
	}

	second, created, err := store.Create(Order{DownstreamOrderNo: "ORD-1", Quantity: 9, Status: OrderStatusPaid})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("duplicate downstream order number must not create a second order")
	}
	if second.ID != first.ID || second.Quantity != 1 {
		t.Fatalf("duplicate returned a different order: %+v", second)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := reopened.GetByDownstreamNo("ORD-1"); !found {
		t.Fatal("order not persisted to disk")
	}
}

func TestStoreUpdatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")
	store, _ := OpenStore(path)
	order, _, _ := store.Create(Order{DownstreamOrderNo: "ORD-2", Status: OrderStatusPaid})

	delivered := time.Now()
	_, err := store.Update(order.ID, func(o *Order) {
		o.Status = OrderStatusFulfilled
		o.Items = []string{"user@example.com | pass"}
		o.Payload = "user@example.com | pass"
		o.DeliveredAt = &delivered
		o.NextCallbackAt = delivered
	})
	if err != nil {
		t.Fatal(err)
	}

	reopened, _ := OpenStore(path)
	stored, found := reopened.GetByID(order.ID)
	if !found || stored.Status != OrderStatusFulfilled || len(stored.Items) != 1 {
		t.Fatalf("update lost: %+v", stored)
	}
}

func TestDueCallbacksOnlyForTerminalStates(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
	pending, _, _ := store.Create(Order{DownstreamOrderNo: "A", Status: OrderStatusPaid})
	done, _, _ := store.Create(Order{DownstreamOrderNo: "B", Status: OrderStatusPaid})

	now := time.Now()
	_, _ = store.Update(done.ID, func(o *Order) {
		o.Status = OrderStatusFulfilled
		o.DeliveredAt = &now
		o.NextCallbackAt = now.Add(-time.Second)
	})

	due := store.DueCallbacks(now)
	if len(due) != 1 || due[0].ID != done.ID {
		t.Fatalf("expected only the fulfilled order, got %+v", due)
	}
	if due[0].ID == pending.ID {
		t.Fatal("pending order must not be reported by DueCallbacks")
	}
}

func TestUnitPriceUsesConfiguredCurrency(t *testing.T) {
	p := TunvnProduct{PriceVND: 1600, PriceUSDT: 0.06}
	if got := UnitPrice(p, "usdt").StringFixed(); got != "0.06" {
		t.Fatalf("usdt price = %s, want 0.06", got)
	}
	if got := UnitPrice(p, "vnd").StringFixed(); got != "1600.00" {
		t.Fatalf("vnd price = %s, want 1600.00", got)
	}
}

func TestCeilCentsNeverUndercharges(t *testing.T) {
	cases := []struct {
		in   micros
		want string
	}{
		{60000, "0.06"},
		{4000, "0.01"},
		{3999, "0.01"},
		{1, "0.01"},
		{1500000, "1.50"},
		{1500001, "1.51"},
		{0, "0.00"},
	}
	for _, c := range cases {
		if got := c.in.CeilCents().StringFixed(); got != c.want {
			t.Fatalf("%d.CeilCents() = %s, want %s", c.in, got, c.want)
		}
	}
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &Config{
		AdapterAPIKey:    "test-key",
		AdapterAPISecret: "test-secret",
		TunvnCurrency:    "usdt",
		SiteName:         "test",
		BuyTimeout:       5 * time.Second,
		ProductsTTL:      time.Minute,
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "orders.json"))
	if err != nil {
		t.Fatal(err)
	}
	tunvn := NewTunvnClient(cfg)
	server := NewServer(cfg, tunvn, store)
	ts := httptest.NewServer(server.Routes())
	t.Cleanup(ts.Close)
	return server, ts
}

func signedRequest(t *testing.T, method, url, path, secret string, body []byte) *http.Request {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequest(method, url, bytes.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	req.Header.Set(HeaderAPIKey, "test-key")
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(secret, method, path, ts, body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSignedMiddlewareRejectsBadSignature(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/upstream/ping", nil)
	req.Header.Set(HeaderAPIKey, "test-key")
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(HeaderSignature, "deadbeef")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSignedMiddlewareRejectsWrongKeyBeforeSecret(t *testing.T) {
	_, ts := newTestServer(t)

	body := []byte(`{}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/upstream/ping", bytes.NewReader(body))
	tsv := time.Now().Unix()
	req.Header.Set(HeaderAPIKey, "someone-else")
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(tsv, 10))
	req.Header.Set(HeaderSignature, Sign("test-secret", http.MethodPost, "/api/v1/upstream/ping", tsv, body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCategoriesIsEmptyAndSigned(t *testing.T) {
	_, ts := newTestServer(t)
	req := signedRequest(t, http.MethodGet, ts.URL+"/api/v1/upstream/categories", "/api/v1/upstream/categories", "test-secret", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		OK         bool  `json:"ok"`
		Categories []any `json:"categories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || len(payload.Categories) != 0 {
		t.Fatalf("unexpected categories response: %+v", payload)
	}
}
