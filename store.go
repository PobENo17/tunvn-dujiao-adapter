package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	OrderStatusPaid      = "paid"
	OrderStatusFulfilled = "fulfilled"
	OrderStatusCanceled  = "canceled"
)

// Order is one downstream purchase translated into a TUNVN purchase.
// DownstreamOrderNo is the idempotency key: TUNVN's buy endpoint has none, so
// the adapter must never call it twice for the same key.
type Order struct {
	ID                uint       `json:"id"`
	DownstreamOrderNo string     `json:"downstream_order_no"`
	TraceID           string     `json:"trace_id,omitempty"`
	CallbackURL       string     `json:"callback_url,omitempty"`
	ProductID         uint       `json:"product_id"`
	SKUID             uint       `json:"sku_id"`
	Title             string     `json:"title"`
	Quantity          int        `json:"quantity"`
	Amount            string     `json:"amount"`
	Currency          string     `json:"currency"`
	Status            string     `json:"status"`
	Items             []string   `json:"items,omitempty"`
	Payload           string     `json:"payload,omitempty"`
	LocalOrderNo      string     `json:"local_order_no"`
	TunvnOrderGroup   string     `json:"tunvn_order_group,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	CallbackSent      bool       `json:"callback_sent"`
	CallbackAttempts  int        `json:"callback_attempts"`
	NextCallbackAt    time.Time  `json:"next_callback_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DeliveredAt       *time.Time `json:"delivered_at,omitempty"`
}

type storeFile struct {
	NextID uint    `json:"next_id"`
	Orders []Order `json:"orders"`
}

// Store is a tiny JSON-backed order store. It is safe for concurrent use and
// writes atomically (temp file + rename) so a crash cannot leave a partial file.
type Store struct {
	mu     sync.RWMutex
	path   string
	nextID uint
	orders []Order
	byKey  map[string]int
	byID   map[uint]int
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, nextID: 1, byKey: map[string]int{}, byID: map[uint]int{}}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read store: %w", err)
	}
	if len(raw) == 0 {
		return s, nil
	}

	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse store %s: %w", path, err)
	}
	s.orders = file.Orders
	s.nextID = file.NextID
	if s.nextID == 0 {
		s.nextID = 1
	}
	for i := range s.orders {
		s.byKey[s.orders[i].DownstreamOrderNo] = i
		s.byID[s.orders[i].ID] = i
		if s.orders[i].ID >= s.nextID {
			s.nextID = s.orders[i].ID + 1
		}
	}
	return s, nil
}

func (s *Store) persistLocked() error {
	file := storeFile{NextID: s.nextID, Orders: s.orders}
	payload, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// GetByDownstreamNo returns a copy of the order for that idempotency key.
func (s *Store) GetByDownstreamNo(key string) (*Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	copied := s.orders[idx]
	return &copied, true
}

func (s *Store) GetByID(id uint) (*Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	copied := s.orders[idx]
	return &copied, true
}

// Create inserts a new order. Returns the stored copy and false when an order
// with the same downstream_order_no already exists (the existing one is returned).
func (s *Store) Create(order Order) (*Order, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idx, ok := s.byKey[order.DownstreamOrderNo]; ok {
		existing := s.orders[idx]
		return &existing, false, nil
	}

	now := time.Now()
	order.ID = s.nextID
	s.nextID++
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	// A stable local order number is exposed downstream immediately; it is not
	// overwritten by the TUNVN order group later, because Dujiao stores it as
	// the upstream order reference for reconciliation.
	if order.LocalOrderNo == "" {
		order.LocalOrderNo = fmt.Sprintf("ADP%s%06d", now.Format("20060102150405"), order.ID)
	}
	order.UpdatedAt = now
	s.orders = append(s.orders, order)
	idx := len(s.orders) - 1
	s.byKey[order.DownstreamOrderNo] = idx
	s.byID[order.ID] = idx
	if err := s.persistLocked(); err != nil {
		return nil, false, err
	}
	stored := s.orders[idx]
	return &stored, true, nil
}

// Update applies mutate to the order identified by id and persists the result.
func (s *Store) Update(id uint, mutate func(*Order)) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("order %d not found", id)
	}
	mutate(&s.orders[idx])
	s.orders[idx].UpdatedAt = time.Now()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	stored := s.orders[idx]
	return &stored, nil
}

// List returns orders newest first, capped at limit.
func (s *Store) List(limit int) []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Order, len(s.orders))
	copy(out, s.orders)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// DueCallbacks returns orders whose state-change callback still needs sending.
// Both delivered and canceled orders are reported, so the downstream site does
// not have to wait for its 30s polling fallback.
func (s *Store) DueCallbacks(now time.Time) []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Order
	for _, order := range s.orders {
		if order.CallbackSent {
			continue
		}
		if order.Status != OrderStatusFulfilled && order.Status != OrderStatusCanceled {
			continue
		}
		if order.NextCallbackAt.After(now) {
			continue
		}
		out = append(out, order)
	}
	return out
}
