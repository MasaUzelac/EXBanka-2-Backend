package trading

// =============================================================================
// service.go — TradingService implementation.
//
// tradingService is intentionally thin: it orchestrates four injected
// dependencies and delegates all pure arithmetic to calculations.go.
// No SQL, no HTTP, no gRPC in this file.
//
// Dependency graph (all interfaces — no concrete types):
//
//   tradingService
//     ├── OrderRepository           (internal/repository/order_repository.go)
//     ├── domain.ListingService     (internal/service/listing_service.go)
//     │     └── used for Ask/Bid (price resolution) AND MaintenanceMargin
//     ├── domain.ActuaryRepository  (internal/repository/actuary_repository.go)
//     │     └── used for approval workflow AND supervisor identity checks
//     └── MarginChecker             (internal/repository/margin_checker.go — future)
//
// Wire-up happens in cmd/server/main.go; this file has zero knowledge of
// how the dependencies are constructed or which DB is behind them.
// =============================================================================

import (
	"context"
	"errors"
	"fmt"
	"log"

	"banka-backend/services/bank-service/internal/domain"

	"github.com/shopspring/decimal"
)

// ─── Constructor ──────────────────────────────────────────────────────────────

type tradingService struct {
	orders    OrderRepository
	listings  domain.ListingService  // ListingService (not Repository) — provides MaintenanceMargin
	actuaries domain.ActuaryRepository
	margin    MarginChecker
	funds     FundsManager
}

// NewTradingService wires the service with its five dependencies.
// All parameters are interfaces; concrete implementations are provided by the
// caller (main.go) at startup.
func NewTradingService(
	orders OrderRepository,
	listings domain.ListingService,
	actuaries domain.ActuaryRepository,
	margin MarginChecker,
	funds FundsManager,
) TradingService {
	return &tradingService{
		orders:    orders,
		listings:  listings,
		actuaries: actuaries,
		margin:    margin,
		funds:     funds,
	}
}

// ─── CalculateOrderDetails ────────────────────────────────────────────────────

// CalculateOrderDetails resolves the effective price per unit for the given
// order type, computes the approximate notional and commission, and — when
// Margin=true — also computes and returns the initial margin cost.
//
// Price-per-unit resolution per order type:
//   MARKET     → current Ask (BUY) or Bid (SELL) from live listing data
//   LIMIT      → user-supplied PricePerUnit (limit value)
//   STOP       → user-supplied StopPrice   (stop trigger value)
//   STOP_LIMIT → user-supplied PricePerUnit (limit value; stop is only the trigger)
func (s *tradingService) CalculateOrderDetails(ctx context.Context, req *OrderCalculationRequest) (*OrderCalculationResponse, error) {
	if req.Direction != OrderDirectionBuy && req.Direction != OrderDirectionSell {
		return nil, ErrInvalidDirection
	}

	var resp *OrderCalculationResponse
	var err error

	switch req.OrderType {
	case OrderTypeMarket:
		resp, err = s.calculateMarket(ctx, req)
	case OrderTypeLimit:
		resp, err = s.calculateLimit(req)
	case OrderTypeStop:
		resp, err = s.calculateStop(req)
	case OrderTypeStopLimit:
		resp, err = s.calculateStopLimit(req)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidOrderType, req.OrderType)
	}

	if err != nil {
		return nil, err
	}

	// Append the initial margin cost when the user intends to use margin
	// financing. We fetch the listing here if it wasn't already fetched by
	// the order-type branch (LIMIT / STOP / STOP_LIMIT paths).
	if req.Margin {
		imc, err := s.initialMarginCostForListing(ctx, req.ListingID)
		if err != nil {
			return nil, err
		}
		resp.InitialMarginCost = &imc
	}

	return resp, nil
}

// calculateMarket handles the MARKET branch of CalculateOrderDetails.
// Fetches live Ask / Bid from the listing service.
func (s *tradingService) calculateMarket(ctx context.Context, req *OrderCalculationRequest) (*OrderCalculationResponse, error) {
	listing, err := s.listings.GetListingByID(ctx, req.ListingID)
	if err != nil {
		return nil, fmt.Errorf("dohvatanje hartije %d za kalkulaciju: %w", req.ListingID, err)
	}

	// Ask/Bid arrive as float64 from the GORM-backed listing repository.
	// decimal.NewFromFloat is acceptable here: the source values are already
	// rounded to NUMERIC(18,6) precision in the database.
	var pricePerUnit decimal.Decimal
	if req.Direction == OrderDirectionBuy {
		pricePerUnit = decimal.NewFromFloat(listing.Ask)
	} else {
		pricePerUnit = decimal.NewFromFloat(listing.Bid)
	}

	notional, commission := calcMarketOrder(req.ContractSize, pricePerUnit, req.Quantity)
	return &OrderCalculationResponse{
		PricePerUnit:     pricePerUnit,
		ApproximatePrice: notional,
		Commission:       commission,
	}, nil
}

