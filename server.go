package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	cfg   *Config
	tunvn *Client
	store *Store

	buyMu  sync.Mutex
	buying map[string]bool
}

func NewServer(cfg *Config, tunvn *Client, store *Store) *Server {
	return &Server{cfg: cfg, tunvn: tunvn, store: store, buying: map[string]bool{}}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/v1/upstream/ping", s.signed(s.handlePing))
	mux.HandleFunc("/api/v1/upstream/categories", s.signed(s.handleCategories))
	mux.HandleFunc("/api/v1/upstream/products", s.signed(s.handleListProducts))
	mux.HandleFunc("/api/v1/upstream/products/", s.signed(s.handleGetProduct))
	mux.HandleFunc("/api/v1/upstream/orders", s.signed(s.handleCreateOrder))
	mux.HandleFunc("/api/v1/upstream/orders/", s.signed(s.handleOrderPath))

	mux.HandleFunc("/admin/orders", s.handleAdminOrders)

	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func okJSON(w http.ResponseWriter, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["ok"] = true
	writeJSON(w, http.StatusOK, payload)
}

func errJSON(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"ok":            false,
		"error_code":    code,
		"error_message": message,
	})
}

// signed enforces the Dujiao-Next HMAC handshake on every upstream route.
func (s *Server) signed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get(HeaderAPIKey)
		tsRaw := r.Header.Get(HeaderTimestamp)
		signature := r.Header.Get(HeaderSignature)
		if apiKey == "" || tsRaw == "" || signature == "" {
			errJSON(w, http.StatusUnauthorized, "unauthorized", "missing authentication headers")
			return
		}
		if apiKey != s.cfg.AdapterAPIKey {
			errJSON(w, http.StatusUnauthorized, "unauthorized", "invalid api key")
			return
		}
		timestamp, err := ParseTimestamp(tsRaw)
		if err != nil {
			errJSON(w, http.StatusUnauthorized, "unauthorized", "invalid timestamp")
			return
		}
		if !TimestampValid(timestamp) {
			errJSON(w, http.StatusUnauthorized, "unauthorized", "timestamp expired")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			errJSON(w, http.StatusBadRequest, "bad_request", "failed to read request body")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))

		if !Verify(s.cfg.AdapterAPISecret, r.Method, r.URL.Path, signature, timestamp, body) {
			errJSON(w, http.StatusUnauthorized, "unauthorized", "signature verification failed")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	balance, err := s.tunvn.Balance(ctx)
	if err != nil {
		log.Printf("ping balance error: %v", err)
		okJSON(w, map[string]any{
			"site_name":        s.cfg.SiteName,
			"protocol_version": "1.0",
			"user_id":          1,
			"balance":          "0.00",
			"currency":         strings.ToUpper(s.cfg.TunvnCurrency),
			"member_level":     nil,
		})
		return
	}

	available := balance.BalanceUSDT
	if s.cfg.TunvnCurrency == "vnd" {
		available = balance.BalanceVND
	}

	okJSON(w, map[string]any{
		"site_name":        s.cfg.SiteName,
		"protocol_version": "1.0",
		"user_id":          1,
		"balance":          formatMoney(available),
		"currency":         strings.ToUpper(s.cfg.TunvnCurrency),
		"member_level":     nil,
	})
}

func formatMoney(v float64) string {
	return micros(int64(v*microScale + 0.5)).StringFixed()
}

// TUNVN exposes no categories; an empty list makes Dujiao fall back gracefully.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	okJSON(w, map[string]any{"categories": []any{}})
}

func (s *Server) handleListProducts(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	products, err := s.tunvn.Products(ctx)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "upstream_error", "failed to fetch products: "+err.Error())
		return
	}

	total := len(products)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	items := make([]map[string]any, 0, end-start)
	for _, p := range products[start:end] {
		items = append(items, s.toUpstreamProduct(p))
	}

	okJSON(w, map[string]any{
		"items":             items,
		"total":             total,
		"page":              page,
		"page_size":         pageSize,
		"includes_inactive": true,
	})
}

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/api/v1/upstream/products/")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "invalid product id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	product, err := s.tunvn.Product(ctx, uint(id))
	if err != nil {
		errJSON(w, http.StatusBadGateway, "upstream_error", "failed to fetch product: "+err.Error())
		return
	}
	if product == nil {
		errJSON(w, http.StatusNotFound, "product_not_found", "product not found")
		return
	}

	okJSON(w, map[string]any{"product": s.toUpstreamProduct(*product)})
}

