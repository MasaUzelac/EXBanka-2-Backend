package handler

// my_orders_handler.go — plain HTTP handler for the "Moji nalozi" endpoint.
//
// Endpoint:
//   GET /bank/trading/my-orders?status=<STATUS>
//
// Returns only the orders that belong to the authenticated caller, regardless
// of their role (CLIENT, EMPLOYEE, ADMIN). The optional ?status query param
// filters by order status.
//
// This is intentionally a plain HTTP handler (not gRPC) so we can avoid
// modifying the proto schema.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/trading"
)

// MyOrdersHandler serves GET /bank/trading/my-orders.
type MyOrdersHandler struct {
	tradingService trading.TradingService
	jwtSecret      string
}

// NewMyOrdersHandler constructs the handler.
func NewMyOrdersHandler(tradingService trading.TradingService, jwtSecret string) *MyOrdersHandler {
	return &MyOrdersHandler{tradingService: tradingService, jwtSecret: jwtSecret}
}

// ServeHTTP handles GET /bank/trading/my-orders.
func (h *MyOrdersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// ── Auth ──────────────────────────────────────────────────────────────────
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	claims, err := auth.VerifyToken(strings.TrimPrefix(authHeader, "Bearer "), h.jwtSecret)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	callerID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "neispravan korisnički ID u tokenu")
		return
	}

	// ── Optional status filter ────────────────────────────────────────────────
	var statusFilter *trading.OrderStatus
	if s := r.URL.Query().Get("status"); s != "" {
		v := trading.OrderStatus(s)
		statusFilter = &v
	}

	// ── Fetch ─────────────────────────────────────────────────────────────────
	orders, err := h.tradingService.ListOrdersByUser(r.Context(), callerID, statusFilter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "greška pri dohvatu naloga")
		return
	}

	// ── Serialize ─────────────────────────────────────────────────────────────
	type orderJSON struct {
		ID                int64      `json:"id"`
		UserID            int64      `json:"userId"`
		AccountID         int64      `json:"accountId"`
		ListingID         int64      `json:"listingId"`
		OrderType         string     `json:"orderType"`
		Direction         string     `json:"direction"`
		Quantity          int32      `json:"quantity"`
		ContractSize      int32      `json:"contractSize"`
		PricePerUnit      *string    `json:"pricePerUnit,omitempty"`
		StopPrice         *string    `json:"stopPrice,omitempty"`
		Status            string     `json:"status"`
		ApprovedBy        *int64     `json:"approvedBy,omitempty"`
		IsDone            bool       `json:"isDone"`
		RemainingPortions int32      `json:"remainingPortions"`
		AfterHours        bool       `json:"afterHours"`
		AllOrNone         bool       `json:"allOrNone"`
		Margin            bool       `json:"margin"`
		LastModified      time.Time  `json:"lastModified"`
		CreatedAt         time.Time  `json:"createdAt"`
	}

	result := make([]orderJSON, 0, len(orders))
	for _, o := range orders {
		item := orderJSON{
			ID:                o.ID,
			UserID:            o.UserID,
			AccountID:         o.AccountID,
			ListingID:         o.ListingID,
			OrderType:         string(o.OrderType),
			Direction:         string(o.Direction),
			Quantity:          o.Quantity,
			ContractSize:      o.ContractSize,
			Status:            string(o.Status),
			IsDone:            o.IsDone,
			RemainingPortions: o.RemainingPortions,
			AfterHours:        o.AfterHours,
			AllOrNone:         o.AllOrNone,
			Margin:            o.Margin,
			LastModified:      o.LastModified,
			CreatedAt:         o.CreatedAt,
		}
		if o.PricePerUnit != nil {
			v := o.PricePerUnit.String()
			item.PricePerUnit = &v
		}
		if o.StopPrice != nil {
			v := o.StopPrice.String()
			item.StopPrice = &v
		}
		if o.ApprovedBy != nil {
			item.ApprovedBy = o.ApprovedBy
		}
		result = append(result, item)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{"orders": result})
}