// calculateLimit handles the LIMIT branch of CalculateOrderDetails.
// No listing fetch required — the user supplies the price directly.
func (s *tradingService) calculateLimit(req *OrderCalculationRequest) (*OrderCalculationResponse, error) {
	if req.PricePerUnit == nil {
		return nil, ErrLimitPriceRequired
	}

	notional, commission := calcLimitOrder(req.ContractSize, *req.PricePerUnit, req.Quantity)
	return &OrderCalculationResponse{
		PricePerUnit:     *req.PricePerUnit,
		ApproximatePrice: notional,
		Commission:       commission,
	}, nil
}

// calculateStop handles the STOP branch of CalculateOrderDetails.
// The approximate price preview uses the stop trigger value (StopPrice), not
// the current market price, because the execution price is unknown until
// triggered.  Commission follows the MARKET schedule.
func (s *tradingService) calculateStop(req *OrderCalculationRequest) (*OrderCalculationResponse, error) {
	if req.StopPrice == nil {
		return nil, ErrStopPriceRequired
	}

	notional, commission := calcStopOrder(req.ContractSize, *req.StopPrice, req.Quantity)
	return &OrderCalculationResponse{
		PricePerUnit:     *req.StopPrice,
		ApproximatePrice: notional,
		Commission:       commission,
	}, nil
}

// calculateStopLimit handles the STOP_LIMIT branch of CalculateOrderDetails.
// The approximate price uses the limit value (PricePerUnit) because that is
// the worst-case fill price once the order is activated.  The stop trigger
// (StopPrice) is validated but not used in the notional arithmetic.
// Commission follows the LIMIT schedule.
func (s *tradingService) calculateStopLimit(req *OrderCalculationRequest) (*OrderCalculationResponse, error) {
	if req.StopPrice == nil {
		return nil, ErrStopPriceRequired
	}
	if req.PricePerUnit == nil {
		return nil, ErrLimitPriceRequired
	}

	notional, commission := calcStopLimitOrder(req.ContractSize, *req.PricePerUnit, req.Quantity)
	return &OrderCalculationResponse{
		PricePerUnit:     *req.PricePerUnit,
		ApproximatePrice: notional,
		Commission:       commission,
	}, nil
}

// initialMarginCostForListing fetches the listing's MaintenanceMargin and
// derives the initial margin cost (MaintenanceMargin × 1.1).
// Called from CalculateOrderDetails and validateMargin whenever Margin=true.
func (s *tradingService) initialMarginCostForListing(ctx context.Context, listingID int64) (decimal.Decimal, error) {
	listing, err := s.listings.GetListingByID(ctx, listingID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("dohvatanje hartije %d za margin kalkulaciju: %w", listingID, err)
	}
	maintenanceMargin := decimal.NewFromFloat(listing.MaintenanceMargin)
	return CalcInitialMarginCost(maintenanceMargin), nil
}

// ─── CreateOrder ──────────────────────────────────────────────────────────────

// CreateOrder validates inputs, checks margin funds when required, applies
// the approval workflow, then persists the order via OrderRepository.
func (s *tradingService) CreateOrder(ctx context.Context, req *CreateOrderRequest) (*Order, error) {
	// ── 1. Input validation ───────────────────────────────────────────────────

	if req.Direction != OrderDirectionBuy && req.Direction != OrderDirectionSell {
		return nil, ErrInvalidDirection
	}

	if err := validateOrderTypeFields(req); err != nil {
		return nil, err
	}

	// ── 2. Margin check (before touching actuary limit) ───────────────────────
	//
	// Rejected margin orders must never be counted against an agent's used_limit,
	// so we validate funds first, before resolveStatus runs.

	if req.Margin {
		if err := s.validateMargin(ctx, req); err != nil {
			return nil, err
		}
	}

	// ── 3. Approval workflow ──────────────────────────────────────────────────

	status, err := s.resolveStatus(ctx, req)
	if err != nil {
		return nil, err
	}

	// ── 4. Persist ────────────────────────────────────────────────────────────

	order, err := s.orders.Create(ctx, *req, status)
	if err != nil {
		return nil, fmt.Errorf("kreiranje naloga: %w", err)
	}

	// ── 5. Reserve funds for APPROVED BUY orders ──────────────────────────────
	// PENDING orders are not reserved yet — funds are reserved when the
	// supervisor approves them (see ApproveOrder).
	if status == OrderStatusApproved && req.Direction == OrderDirectionBuy {
		if err := s.reserveFundsForOrder(ctx, order); err != nil {
			// Order is persisted but reservation failed — cancel it to keep DB consistent.
			if _, cancelErr := s.orders.Cancel(ctx, order.ID); cancelErr != nil {
				log.Printf("[trading] auto-otkazivanje naloga %d nakon greške rezervacije: %v", order.ID, cancelErr)
			}
			return nil, fmt.Errorf("rezervacija sredstava: %w", err)
		}
	}

	return order, nil
}

