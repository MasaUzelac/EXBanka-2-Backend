package handler

// fund_handler.go — HTTP handlers for investment funds (Moji fondovi).
//
// Endpoints:
//   GET  /bank/funds                       — list funds (clients: own positions; supervisors: managed funds)
//   POST /bank/funds/{id}/invest           — invest in a fund
//   POST /bank/funds/{id}/withdraw         — withdraw from a fund

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/domain"

	"gorm.io/gorm"
)

// ─── GORM models ──────────────────────────────────────────────────────────────

type investmentFundModel struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	Name        string    `gorm:"column:name"`
	Description string    `gorm:"column:description"`
	ManagerID   int64     `gorm:"column:manager_id"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (investmentFundModel) TableName() string { return "core_banking.investment_funds" }

type fundPositionModel struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	FundID      int64     `gorm:"column:fund_id"`
	UserID      int64     `gorm:"column:user_id"`
	AccountID   int64     `gorm:"column:account_id"`
	InvestedRSD float64   `gorm:"column:invested_rsd"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (fundPositionModel) TableName() string { return "core_banking.fund_positions" }

// ─── FundHandler ──────────────────────────────────────────────────────────────

// FundHandler serves all /bank/funds/* endpoints.
type FundHandler struct {
	db              *gorm.DB
	exchangeService domain.ExchangeService
	jwtSecret       string
}

// NewFundHandler constructs the handler.
func NewFundHandler(
	db *gorm.DB,
	exchangeService domain.ExchangeService,
	jwtSecret string,
) *FundHandler {
	return &FundHandler{db: db, exchangeService: exchangeService, jwtSecret: jwtSecret}
}

// ServeHTTP dispatches /bank/funds/* requests.
func (h *FundHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

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

	path := r.URL.Path // e.g. /bank/funds or /bank/funds/3/invest

	switch {
	case path == "/bank/funds" && r.Method == http.MethodGet:
		h.listFunds(w, r, claims)
	case strings.HasSuffix(path, "/invest") && r.Method == http.MethodPost:
		h.invest(w, r, claims, extractFundID(path, "/invest"))
	case strings.HasSuffix(path, "/withdraw") && r.Method == http.MethodPost:
		h.withdraw(w, r, claims, extractFundID(path, "/withdraw"))
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

// extractFundID parses the fund ID from a path like /bank/funds/3/invest.
func extractFundID(path, suffix string) int64 {
	trimmed := strings.TrimSuffix(path, suffix)               // /bank/funds/3
	parts := strings.Split(trimmed, "/")                      // ["", "bank", "funds", "3"]
	if len(parts) == 0 {
		return 0
	}
	id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	return id
}

// ─── GET /bank/funds ──────────────────────────────────────────────────────────

type fundForClient struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	FundValueRSD float64 `json:"fundValueRsd"`
	SharePercent float64 `json:"sharePercent"`
	ShareRSD     float64 `json:"shareRsd"`
	Profit       float64 `json:"profit"`
	InvestedRSD  float64 `json:"investedRsd"`
}

type fundForManager struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	FundValueRSD float64 `json:"fundValueRsd"`
	LiquidityRSD float64 `json:"liquidityRsd"`
}

type fundsResponse struct {
	ClientFunds  []fundForClient  `json:"clientFunds,omitempty"`
	ManagedFunds []fundForManager `json:"managedFunds,omitempty"`
}