// toUpstreamProduct maps a TUNVN product onto the Dujiao-Next catalogue shape.
// TUNVN has no SKUs, so every product exposes exactly one DEFAULT SKU whose id
// equals the TUNVN product id.
func (s *Server) toUpstreamProduct(p TunvnProduct) map[string]any {
	currency := strings.ToUpper(s.cfg.TunvnCurrency)
	price := UnitPrice(p, s.cfg.TunvnCurrency).CeilCents().StringFixed()
	active := p.Stock != 0

	stockStatus := "out_of_stock"
	switch {
	case p.Stock < 0:
		stockStatus = "unlimited"
	case p.Stock > 20:
		stockStatus = "in_stock"
	case p.Stock > 0:
		stockStatus = "low_stock"
	}

	stockQty := int(p.Stock)
	if p.Stock < 0 {
		stockQty = -1
	}

	sku := map[string]any{
		"id":             p.ID,
		"sku_code":       "DEFAULT",
		"spec_values":    map[string]any{},
		"price_amount":   price,
		"stock_status":   stockStatus,
		"stock_quantity": stockQty,
		"is_active":      active,
	}

	return map[string]any{
		"id":    p.ID,
		"slug":  fmt.Sprintf("tunvn-%d", p.ID),
		"title": map[string]any{"zh-CN": p.Name, "zh-TW": p.Name, "en": p.Name},
		"description": map[string]any{
			"zh-CN": truncate(p.Description, 2000),
			"en":    truncate(p.Description, 2000),
		},
		"content": map[string]any{
			"zh-CN": p.Description,
			"en":    p.Description,
		},
		"seo_meta":         map[string]any{},
		"images":           []string{},
		"tags":             []string{},
		"price_amount":     price,
		"currency":         currency,
		"fulfillment_type": "auto",
		"is_active":        active,
		"category_id":      0,
		"skus":             []any{sku},
		"created_at":       time.Now().UTC().Format(time.RFC3339),
		"updated_at":       time.Now().UTC().Format(time.RFC3339),
	}
}

func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

type createOrderRequest struct {
	SKUID             uint           `json:"sku_id"`
	Quantity          int            `json:"quantity"`
	ManualFormData    map[string]any `json:"manual_form_data"`
	DownstreamOrderNo string         `json:"downstream_order_no"`
	TraceID           string         `json:"trace_id"`
	CallbackURL       string         `json:"callback_url"`
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}

	var req createOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return
	}
	if req.SKUID == 0 || req.Quantity < 1 {
		errJSON(w, http.StatusBadRequest, "bad_request", "sku_id and quantity are required")
		return
	}
	if req.Quantity > 100 {
		errJSON(w, http.StatusBadRequest, "bad_request", "quantity exceeds the upstream limit of 100")
		return
	}
	if strings.TrimSpace(req.DownstreamOrderNo) == "" {
		errJSON(w, http.StatusBadRequest, "bad_request", "downstream_order_no is required")
		return
	}

	// Idempotency: a repeated downstream order number must never trigger a
	// second TUNVN purchase, because /api/buy has no dedupe key.
	if existing, found := s.store.GetByDownstreamNo(req.DownstreamOrderNo); found {
		log.Printf("idempotent replay for %s -> order %d (%s)", req.DownstreamOrderNo, existing.ID, existing.Status)
		s.respondOrderState(w, *existing)
		return
	}

	callbackURL := strings.TrimSpace(req.CallbackURL)
	if callbackURL == "" {
		callbackURL = s.cfg.DefaultCallbackURL
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	product, err := s.tunvn.Product(ctx, req.SKUID)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "upstream_error", "failed to load product: "+err.Error())
		return
	}
	if product == nil {
		errJSON(w, http.StatusBadRequest, "product_unavailable", "product not found")
		return
	}
	if product.Stock == 0 {
		errJSON(w, http.StatusConflict, "insufficient_stock", "product is out of stock")
		return
	}

	// The reported amount must equal the catalogue price the downstream site
	// already showed the buyer, so both go through the same rounding.
	unit := UnitPrice(*product, s.cfg.TunvnCurrency).CeilCents()
	total := unit.Mul(req.Quantity)
	currency := strings.ToUpper(s.cfg.TunvnCurrency)

	order, created, err := s.store.Create(Order{
		DownstreamOrderNo: req.DownstreamOrderNo,
		TraceID:           req.TraceID,
		CallbackURL:       callbackURL,
		ProductID:         product.ID,
		SKUID:             req.SKUID,
		Title:             product.Name,
		Quantity:          req.Quantity,
		Amount:            total.StringFixed(),
		Currency:          currency,
		Status:            OrderStatusPaid,
	})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "internal_error", "failed to persist order: "+err.Error())
		return
	}
	if !created {
		s.respondOrderState(w, *order)
		return
	}

	// The intent is durable before money is spent, so a crash here leaves a
	// recorded order instead of an untracked charge.
	//
	// The purchase runs concurrently but is awaited briefly: deterministic
	// failures (e.g. insufficient wallet balance) then reach the downstream site
	// as an immediate error instead of a paid-then-rolled-back order. Slow
	// upstream calls still answer "paid" and finish in the background, with the
	// result pushed by callback.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.purchase(*order)
	}()

	select {
	case <-done:
		if stored, found := s.store.GetByID(order.ID); found {
			s.respondOrderState(w, *stored)
			return
		}
		s.respondOrderState(w, *order)
	case <-time.After(s.syncBuyWindow()):
		log.Printf("buy for %s still running, answering paid and completing in background", order.DownstreamOrderNo)
		s.respondOrderState(w, *order)
	}
}