// validateOrderTypeFields enforces the price-field preconditions for each
// order type.  All branches must be reached before any I/O is performed.
//
//	MARKET     → no price fields required
//	LIMIT      → PricePerUnit required
//	STOP       → StopPrice required
//	STOP_LIMIT → both StopPrice and PricePerUnit required
func validateOrderTypeFields(req *CreateOrderRequest) error {
	switch req.OrderType {
	case OrderTypeMarket:
		// No price fields are required; Ask/Bid are resolved at execution time.
		return nil

	case OrderTypeLimit:
		if req.PricePerUnit == nil {
			return ErrLimitPriceRequired
		}
		return nil

	case OrderTypeStop:
		if req.StopPrice == nil {
			return ErrStopPriceRequired
		}
		return nil

	case OrderTypeStopLimit:
		if req.StopPrice == nil {
			return ErrStopPriceRequired
		}
		if req.PricePerUnit == nil {
			return ErrLimitPriceRequired
		}
		return nil

	default:
		return fmt.Errorf("%w: %q", ErrInvalidOrderType, req.OrderType)
	}
}

// validateMargin fetches the listing's InitialMarginCost and asks the
// MarginChecker whether the account holds sufficient free balance.
// Returns ErrInsufficientMargin when funds are insufficient.
func (s *tradingService) validateMargin(ctx context.Context, req *CreateOrderRequest) error {
	imc, err := s.initialMarginCostForListing(ctx, req.ListingID)
	if err != nil {
		return err
	}

	ok, err := s.margin.HasSufficientMargin(ctx, req.AccountID, imc)
	if err != nil {
		return fmt.Errorf("provjera margin sredstava za račun %d: %w", req.AccountID, err)
	}
	if !ok {
		return ErrInsufficientMargin
	}
	return nil
}

// ─── Approval workflow ────────────────────────────────────────────────────────

// resolveStatus determines the initial OrderStatus according to the caller's role:
//
//	Client (not in actuary_info)  → APPROVED immediately
//	Supervisor                     → APPROVED immediately
//	Agent:
//	    need_approval == true                   → PENDING
//	    used_limit + approx_price > daily_limit → PENDING
//	    otherwise                               → APPROVED; used_limit incremented
//
// NOTE: The used_limit increment and the subsequent orders.Create are NOT
// wrapped in a single DB transaction.  Under concurrent load, two requests for
// the same agent could both pass the limit check simultaneously (TOCTOU race).
// A SELECT FOR UPDATE or an atomic counter must be introduced before this path
// goes to production.
func (s *tradingService) resolveStatus(ctx context.Context, req *CreateOrderRequest) (OrderStatus, error) {
	// JWT permissions are the authoritative source: if the caller holds the
	// SUPERVISOR role (or is an ADMIN), auto-approve regardless of the DB state.
	// This prevents stale actuary_info records from blocking legitimate supervisors.
	if req.IsSupervisor {
		return OrderStatusApproved, nil
	}

	actuary, err := s.actuaries.GetByEmployeeID(ctx, req.UserID)

	if errors.Is(err, domain.ErrActuaryNotFound) {
		// No actuary_info row → regular Client → auto-approve.
		return OrderStatusApproved, nil
	}
	if err != nil {
		return "", fmt.Errorf("provjera aktuar zapisa za korisnika %d: %w", req.UserID, err)
	}

	if actuary.ActuaryType == domain.ActuaryTypeSupervisor {
		return OrderStatusApproved, nil
	}

	// ── Agent path ────────────────────────────────────────────────────────────

	// need_approval flag takes full precedence: skip the (potentially expensive)
	// market-price fetch entirely when the supervisor has already flagged the agent.
	if actuary.NeedApproval {
		return OrderStatusPending, nil
	}

	notional, err := s.resolveNotional(ctx, req)
	if err != nil {
		return "", err
	}

	// Atomično povećavamo used_limit ako je iznos u okviru dnevnog limita.
	// Jedan UPDATE iskaz eliminuje TOCTOU race condition.
	_, err = s.actuaries.IncrementUsedLimitIfWithin(ctx, req.UserID, notional)
	if errors.Is(err, domain.ErrActuaryLimitExceeded) {
		// Iznos bi premašio limit → order ostaje PENDING (čeka supervisora).
		return OrderStatusPending, nil
	}
	if errors.Is(err, domain.ErrActuaryNotFound) {
		// Aktuar je obrisan između prve provjere i ove operacije — tretiramo kao klijenta.
		return OrderStatusApproved, nil
	}
	if err != nil {
		return "", fmt.Errorf("ažuriranje used_limit za agenta (employee_id=%d): %w", req.UserID, err)
	}

	return OrderStatusApproved, nil
}