func (h *FundHandler) listFunds(w http.ResponseWriter, r *http.Request, claims *auth.AccessClaims) {
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "invalid user id")
		return
	}

	ctx := r.Context()

	isSupervisor := h.isSupervisor(claims)

	if isSupervisor {
		// Return funds managed by this supervisor
		var funds []investmentFundModel
		h.db.WithContext(ctx).Where("manager_id = ?", userID).Find(&funds)

		managedFunds := make([]fundForManager, 0, len(funds))
		for _, f := range funds {
			// Fund value = sum of all invested amounts
			var totalInvested float64
			h.db.WithContext(ctx).Raw(`
				SELECT COALESCE(SUM(invested_rsd), 0)
				FROM core_banking.fund_positions WHERE fund_id = ?
			`, f.ID).Scan(&totalInvested)

			// Liquidity = total invested (simplified: no separate cash tracking)
			managedFunds = append(managedFunds, fundForManager{
				ID:           strconv.FormatInt(f.ID, 10),
				Name:         f.Name,
				Description:  f.Description,
				FundValueRSD: totalInvested,
				LiquidityRSD: totalInvested,
			})
		}
		writeJSON(w, http.StatusOK, fundsResponse{ManagedFunds: managedFunds})
		return
	}

	// Client / Actuary Agent: show their positions
	var positions []fundPositionModel
	h.db.WithContext(ctx).Where("user_id = ?", userID).Find(&positions)

	if len(positions) == 0 {
		writeJSON(w, http.StatusOK, fundsResponse{ClientFunds: []fundForClient{}})
		return
	}

	clientFunds := make([]fundForClient, 0, len(positions))
	for _, pos := range positions {
		var fund investmentFundModel
		if err := h.db.WithContext(ctx).First(&fund, pos.FundID).Error; err != nil {
			continue
		}

		var totalFundValue float64
		h.db.WithContext(ctx).Raw(`
			SELECT COALESCE(SUM(invested_rsd), 0) FROM core_banking.fund_positions WHERE fund_id = ?
		`, pos.FundID).Scan(&totalFundValue)

		sharePercent := 0.0
		if totalFundValue > 0 {
			sharePercent = (pos.InvestedRSD / totalFundValue) * 100
		}
		shareRSD := pos.InvestedRSD // simplified: current share equals invested (no growth modeled)

		clientFunds = append(clientFunds, fundForClient{
			ID:           strconv.FormatInt(fund.ID, 10),
			Name:         fund.Name,
			Description:  fund.Description,
			FundValueRSD: totalFundValue,
			SharePercent: sharePercent,
			ShareRSD:     shareRSD,
			Profit:       0, // simplified: growth not modeled in this iteration
			InvestedRSD:  pos.InvestedRSD,
		})
	}
	writeJSON(w, http.StatusOK, fundsResponse{ClientFunds: clientFunds})
}

// ─── POST /bank/funds/{id}/invest ────────────────────────────────────────────

type investRequest struct {
	AccountID string  `json:"accountId"`
	Amount    float64 `json:"amount"` // in account currency
}

func (h *FundHandler) invest(w http.ResponseWriter, r *http.Request, claims *auth.AccessClaims, fundID int64) {
	if fundID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid fund id")
		return
	}

	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "invalid user id")
		return
	}

	var req investRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount <= 0 {
		writeJSONError(w, http.StatusBadRequest, "amount must be greater than 0")
		return
	}
	accountID, err := strconv.ParseInt(req.AccountID, 10, 64)
	if err != nil || accountID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid accountId")
		return
	}

	ctx := r.Context()

	// Verify fund exists
	var fund investmentFundModel
	if err := h.db.WithContext(ctx).First(&fund, fundID).Error; err != nil {
		writeJSONError(w, http.StatusNotFound, "fond nije pronađen")
		return
	}

	// Get account currency
	var accCurrency string
	h.db.WithContext(ctx).Raw(`
		SELECT v.oznaka FROM core_banking.racun r
		JOIN core_banking.valuta v ON v.id = r.valuta_id WHERE r.id = ?
	`, accountID).Scan(&accCurrency)

	// Convert to RSD
	amountRSD := req.Amount
	if accCurrency != "" && accCurrency != "RSD" {
		rates, err := h.exchangeService.GetRates(ctx)
		if err == nil {
			for _, rt := range rates {
				if rt.Oznaka == accCurrency && rt.Srednji > 0 {
					amountRSD = req.Amount * rt.Srednji
					break
				}
			}
		}
	}

	// Check sufficient funds
	var available float64
	h.db.WithContext(ctx).Raw(`
		SELECT stanje_racuna - rezervisana_sredstva FROM core_banking.racun WHERE id = ?
	`, accountID).Scan(&available)
	if available < req.Amount {
		writeJSONError(w, http.StatusBadRequest, "nedovoljno sredstava na računu")
		return
	}

	// Deduct from account
	h.db.WithContext(ctx).Exec(`
		UPDATE core_banking.racun SET stanje_racuna = stanje_racuna - ? WHERE id = ?
	`, req.Amount, accountID)

	// Upsert fund position
	h.db.WithContext(ctx).Exec(`
		INSERT INTO core_banking.fund_positions (fund_id, user_id, account_id, invested_rsd)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (fund_id, user_id)
		DO UPDATE SET invested_rsd = fund_positions.invested_rsd + EXCLUDED.invested_rsd
	`, fundID, userID, accountID, amountRSD)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":     "Investicija je uspešno obavljena.",
		"amountRsd":   amountRSD,
		"fundId":      strconv.FormatInt(fundID, 10),
	})
}