// syncBuyWindow bounds how long an HTTP create-order call waits for the actual
// purchase. It stays under the 30s client timeout used by Dujiao-Next.
func (s *Server) syncBuyWindow() time.Duration {
	const max = 25 * time.Second
	if s.cfg.BuyTimeout < max {
		return s.cfg.BuyTimeout
	}
	return max
}

// respondOrderState returns the Dujiao-Next create-order contract for an order
// in any state, so replays and first attempts answer identically.
func (s *Server) respondOrderState(w http.ResponseWriter, order Order) {
	switch order.Status {
	case OrderStatusFulfilled:
		okJSON(w, map[string]any{
			"order_id": order.ID,
			"order_no": order.LocalOrderNo,
			"status":   "delivered",
			"amount":   order.Amount,
			"currency": order.Currency,
		})
	case OrderStatusCanceled:
		code := "product_unavailable"
		message := "upstream rejected the order"
		lower := strings.ToLower(order.LastError)
		if strings.Contains(lower, "insufficient balance") {
			code = "insufficient_balance"
			// The raw upstream text leaks the reseller's wallet balance, so it is
			// deliberately not forwarded to the downstream site.
			message = "upstream balance is insufficient"
		} else if strings.Contains(lower, "out of stock") || strings.Contains(lower, "insufficient") {
			code = "insufficient_stock"
			message = "product stock is insufficient"
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":            false,
			"order_id":      order.ID,
			"order_no":      order.LocalOrderNo,
			"status":        order.Status,
			"error_code":    code,
			"error_message": message,
		})
	default:
		okJSON(w, map[string]any{
			"order_id": order.ID,
			"order_no": order.LocalOrderNo,
			"status":   order.Status,
			"amount":   order.Amount,
			"currency": order.Currency,
		})
	}
}