// resolveNotional returns the approximate order value used for the agent's
// daily-limit check.  The logic is intentionally consistent with the price
// resolution in CalculateOrderDetails so that the preview and the actual check
// use the same number.
//
//	MARKET     → live Ask/Bid × ContractSize × Quantity
//	LIMIT      → PricePerUnit × ContractSize × Quantity
//	STOP       → StopPrice    × ContractSize × Quantity
//	STOP_LIMIT → PricePerUnit × ContractSize × Quantity  (same as LIMIT)
func (s *tradingService) resolveNotional(ctx context.Context, req *CreateOrderRequest) (decimal.Decimal, error) {
	switch req.OrderType {
	case OrderTypeMarket:
		listing, err := s.listings.GetListingByID(ctx, req.ListingID)
		if err != nil {
			return decimal.Zero, fmt.Errorf("dohvatanje hartije %d za provjeru limita: %w", req.ListingID, err)
		}
		var pricePerUnit decimal.Decimal
		if req.Direction == OrderDirectionBuy {
			pricePerUnit = decimal.NewFromFloat(listing.Ask)
		} else {
			pricePerUnit = decimal.NewFromFloat(listing.Bid)
		}
		return approxPrice(req.ContractSize, pricePerUnit, req.Quantity), nil

	case OrderTypeLimit, OrderTypeStopLimit:
		// PricePerUnit is guaranteed non-nil — validateOrderTypeFields ran first.
		return approxPrice(req.ContractSize, *req.PricePerUnit, req.Quantity), nil

	case OrderTypeStop:
		// StopPrice is guaranteed non-nil — validateOrderTypeFields ran first.
		return approxPrice(req.ContractSize, *req.StopPrice, req.Quantity), nil

	default:
		// Unreachable: validateOrderTypeFields already rejected unknown types.
		return decimal.Zero, fmt.Errorf("%w: %s", ErrInvalidOrderType, req.OrderType)
	}
}

// ─── Supervisor dashboard ─────────────────────────────────────────────────────

// ListOrders returns all orders, optionally filtered by status.
// Delegates directly to the repository; no additional business logic needed.
func (s *tradingService) ListOrders(ctx context.Context, statusFilter *OrderStatus) ([]Order, error) {
	orders, err := s.orders.ListByStatus(ctx, statusFilter)
	if err != nil {
		return nil, fmt.Errorf("listanje naloga: %w", err)
	}
	return orders, nil
}

// ListOrdersByUser returns orders belonging to a single user, newest first.
func (s *tradingService) ListOrdersByUser(ctx context.Context, userID int64, statusFilter *OrderStatus) ([]Order, error) {
	orders, err := s.orders.ListByUserID(ctx, userID, statusFilter)
	if err != nil {
		return nil, fmt.Errorf("listanje naloga korisnika: %w", err)
	}
	return orders, nil
}