// ─── POST /bank/funds/{id}/withdraw ──────────────────────────────────────────

type withdrawRequest struct {
	AccountID   string  `json:"accountId"`
	AmountRSD   float64 `json:"amountRsd"`   // amount to withdraw in RSD (0 = full withdrawal)
	WithdrawAll bool    `json:"withdrawAll"`
}

func (h *FundHandler) withdraw(w http.ResponseWriter, r *http.Request, claims *auth.AccessClaims, fundID int64) {
	if fundID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid fund id")
		return
	}

	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "invalid user id")
		return
	}

	var req withdrawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	accountID, err := strconv.ParseInt(req.AccountID, 10, 64)
	if err != nil || accountID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid accountId")
		return
	}

	ctx := r.Context()

	// Load user's position in this fund
	var pos fundPositionModel
	if err := h.db.WithContext(ctx).
		Where("fund_id = ? AND user_id = ?", fundID, userID).
		First(&pos).Error; err != nil {
		writeJSONError(w, http.StatusNotFound, "nemate poziciju u ovom fondu")
		return
	}

	withdrawRSD := req.AmountRSD
	if req.WithdrawAll || withdrawRSD <= 0 {
		withdrawRSD = pos.InvestedRSD
	}
	if withdrawRSD > pos.InvestedRSD {
		writeJSONError(w, http.StatusBadRequest, "iznos povlačenja veći od investiranog iznosa")
		return
	}

	// Get account currency for crediting
	var accCurrency string
	h.db.WithContext(ctx).Raw(`
		SELECT v.oznaka FROM core_banking.racun r
		JOIN core_banking.valuta v ON v.id = r.valuta_id WHERE r.id = ?
	`, accountID).Scan(&accCurrency)

	// Convert withdraw amount (RSD) to account currency
	creditAmount := withdrawRSD
	if accCurrency != "" && accCurrency != "RSD" {
		rates, err := h.exchangeService.GetRates(ctx)
		if err == nil {
			for _, rt := range rates {
				if rt.Oznaka == accCurrency && rt.Srednji > 0 {
					creditAmount = withdrawRSD / rt.Srednji
					break
				}
			}
		}
	}

	// Credit account
	h.db.WithContext(ctx).Exec(`
		UPDATE core_banking.racun SET stanje_racuna = stanje_racuna + ? WHERE id = ?
	`, creditAmount, accountID)

	// Update or delete fund position
	remaining := pos.InvestedRSD - withdrawRSD
	if remaining <= 0 {
		h.db.WithContext(ctx).Delete(&pos)
	} else {
		h.db.WithContext(ctx).Model(&pos).Update("invested_rsd", remaining)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":       "Povlačenje je uspešno obavljeno.",
		"withdrawnRsd":  withdrawRSD,
		"creditedAmount": creditAmount,
		"currency":      accCurrency,
	})
}

// isSupervisor checks if the user is a supervisor (has SUPERVISOR permission).
func (h *FundHandler) isSupervisor(claims *auth.AccessClaims) bool {
	if claims.UserType == "ADMIN" {
		return true
	}
	for _, p := range claims.Permissions {
		if p == "SUPERVISOR" {
			return true
		}
	}
	return false
}
