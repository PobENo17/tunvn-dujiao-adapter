package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Mock downstream site (stands in for a real Dujiao-Next install).
// It signs requests like the real adapter client and verifies our callbacks.
const (
	callbackPath    = "/api/v1/upstream/callback"
	HeaderAPIKey    = "Dujiao-Next-Api-Key"
	HeaderTimestamp = "Dujiao-Next-Timestamp"
	HeaderSignature = "Dujiao-Next-Signature"
)

func main() {
	base := envOr("ADAPTER_URL", "http://127.0.0.1:8080")
	key := envOr("ADAPTER_API_KEY", "")
	secret := envOr("ADAPTER_API_SECRET", "")
	if key == "" || secret == "" {
		log.Fatal("ADAPTER_API_KEY / ADAPTER_API_SECRET required")
	}

	callbackAddr := envOr("CALLBACK_LISTEN", "127.0.0.1:9099")
	callbackPublic := envOr("CALLBACK_PUBLIC", "http://127.0.0.1:9099"+callbackPath)

	received := make(chan map[string]any, 4)
	go serveCallback(callbackAddr, secret, key, received)

	// 1. ping
	body, status := call(base, key, secret, http.MethodPost, "/api/v1/upstream/ping", nil)
	fmt.Printf("PING %d %s\n", status, string(body))

	// 2. products
	body, status = call(base, key, secret, http.MethodGet, "/api/v1/upstream/products?page=1&page_size=5", nil)
	fmt.Printf("PRODUCTS %d %s\n", status, truncate(string(body), 900))

	var listing struct {
		Items []struct {
			ID    uint              `json:"id"`
			Title map[string]string `json:"title"`
			Price string            `json:"price_amount"`
			Skus  []struct {
				ID          uint   `json:"id"`
				StockQty    int    `json:"stock_quantity"`
				StockStatus string `json:"stock_status"`
			} `json:"skus"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		log.Fatalf("decode products: %v (%s)", err, string(body))
	}
	if len(listing.Items) == 0 {
		log.Fatal("no products returned")
	}

	// Pick the cheapest in-stock product to limit real spend.
	chosen := listing.Items[0]
	for _, item := range listing.Items {
		if len(item.Skus) > 0 && item.Skus[0].StockQty > 0 {
			chosen = item
			break
		}
	}
	fmt.Printf("CHOSEN product=%d title=%v price=%s sku=%d stock=%d\n",
		chosen.ID, chosen.Title["zh-CN"], chosen.Price, chosen.Skus[0].ID, chosen.Skus[0].StockQty)

	if os.Getenv("DRY_RUN") == "1" {
		fmt.Println("DRY_RUN set: stopping before purchase")
		return
	}

	// 3. buy (charges the real wallet)
	orderNo := "MOCK-" + strconv.FormatInt(time.Now().Unix(), 10)
	payload := map[string]any{
		"sku_id":              chosen.Skus[0].ID,
		"quantity":            1,
		"downstream_order_no": orderNo,
		"trace_id":            "trace-" + orderNo,
		"callback_url":        callbackPublic,
	}
	body, status = call(base, key, secret, http.MethodPost, "/api/v1/upstream/orders", payload)
	fmt.Printf("CREATE %d %s\n", status, string(body))

	var created struct {
		OK      bool   `json:"ok"`
		OrderID uint   `json:"order_id"`
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal(body, &created)

	// 4. idempotent replay must not charge twice
	body, status = call(base, key, secret, http.MethodPost, "/api/v1/upstream/orders", payload)
	fmt.Printf("REPLAY %d %s\n", status, string(body))

	// 5. wait for the signed callback
	select {
	case cb := <-received:
		fmt.Printf("CALLBACK %s\n", mustJSON(cb))
	case <-time.After(120 * time.Second):
		log.Fatal("callback never arrived")
	}

	// 6. order detail must expose the delivered payload
	body, status = call(base, key, secret, http.MethodGet, fmt.Sprintf("/api/v1/upstream/orders/%d", created.OrderID), nil)
	fmt.Printf("DETAIL %d %s\n", status, truncate(string(body), 900))

	// 7. cancel must be refused
	body, status = call(base, key, secret, http.MethodPost, fmt.Sprintf("/api/v1/upstream/orders/%d/cancel", created.OrderID), nil)
	fmt.Printf("CANCEL %d %s\n", status, string(body))
}

func serveCallback(addr, secret, apiKey string, out chan<- map[string]any) {
	http.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, err := strconv.ParseInt(r.Header.Get(HeaderTimestamp), 10, 64)
		if err != nil {
			http.Error(w, "bad timestamp", http.StatusBadRequest)
			return
		}
		if !verify(secret, r.Method, callbackPath, r.Header.Get(HeaderSignature), ts, body) {
			log.Printf("CALLBACK signature invalid")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":false,"message":"signature verification failed"}`))
			return
		}
		if r.Header.Get(HeaderAPIKey) != apiKey {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":false,"message":"invalid api key"}`))
			return
		}
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		out <- payload
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	log.Printf("mock downstream callback listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func call(base, key, secret, method, path string, payload any) ([]byte, int) {
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	ts := time.Now().Unix()
	req, err := http.NewRequest(method, base+path, bytes.NewReader(raw))
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set(HeaderAPIKey, key)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, sign(secret, method, pathOnly(path), ts, raw))
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode
}

func pathOnly(p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] == '?' {
			return p[:i]
		}
	}
	return p
}

func sign(secret, method, path string, ts int64, body []byte) string {
	sum := md5.Sum(body)
	s := fmt.Sprintf("%s\n%s\n%d\n%s", method, path, ts, hex.EncodeToString(sum[:]))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(s))
	return hex.EncodeToString(mac.Sum(nil))
}

func verify(secret, method, path, sig string, ts int64, body []byte) bool {
	return hmac.Equal([]byte(sign(secret, method, path, ts, body)), []byte(sig))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