// ApproveOrder transitions a PENDING order to APPROVED and records the
// supervisor's ID.  When the order belongs to an AGENT, their used_limit is
// incremented by the order's notional value (spec §4: "Used Limit se menja
// pri svakoj transakciji").
func (s *tradingService) ApproveOrder(ctx context.Context, orderID int64, supervisorID int64) (*Order, error) {
	order, err := s.orders.GetByID(ctx, orderID)
	if err != nil {
		return nil, err // ErrOrderNotFound already set by repository
	}

	if order.Status != OrderStatusPending {
		return nil, fmt.Errorf("%w: nalog je u statusu %s, a ne PENDING", ErrInvalidOrderState, order.Status)
	}

	updated, err := s.orders.UpdateStatus(ctx, orderID, OrderStatusApproved, &supervisorID)
	if err != nil {
		return nil, fmt.Errorf("odobravanje naloga %d: %w", orderID, err)
	}

	// Increment agent's used_limit when a supervisor manually approves a PENDING order.
	// We use IncrementUsedLimitIfWithin which is atomic; if the agent's limit is now
	// exceeded (edge case: limit was lowered after order was created) we still approve
	// the order but log the overflow — the supervisor made the conscious decision.
	notional, notionalErr := s.computeNotional(ctx, order, order.Quantity)
	if notionalErr == nil {
		if _, limErr := s.actuaries.IncrementUsedLimitIfWithin(ctx, order.UserID, notional); limErr != nil {
			if !errors.Is(limErr, domain.ErrActuaryLimitExceeded) && !errors.Is(limErr, domain.ErrActuaryNotFound) {
				log.Printf("[trading] ažuriranje used_limit za agenta (order %d, user %d): %v", orderID, order.UserID, limErr)
			}
			// ErrActuaryLimitExceeded → supervisor explicitly approved over limit — allow it.
			// ErrActuaryNotFound → order belongs to a client (no actuary_info) — nothing to update.
		}
	} else {
		log.Printf("[trading] izračunavanje notional-a za used_limit update (order %d): %v", orderID, notionalErr)
	}

	// Reserve funds now that the order transitions from PENDING to APPROVED.
	// updated.Quantity is the full original quantity (no fills yet for PENDING orders).
	if err := s.reserveFundsForOrder(ctx, updated); err != nil {
		// Order is APPROVED in DB — log the failure but don't revert the approval.
		// Engine will still execute; SettleBuyFill will debit stanje_racuna directly.
		log.Printf("[trading] rezervacija sredstava za odobreni nalog %d nije uspela: %v", orderID, err)
	}

	return updated, nil
}

// DeclineOrder transitions a PENDING order to DECLINED and records the
// supervisor's ID.
func (s *tradingService) DeclineOrder(ctx context.Context, orderID int64, supervisorID int64) (*Order, error) {
	order, err := s.orders.GetByID(ctx, orderID)
	if err != nil {
		return nil, err
	}

	if order.Status != OrderStatusPending {
		return nil, fmt.Errorf("%w: nalog je u statusu %s, a ne PENDING", ErrInvalidOrderState, order.Status)
	}

	updated, err := s.orders.UpdateStatus(ctx, orderID, OrderStatusDeclined, &supervisorID)
	if err != nil {
		return nil, fmt.Errorf("odbijanje naloga %d: %w", orderID, err)
	}
	return updated, nil
}

// ─── Cancelation ─────────────────────────────────────────────────────────────

// CancelOrder validates permissions and state, then atomically sets the order
// to CANCELED with remaining_portions=0 and is_done=true.
//
// Cancelable states: PENDING, APPROVED (and is_done=false).
// Non-cancelable states: DONE, DECLINED, CANCELED.
func (s *tradingService) CancelOrder(ctx context.Context, orderID int64, requestedBy int64) (*Order, error) {
	order, err := s.orders.GetByID(ctx, orderID)
	if err != nil {
		return nil, err
	}

	// ── Permission check ──────────────────────────────────────────────────────
	// The requester must be the order's original owner OR a Supervisor.
	// Clients who did not place the order have no authority over it.
	if order.UserID != requestedBy {
		isSuper, err := s.isSupervisor(ctx, requestedBy)
		if err != nil {
			return nil, fmt.Errorf("provjera privilegija za korisnika %d: %w", requestedBy, err)
		}
		if !isSuper {
			return nil, ErrPermissionDenied
		}
	}

	// ── State check ───────────────────────────────────────────────────────────
	// Only active orders (PENDING or APPROVED, not yet finished) can be canceled.
	// DONE      → execution already completed; nothing to stop.
	// DECLINED  → supervisor already rejected; no execution ever started.
	// CANCELED  → already canceled; idempotent rejection is clearer than a no-op.
	cancelable := (order.Status == OrderStatusPending || order.Status == OrderStatusApproved) && !order.IsDone
	if !cancelable {
		return nil, fmt.Errorf("%w: nalog je u statusu %s (is_done=%v)", ErrInvalidOrderState, order.Status, order.IsDone)
	}

	// Release reserved funds before canceling — only APPROVED BUY orders have
	// funds reserved.  PENDING orders were never charged.
	if order.Status == OrderStatusApproved {
		s.releaseFundsForOrder(ctx, order)
	}

	canceled, err := s.orders.Cancel(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("otkazivanje naloga %d: %w", orderID, err)
	}
	return canceled, nil
}