// purchase performs the TUNVN buy exactly once per order.
func (s *Server) purchase(order Order) {
	s.buyMu.Lock()
	if s.buying[order.DownstreamOrderNo] {
		s.buyMu.Unlock()
		return
	}
	s.buying[order.DownstreamOrderNo] = true
	s.buyMu.Unlock()

	defer func() {
		s.buyMu.Lock()
		delete(s.buying, order.DownstreamOrderNo)
		s.buyMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.BuyTimeout)
	defer cancel()

	result, err := s.tunvn.Buy(ctx, order.ProductID, order.Quantity, s.cfg.TunvnCurrency)
	now := time.Now()

	if err != nil {
		classification := classifyBuyError(err)
		log.Printf("buy failed for %s (%s): %s", order.DownstreamOrderNo, classification, err)

		if classification == "ambiguous" {
			// A timeout may or may not have charged the wallet. Retrying could
			// double-charge, so the order is parked for manual review instead.
			_, _ = s.store.Update(order.ID, func(o *Order) {
				o.Status = OrderStatusPaid
				o.LastError = "ambiguous upstream outcome: " + err.Error()
			})
			return
		}

		_, _ = s.store.Update(order.ID, func(o *Order) {
			o.Status = OrderStatusCanceled
			o.LastError = err.Error()
			o.NextCallbackAt = now
		})
		return
	}

	payload := strings.Join(result.Items, "\n")
	_, _ = s.store.Update(order.ID, func(o *Order) {
		o.Status = OrderStatusFulfilled
		o.Items = result.Items
		o.Payload = payload
		o.TunvnOrderGroup = result.Order.OrderGroup
		o.LastError = ""
		o.DeliveredAt = &now
		o.NextCallbackAt = now
	})
	log.Printf("buy ok for %s -> tunvn %s (%d items)", order.DownstreamOrderNo, result.Order.OrderGroup, len(result.Items))
}

// classifyBuyError separates deterministic failures (safe to reject) from
// ambiguous ones (money may already have moved).
func classifyBuyError(err error) string {
	var apiErr *TunvnError
	if errors.As(err, &apiErr) {
		return "deterministic"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "ambiguous"
	}
	return "ambiguous"
}

func (s *Server) handleOrderPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/upstream/orders/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		errJSON(w, http.StatusBadRequest, "bad_request", "missing order id")
		return
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "bad_request", "invalid order id")
		return
	}

	if len(parts) > 1 && parts[1] == "cancel" {
		s.handleCancelOrder(w, r, uint(id))
		return
	}

	order, found := s.store.GetByID(uint(id))
	if !found {
		errJSON(w, http.StatusNotFound, "order_not_found", "order not found")
		return
	}

	resp := map[string]any{
		"order_id":        order.ID,
		"order_no":        order.LocalOrderNo,
		"status":          order.Status,
		"amount":          order.Amount,
		"refunded_amount": "0.00",
		"currency":        order.Currency,
		"refund_records":  []any{},
		"items": []map[string]any{{
			"product_id":           order.ProductID,
			"sku_id":               order.SKUID,
			"title":                map[string]any{"zh-CN": order.Title, "en": order.Title},
			"quantity":             order.Quantity,
			"original_unit_price":  unitFromTotal(order),
			"unit_price":           unitFromTotal(order),
			"original_total_price": order.Amount,
			"total_price":          order.Amount,
			"fulfillment_type":     "auto",
		}},
	}

	if order.Status == OrderStatusFulfilled {
		resp["fulfillment"] = map[string]any{
			"type":          "auto",
			"status":        "delivered",
			"payload":       order.Payload,
			"delivery_data": map[string]any{"items": order.Items},
			"delivered_at":  order.DeliveredAt,
		}
	}

	okJSON(w, resp)
}

func unitFromTotal(order *Order) string {
	if order.Quantity < 1 {
		return order.Amount
	}
	value, err := parseMicros(order.Amount)
	if err != nil {
		return order.Amount
	}
	return micros(int64(value) / int64(order.Quantity)).StringFixed()
}

// handleCancelOrder always refuses: TUNVN has no refund, so cancelling locally
// would leave the adapter holding a paid, undeliverable order.
func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request, id uint) {
	order, found := s.store.GetByID(id)
	if !found {
		errJSON(w, http.StatusNotFound, "order_not_found", "order not found")
		return
	}
	if order.Status == OrderStatusPaid && order.DeliveredAt == nil {
		// Not yet purchased: safe to mark canceled locally.
		updated, err := s.store.Update(id, func(o *Order) {
			o.Status = OrderStatusCanceled
			o.LastError = "canceled before purchase"
		})
		if err == nil {
			okJSON(w, map[string]any{
				"order_id": updated.ID,
				"order_no": updated.LocalOrderNo,
				"status":   updated.Status,
			})
			return
		}
	}
	errJSON(w, http.StatusConflict, "cancel_not_allowed", "upstream order cannot be canceled once paid")
}

// handleAdminOrders is an unauthenticated-by-design local diagnostics endpoint.
// Bind it to loopback only, or put it behind a reverse proxy rule.
func (s *Server) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "orders": s.store.List(limit)})
}
