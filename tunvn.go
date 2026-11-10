package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TunvnProduct mirrors one entry of GET /api/products.
type TunvnProduct struct {
	ID                uint    `json:"id"`
	Name              string  `json:"name"`
	PriceVND          int64   `json:"price_vnd"`
	PriceUSDT         float64 `json:"price_usdt"`
	DiscountThreshold int64   `json:"discount_threshold"`
	DiscountPriceVND  int64   `json:"discount_price_vnd"`
	DiscountThreshUSD float64 `json:"discount_threshold_usdt"`
	DiscountPriceUSD  float64 `json:"discount_price_usdt"`
	Stock             int64   `json:"stock"`
	Description       string  `json:"description"`
}

type TunvnBalance struct {
	UserID      int64   `json:"user_id"`
	Username    string  `json:"username"`
	BalanceVND  float64 `json:"balance_vnd"`
	BalanceUSDT float64 `json:"balance_usdt"`
	USDTtoVND   float64 `json:"usdt_vnd"`
}

type TunvnBuyOrder struct {
	OrderGroup string  `json:"order_group"`
	Product    string  `json:"product"`
	Quantity   int     `json:"quantity"`
	Bonus      int     `json:"bonus"`
	TotalItems int     `json:"total_items"`
	TotalPrice float64 `json:"total_price"`
	Currency   string  `json:"currency"`
}

type TunvnBuyResult struct {
	Order      TunvnBuyOrder `json:"order"`
	Items      []string      `json:"items"`
	NewBalance float64       `json:"new_balance"`
}

// TunvnError is a business failure returned by the shop API (success=false).
type TunvnError struct {
	Status  int
	Message string
}

func (e *TunvnError) Error() string {
	return fmt.Sprintf("tunvn %d: %s", e.Status, e.Message)
}

// Client talks to the TUNVN Shop Bot API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	mu         sync.Mutex
	products   []TunvnProduct
	fetchedAt  time.Time
	productsTT time.Duration
}

func NewTunvnClient(cfg *Config) *Client {
	return &Client{
		baseURL:    cfg.TunvnBaseURL,
		apiKey:     cfg.TunvnAPIKey,
		http:       &http.Client{Timeout: 45 * time.Second},
		productsTT: cfg.ProductsTTL,
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		msg := strings.TrimSpace(envelope.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return &TunvnError{Status: resp.StatusCode, Message: msg}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// Rate returns the USDT→VND rate.
func (c *Client) Rate(ctx context.Context) (float64, error) {
	var out struct {
		Success bool    `json:"success"`
		USDTVND float64 `json:"usdt_vnd"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/rate", nil, &out); err != nil {
		return 0, err
	}
	if !out.Success || out.USDTVND <= 0 {
		return 0, errors.New("invalid rate response")
	}
	return out.USDTVND, nil
}

// Balance returns the wallet balance of the configured API key.
func (c *Client) Balance(ctx context.Context) (*TunvnBalance, error) {
	var out struct {
		Success bool `json:"success"`
		TunvnBalance
	}
	if err := c.do(ctx, http.MethodGet, "/api/balance", nil, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, errors.New("balance request failed")
	}
	result := out.TunvnBalance
	return &result, nil
}

// Products returns the cached product list, refreshing when stale.
func (c *Client) Products(ctx context.Context) ([]TunvnProduct, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.products) > 0 && time.Since(c.fetchedAt) < c.productsTT {
		return c.products, nil
	}

	var out struct {
		Success  bool           `json:"success"`
		Products []TunvnProduct `json:"products"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/products", nil, &out); err != nil {
		if len(c.products) > 0 {
			return c.products, nil
		}
		return nil, err
	}
	c.products = out.Products
	c.fetchedAt = time.Now()
	return c.products, nil
}

// Product looks up a single product from the cached list.
func (c *Client) Product(ctx context.Context, id uint) (*TunvnProduct, error) {
	products, err := c.Products(ctx)
	if err != nil {
		return nil, err
	}
	for i := range products {
		if products[i].ID == id {
			found := products[i]
			return &found, nil
		}
	}
	return nil, nil
}

// Buy places an order and returns the delivered account strings.
// This call charges the wallet and has no idempotency key: callers must
// guarantee at most one invocation per downstream order.
func (c *Client) Buy(ctx context.Context, productID uint, quantity int, currency string) (*TunvnBuyResult, error) {
	body := map[string]any{
		"product_id": productID,
		"quantity":   quantity,
		"currency":   currency,
	}
	var out struct {
		Success bool          `json:"success"`
		Order   TunvnBuyOrder `json:"order"`
		Items   []string      `json:"items"`
		Balance *float64      `json:"new_balance"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/buy", body, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, errors.New("buy rejected")
	}
	result := &TunvnBuyResult{Order: out.Order, Items: out.Items}
	if out.Balance != nil {
		result.NewBalance = *out.Balance
	}
	return result, nil
}

// UnitPrice returns the effective unit price of a product in the configured
// currency as fixed-point micros. Discount pricing is ignored on purpose:
// TUNVN decides the real charge server side, and the adapter only needs a
// display price for the downstream catalogue.
func UnitPrice(p TunvnProduct, currency string) micros {
	if strings.EqualFold(currency, "vnd") {
		return micros(p.PriceVND) * microScale
	}
	return micros(int64(p.PriceUSDT*microScale + 0.5))
}