// ─── Private helpers ──────────────────────────────────────────────────────────

// isSupervisor returns true when userID is registered in actuary_info with
// type SUPERVISOR.  Clients (no actuary_info row) and Agents return false.
// Used by CancelOrder for permission validation.
func (s *tradingService) isSupervisor(ctx context.Context, userID int64) (bool, error) {
	actuary, err := s.actuaries.GetByEmployeeID(ctx, userID)
	if errors.Is(err, domain.ErrActuaryNotFound) {
		return false, nil // regular Client
	}
	if err != nil {
		return false, fmt.Errorf("dohvatanje aktuar zapisa za korisnika %d: %w", userID, err)
	}
	return actuary.ActuaryType == domain.ActuaryTypeSupervisor, nil
}

// ─── Funds helpers ────────────────────────────────────────────────────────────

// computeNotional calculates the notional value for a given order and quantity.
// For MARKET orders, fetches the current Ask/Bid price from the listing service.
// Used to determine how much to reserve or release from the account.
func (s *tradingService) computeNotional(ctx context.Context, order *Order, qty int32) (decimal.Decimal, error) {
	switch order.OrderType {
	case OrderTypeMarket:
		listing, err := s.listings.GetListingByID(ctx, order.ListingID)
		if err != nil {
			return decimal.Zero, fmt.Errorf("dohvatanje hartije %d za kalkulaciju iznosa: %w", order.ListingID, err)
		}
		var unitPrice decimal.Decimal
		if order.Direction == OrderDirectionBuy {
			unitPrice = decimal.NewFromFloat(listing.Ask)
		} else {
			unitPrice = decimal.NewFromFloat(listing.Bid)
		}
		return approxPrice(order.ContractSize, unitPrice, qty), nil

	case OrderTypeLimit, OrderTypeStopLimit:
		if order.PricePerUnit == nil {
			return decimal.Zero, ErrLimitPriceRequired
		}
		return approxPrice(order.ContractSize, *order.PricePerUnit, qty), nil

	case OrderTypeStop:
		if order.StopPrice == nil {
			return decimal.Zero, ErrStopPriceRequired
		}
		return approxPrice(order.ContractSize, *order.StopPrice, qty), nil

	default:
		return decimal.Zero, fmt.Errorf("%w: %s", ErrInvalidOrderType, order.OrderType)
	}
}

// reserveFundsForOrder reserves the full order notional (based on Quantity) on
// the account.  Only applicable to BUY orders.  Returns an error so callers can
// react (cancel the order or log a visible warning) instead of silently skipping.
func (s *tradingService) reserveFundsForOrder(ctx context.Context, order *Order) error {
	if order.Direction != OrderDirectionBuy {
		return nil
	}
	notional, err := s.computeNotional(ctx, order, order.Quantity)
	if err != nil {
		return fmt.Errorf("izračunavanje iznosa za rezervaciju (nalog %d): %w", order.ID, err)
	}
	if err := s.funds.ReserveFunds(ctx, order.AccountID, notional); err != nil {
		return fmt.Errorf("rezervacija sredstava za nalog %d: %w", order.ID, err)
	}
	return nil
}

// releaseFundsForOrder releases the remaining notional (based on RemainingPortions)
// from the account reservation.  Only applicable to APPROVED BUY orders.
func (s *tradingService) releaseFundsForOrder(ctx context.Context, order *Order) {
	if order.Direction != OrderDirectionBuy {
		return
	}
	notional, err := s.computeNotional(ctx, order, order.RemainingPortions)
	if err != nil {
		log.Printf("[trading] izračunavanje iznosa za oslobađanje (nalog %d): %v", order.ID, err)
		return
	}
	if err := s.funds.ReleaseFunds(ctx, order.AccountID, notional); err != nil {
		log.Printf("[trading] oslobađanje sredstava za nalog %d: %v", order.ID, err)
	}
}
