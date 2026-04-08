package handler

// tax_handler.go — HTTP handlers for the "Porez Tracking" portal.
//
// Endpoints:
//   GET  /bank/tax/users      — lists all users with current tax debt (supervisor only)
//   POST /bank/tax/calculate  — triggers monthly tax calculation and deduction (supervisor only)
//
// Tax model:
//   - Capital gains tax at 15% on profits from stock sales
//   - Profit = (sell_price - avg_buy_price) × quantity
//   - Tax converted to RSD using exchange rates (no fee, same as exchange system)
//   - Deducted from the same account used for the sell order

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/domain"

	"gorm.io/gorm"
)

const taxRate = 0.15

// TaxHandler serves all /bank/tax/* endpoints.
type TaxHandler struct {
	db              *gorm.DB
	exchangeService domain.ExchangeService
	jwtSecret       string
}

// NewTaxHandler constructs the handler with its dependencies.
func NewTaxHandler(
	db *gorm.DB,
	exchangeService domain.ExchangeService,
	jwtSecret string,
) *TaxHandler {
	return &TaxHandler{
		db:              db,
		exchangeService: exchangeService,
		jwtSecret:       jwtSecret,
	}
}

// ServeHTTP dispatches to sub-handlers.
func (h *TaxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	claims, ok := h.verifySupervisor(w, r)
	if !ok {
		return
	}

	switch {
	case r.URL.Path == "/bank/tax/users" && r.Method == http.MethodGet:
		h.listUsers(w, r, claims)
	case r.URL.Path == "/bank/tax/calculate" && r.Method == http.MethodPost:
		h.calculateAndCollect(w, r, claims)
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

// ─── GET /bank/tax/users ──────────────────────────────────────────────────────

type taxUserRecord struct {
	UserID    string  `json:"userId"`
	UserType  string  `json:"userType"` // "CLIENT" | "ACTUARY"
	TaxDebt   float64 `json:"taxDebt"`   // RSD
}

type taxUsersResponse struct {
	Users []taxUserRecord `json:"users"`
}

func (h *TaxHandler) listUsers(w http.ResponseWriter, r *http.Request, _ *auth.AccessClaims) {
	ctx := r.Context()
	now := time.Now()

	// Tax debt = sum of unpaid tax_records for current year
	type debtRow struct {
		UserID  int64   `gorm:"column:user_id"`
		TaxDebt float64 `gorm:"column:tax_debt"`
	}
	var debts []debtRow
	h.db.WithContext(ctx).Raw(`
		SELECT user_id, SUM(amount_rsd) AS tax_debt
		FROM core_banking.tax_records
		WHERE paid = FALSE AND year = ?
		GROUP BY user_id
	`, now.Year()).Scan(&debts)

	// Also include users who have un-calculated sell orders this month
	// (estimated debt not yet written to tax_records)
	type estimateRow struct {
		UserID      int64   `gorm:"column:user_id"`
		EstimatedRSD float64 `gorm:"column:estimated_rsd"`
	}
	var estimates []estimateRow
	h.db.WithContext(ctx).Raw(`
		WITH sell_fills AS (
			SELECT o.user_id,
			       SUM(ot.executed_quantity * CAST(ot.executed_price AS FLOAT)) AS gross_sell,
			       SUM(ot.executed_quantity)                                    AS sell_qty,
			       o.listing_id
			FROM core_banking.orders o
			JOIN core_banking.order_transactions ot ON ot.order_id = o.id
			JOIN core_banking.listing l ON l.id = o.listing_id AND l.listing_type = 'STOCK'
			WHERE o.direction = 'SELL' AND o.status = 'DONE' AND o.is_done = TRUE
			  AND EXTRACT(YEAR  FROM o.last_modified) = ?
			  AND EXTRACT(MONTH FROM o.last_modified) = ?
			GROUP BY o.user_id, o.listing_id
		),
		buy_avg AS (
			SELECT o.user_id, o.listing_id,
			       CASE
			           WHEN SUM(ot2.executed_quantity) > 0
			           THEN SUM(ot2.executed_quantity * CAST(ot2.executed_price AS FLOAT))
			                / SUM(ot2.executed_quantity)
			           ELSE AVG(CAST(o.price_per_unit AS FLOAT))
			       END AS avg_buy
			FROM core_banking.orders o
			LEFT JOIN core_banking.order_transactions ot2 ON ot2.order_id = o.id
			JOIN core_banking.listing l ON l.id = o.listing_id AND l.listing_type = 'STOCK'
			WHERE o.direction = 'BUY' AND o.status = 'DONE' AND o.is_done = TRUE
			GROUP BY o.user_id, o.listing_id
		)
		SELECT s.user_id,
		       SUM(GREATEST(0, s.gross_sell - COALESCE(b.avg_buy, 0) * s.sell_qty) * ?) AS estimated_rsd
		FROM sell_fills s
		LEFT JOIN buy_avg b ON b.user_id = s.user_id AND b.listing_id = s.listing_id
		GROUP BY s.user_id
		HAVING SUM(GREATEST(0, s.gross_sell - COALESCE(b.avg_buy, 0) * s.sell_qty)) > 0
	`, now.Year(), int(now.Month()), taxRate).Scan(&estimates)

	// Merge: prefer tax_records debt, fall back to estimates
	debtMap := make(map[int64]float64)
	for _, d := range debts {
		debtMap[d.UserID] += d.TaxDebt
	}
	for _, e := range estimates {
		if _, found := debtMap[e.UserID]; !found {
			debtMap[e.UserID] = e.EstimatedRSD
		}
	}

	// Determine user types via actuary_info
	type actuaryIDRow struct {
		EmployeeID int64 `gorm:"column:employee_id"`
	}
	var actuaryIDs []actuaryIDRow
	h.db.WithContext(ctx).Raw(`SELECT employee_id FROM core_banking.actuary_info`).Scan(&actuaryIDs)
	actuarySet := make(map[int64]bool, len(actuaryIDs))
	for _, a := range actuaryIDs {
		actuarySet[a.EmployeeID] = true
	}

	result := make([]taxUserRecord, 0, len(debtMap))
	for uid, debt := range debtMap {
		ut := "CLIENT"
		if actuarySet[uid] {
			ut = "ACTUARY"
		}
		result = append(result, taxUserRecord{
			UserID:   strconv.FormatInt(uid, 10),
			UserType: ut,
			TaxDebt:  debt,
		})
	}

	writeJSON(w, http.StatusOK, taxUsersResponse{Users: result})
}

// ─── POST /bank/tax/calculate ─────────────────────────────────────────────────

type taxCalculateResponse struct {
	ProcessedUsers int     `json:"processedUsers"`
	TotalCollected float64 `json:"totalCollectedRsd"`
	Message        string  `json:"message"`
}

func (h *TaxHandler) calculateAndCollect(w http.ResponseWriter, r *http.Request, _ *auth.AccessClaims) {
	ctx := r.Context()
	now := time.Now()
	year := now.Year()
	month := int(now.Month())

	// Fetch exchange rates for USD→RSD conversion
	rates, err := h.exchangeService.GetRates(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "nije moguće dohvatiti kurseve")
		return
	}
	usdToRSD := h.usdToRSD(rates)
	if usdToRSD <= 0 {
		writeJSONError(w, http.StatusInternalServerError, "USD kurs nije dostupan")
		return
	}

	// ── Compute capital gains per (user, account) for this month ──────────────
	type taxRow struct {
		UserID    int64   `gorm:"column:user_id"`
		AccountID int64   `gorm:"column:account_id"`
		TaxUSD    float64 `gorm:"column:tax_usd"`
	}
	var taxRows []taxRow
	h.db.WithContext(ctx).Raw(`
		WITH sell_fills AS (
			SELECT o.user_id, o.account_id, o.listing_id,
			       SUM(ot.executed_quantity * CAST(ot.executed_price AS FLOAT)) AS gross_sell,
			       SUM(ot.executed_quantity)                                    AS sell_qty
			FROM core_banking.orders o
			JOIN core_banking.order_transactions ot ON ot.order_id = o.id
			JOIN core_banking.listing l ON l.id = o.listing_id AND l.listing_type = 'STOCK'
			WHERE o.direction = 'SELL' AND o.status = 'DONE' AND o.is_done = TRUE
			  AND EXTRACT(YEAR  FROM o.last_modified) = ?
			  AND EXTRACT(MONTH FROM o.last_modified) = ?
			GROUP BY o.user_id, o.account_id, o.listing_id
		),
		buy_avg AS (
			SELECT o.user_id, o.listing_id,
			       CASE
			           WHEN SUM(ot2.executed_quantity) > 0
			           THEN SUM(ot2.executed_quantity * CAST(ot2.executed_price AS FLOAT))
			                / SUM(ot2.executed_quantity)
			           ELSE AVG(CAST(o.price_per_unit AS FLOAT))
			       END AS avg_buy
			FROM core_banking.orders o
			LEFT JOIN core_banking.order_transactions ot2 ON ot2.order_id = o.id
			JOIN core_banking.listing l ON l.id = o.listing_id AND l.listing_type = 'STOCK'
			WHERE o.direction = 'BUY' AND o.status = 'DONE' AND o.is_done = TRUE
			GROUP BY o.user_id, o.listing_id
		)
		SELECT s.user_id, s.account_id,
		       SUM(GREATEST(0, s.gross_sell - COALESCE(b.avg_buy, 0) * s.sell_qty) * ?) AS tax_usd
		FROM sell_fills s
		LEFT JOIN buy_avg b ON b.user_id = s.user_id AND b.listing_id = s.listing_id
		GROUP BY s.user_id, s.account_id
		HAVING SUM(GREATEST(0, s.gross_sell - COALESCE(b.avg_buy, 0) * s.sell_qty)) > 0
	`, year, month, taxRate).Scan(&taxRows)

	processedUsers := 0
	var totalCollected float64

	for _, row := range taxRows {
		taxRSD := row.TaxUSD * usdToRSD
		if taxRSD <= 0 {
			continue
		}

		// Get account currency to handle non-RSD accounts correctly
		var accCurrency string
		h.db.WithContext(ctx).Raw(`
			SELECT v.oznaka FROM core_banking.racun r
			JOIN core_banking.valuta v ON v.id = r.valuta_id
			WHERE r.id = ?
		`, row.AccountID).Scan(&accCurrency)

		// Determine deduction amount in account currency
		deductionInAccCurrency := taxRSD
		if accCurrency != "" && accCurrency != "RSD" {
			// Convert RSD tax to account currency (no fee)
			deductionInAccCurrency = h.rsdToAccountCurrency(rates, taxRSD, accCurrency)
		}

		if deductionInAccCurrency <= 0 {
			continue
		}

		// Deduct tax from account
		result := h.db.WithContext(ctx).Exec(`
			UPDATE core_banking.racun
			SET stanje_racuna = GREATEST(0, stanje_racuna - ?)
			WHERE id = ?
		`, deductionInAccCurrency, row.AccountID)
		if result.Error != nil {
			continue
		}

		// Record transaction
		h.db.WithContext(ctx).Exec(`
			INSERT INTO core_banking.transakcija (racun_id, tip_transakcije, iznos, opis, vreme_izvrsavanja, status)
			VALUES (?, 'ISPLATA', ?, 'Porez na kapitalnu dobit', NOW(), 'IZVRSEN')
		`, row.AccountID, deductionInAccCurrency)

		// Upsert tax_record (mark as paid)
		h.db.WithContext(ctx).Exec(`
			INSERT INTO core_banking.tax_records (user_id, account_id, year, month, amount_rsd, paid, paid_at)
			VALUES (?, ?, ?, ?, ?, TRUE, NOW())
			ON CONFLICT (user_id, account_id, year, month)
			DO UPDATE SET amount_rsd = EXCLUDED.amount_rsd, paid = TRUE, paid_at = NOW()
		`, row.UserID, row.AccountID, year, month, taxRSD)

		processedUsers++
		totalCollected += taxRSD
	}

	writeJSON(w, http.StatusOK, taxCalculateResponse{
		ProcessedUsers: processedUsers,
		TotalCollected: totalCollected,
		Message:        "Obračun poreza je uspešno izvršen.",
	})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func (h *TaxHandler) verifySupervisor(w http.ResponseWriter, r *http.Request) (*auth.AccessClaims, bool) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	claims, err := auth.VerifyToken(strings.TrimPrefix(authHeader, "Bearer "), h.jwtSecret)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	// Only EMPLOYEE (supervisor) or ADMIN can access tax portal
	if claims.UserType != "EMPLOYEE" && claims.UserType != "ADMIN" {
		writeJSONError(w, http.StatusForbidden, "pristup dozvoljen samo supervizorima")
		return nil, false
	}
	// Check SUPERVISOR permission for non-admin employees
	if claims.UserType == "EMPLOYEE" {
		isSupervisor := false
		for _, p := range claims.Permissions {
			if p == "SUPERVISOR" {
				isSupervisor = true
				break
			}
		}
		if !isSupervisor {
			writeJSONError(w, http.StatusForbidden, "pristup dozvoljen samo supervizorima")
			return nil, false
		}
	}
	return claims, true
}

// usdToRSD returns the USD→RSD mid-rate from the exchange rates list.
// Trading prices are in USD. Tax must be paid in RSD.
// No fee is applied (per spec: "same logic as exchange system but without fee").
func (h *TaxHandler) usdToRSD(rates []domain.ExchangeRate) float64 {
	for _, r := range rates {
		if r.Oznaka == "USD" {
			return r.Srednji // mid-rate, no fee
		}
	}
	return 0
}

// rsdToAccountCurrency converts an RSD amount to the account's currency using
// the mid-rate (no fee per spec).
func (h *TaxHandler) rsdToAccountCurrency(rates []domain.ExchangeRate, rsdAmount float64, toCurrency string) float64 {
	if toCurrency == "RSD" {
		return rsdAmount
	}
	for _, r := range rates {
		if r.Oznaka == toCurrency && r.Srednji > 0 {
			return rsdAmount / r.Srednji
		}
	}
	// If currency not found, return full RSD amount (safest fallback)
	return rsdAmount
}

