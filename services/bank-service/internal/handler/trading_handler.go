package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	pb "banka-backend/proto/banka"
	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/domain"
	"banka-backend/services/bank-service/internal/trading"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// orderToPb converts a trading.Order domain entity to the proto TradingOrder message.
func orderToPb(o trading.Order) *pb.TradingOrder {
	msg := &pb.TradingOrder{
		Id:                o.ID,
		UserId:            o.UserID,
		AccountId:         o.AccountID,
		ListingId:         o.ListingID,
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
		LastModified:      o.LastModified.UTC().Format("2006-01-02T15:04:05Z"),
		CreatedAt:         o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if o.PricePerUnit != nil {
		v := o.PricePerUnit.String()
		msg.PricePerUnit = &v
	}
	if o.StopPrice != nil {
		v := o.StopPrice.String()
		msg.StopPrice = &v
	}
	if o.ApprovedBy != nil {
		msg.ApprovedBy = o.ApprovedBy
	}
	return msg
}

// tradingError maps domain sentinel errors to appropriate gRPC status codes.
func tradingError(err error) error {
	switch {
	case errors.Is(err, trading.ErrOrderNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, trading.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, trading.ErrInvalidOrderState):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, trading.ErrInsufficientMargin):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, trading.ErrLimitPriceRequired),
		errors.Is(err, trading.ErrStopPriceRequired),
		errors.Is(err, trading.ErrInvalidOrderType),
		errors.Is(err, trading.ErrInvalidDirection),
		errors.Is(err, trading.ErrListingTypeNotAllowed):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, trading.ErrSettlementDatePassed):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "interna greška: %v", err)
	}
}

// parseOptionalDecimal parses a *string proto field into a *decimal.Decimal.
// Returns an InvalidArgument status error if the string is not a valid decimal.
func parseOptionalDecimal(field *string, name string) (*decimal.Decimal, error) {
	if field == nil {
		return nil, nil
	}
	d, err := decimal.NewFromString(*field)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "neispravan %s: %v", name, err)
	}
	return &d, nil
}

// =============================================================================
// TradingCalculate
// =============================================================================

// TradingCalculate computes an approximate price and commission without persisting anything.
// Auth: any authenticated user.
// Mapped to: POST /bank/trading/calculate
func (h *BankHandler) TradingCalculate(ctx context.Context, req *pb.TradingCalculateRequest) (*pb.TradingCalculateResponse, error) {
	if _, ok := auth.ClaimsFromContext(ctx); !ok {
		return nil, status.Error(codes.Unauthenticated, "niste autentifikovani")
	}

	calcReq := &trading.OrderCalculationRequest{
		OrderType:    trading.OrderType(req.GetOrderType()),
		Direction:    trading.OrderDirection(req.GetDirection()),
		ListingID:    req.GetListingId(),
		Quantity:     req.GetQuantity(),
		ContractSize: req.GetContractSize(),
		Margin:       req.GetMargin(),
		AllOrNone:    req.GetAllOrNone(),
	}

	ppu, err := parseOptionalDecimal(req.PricePerUnit, "price_per_unit")
	if err != nil {
		return nil, err
	}
	calcReq.PricePerUnit = ppu

	sp, err := parseOptionalDecimal(req.StopPrice, "stop_price")
	if err != nil {
		return nil, err
	}
	calcReq.StopPrice = sp

	resp, err := h.tradingService.CalculateOrderDetails(ctx, calcReq)
	if err != nil {
		return nil, tradingError(err)
	}

	out := &pb.TradingCalculateResponse{
		PricePerUnit:    resp.PricePerUnit.String(),
		ApproximatePrice: resp.ApproximatePrice.String(),
		Commission:      resp.Commission.String(),
	}
	if resp.InitialMarginCost != nil {
		v := resp.InitialMarginCost.String()
		out.InitialMarginCost = &v
	}
	return out, nil
}

// =============================================================================
// TradingCreateOrder
// =============================================================================

// TradingCreateOrder places a new order and applies the approval workflow.
// Auth: any authenticated user.
// Mapped to: POST /bank/trading/orders
func (h *BankHandler) TradingCreateOrder(ctx context.Context, req *pb.TradingCreateOrderRequest) (*pb.TradingCreateOrderResponse, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "niste autentifikovani")
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "neispravan korisnički ID u tokenu: %v", err)
	}

	// Resolve accountId: employees trade from the bank's USD trezor account.
	// The frontend sends accountId=0 for employees; we resolve the real ID here.
	accountID := req.GetAccountId()
	if accountID == 0 && (claims.UserType == "EMPLOYEE" || claims.UserType == "ADMIN") {
		trezorID, lookupErr := h.accountService.FindAccountIDByNumber(ctx, "666000122200000008")
		if lookupErr != nil {
			return nil, status.Errorf(codes.Internal, "greška pri traženju trezor računa: %v", lookupErr)
		}
		if trezorID == 0 {
			return nil, status.Error(codes.Internal, "bankin USD trezor račun nije pronađen (broj: 666000122200000008)")
		}
		accountID = trezorID
	}

	// Determine supervisor status from JWT — the authoritative source.
	// An ADMIN is always treated as a supervisor; for EMPLOYEE we check the
	// permissions array for the "SUPERVISOR" permission.
	isSupervisor := claims.UserType == "ADMIN"
	if !isSupervisor {
		for _, p := range claims.Permissions {
			if p == "SUPERVISOR" {
				isSupervisor = true
				break
			}
		}
	}

	domainReq := &trading.CreateOrderRequest{
		UserID:       userID,
		AccountID:    accountID,
		ListingID:    req.GetListingId(),
		OrderType:    trading.OrderType(req.GetOrderType()),
		Direction:    trading.OrderDirection(req.GetDirection()),
		Quantity:     req.GetQuantity(),
		ContractSize: req.GetContractSize(),
		AfterHours:   req.GetAfterHours(),
		AllOrNone:    req.GetAllOrNone(),
		Margin:       req.GetMargin(),
		IsSupervisor: isSupervisor,
	}

	ppu, err := parseOptionalDecimal(req.PricePerUnit, "price_per_unit")
	if err != nil {
		return nil, err
	}
	domainReq.PricePerUnit = ppu

	sp, err := parseOptionalDecimal(req.StopPrice, "stop_price")
	if err != nil {
		return nil, err
	}
	domainReq.StopPrice = sp

	// ── Listing-level validations ─────────────────────────────────────────────
	// Fetch listing once for all checks so we avoid multiple round-trips.
	listing, err := h.listingService.GetListingByID(ctx, domainReq.ListingID)
	if err != nil {
		if errors.Is(err, domain.ErrListingNotFound) {
			return nil, status.Errorf(codes.NotFound, "hartija od vrednosti nije pronađena: %d", domainReq.ListingID)
		}
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu hartije: %v", err)
	}

	// Klijenti mogu trgovati samo akcijama i futures-ima (spec §2).
	if claims.UserType == "CLIENT" {
		if listing.ListingType != domain.ListingTypeStock && listing.ListingType != domain.ListingTypeFuture {
			return nil, tradingError(trading.ErrListingTypeNotAllowed)
		}
	}

	// Automatsko odbijanje za istekle FUTURE/OPTION instrumente (spec §7).
	if listing.ListingType == domain.ListingTypeFuture || listing.ListingType == domain.ListingTypeOption {
		if expired := settlementDateExpired(listing.DetailsJSON); expired {
			return nil, tradingError(trading.ErrSettlementDatePassed)
		}
	}

	// AfterHours detekcija: ako je berza zatvorena ili u after-hours periodu (spec §7).
	// Frontend šalje false po defaultu; server uvek overriduje sa tačnom vrijednošću.
	if marketStatus, msErr := h.berzaService.IsExchangeOpen(ctx, listing.ExchangeID); msErr == nil {
		domainReq.AfterHours = marketStatus == domain.MarketStatusAfterHours ||
			marketStatus == domain.MarketStatusClosed
	}

	order, err := h.tradingService.CreateOrder(ctx, domainReq)
	if err != nil {
		return nil, tradingError(err)
	}
	return &pb.TradingCreateOrderResponse{Order: orderToPb(*order)}, nil
}

// settlementDateExpired returns true when the settlement_date in details_json
// is in the past.  Returns false if the field is missing or cannot be parsed
// (conservative: don't block orders on parse failures).
func settlementDateExpired(detailsJSON string) bool {
	var details struct {
		SettlementDate string `json:"settlement_date"`
	}
	if err := json.Unmarshal([]byte(detailsJSON), &details); err != nil || details.SettlementDate == "" {
		return false
	}
	// Accept both date-only and full RFC3339 formats.
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, details.SettlementDate); err == nil {
			return t.Before(time.Now().UTC())
		}
	}
	return false
}

// =============================================================================
// TradingListOrders
// =============================================================================

// TradingListOrders returns orders with an optional status filter.
// Auth:
//   - EMPLOYEE / ADMIN → returns ALL orders (supervisor dashboard view).
//   - CLIENT           → returns only the caller's own orders (Moji nalozi view).
//
// Mapped to: GET /bank/trading/orders
func (h *BankHandler) TradingListOrders(ctx context.Context, req *pb.TradingListOrdersRequest) (*pb.TradingListOrdersResponse, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "niste autentifikovani")
	}

	var statusFilter *trading.OrderStatus
	if s := req.GetStatus(); s != "" {
		v := trading.OrderStatus(s)
		statusFilter = &v
	}

	var orders []trading.Order
	var err error

	if claims.UserType == "CLIENT" {
		callerID, parseErr := strconv.ParseInt(claims.Subject, 10, 64)
		if parseErr != nil {
			return nil, status.Errorf(codes.Internal, "neispravan korisnički ID u tokenu: %v", parseErr)
		}
		orders, err = h.tradingService.ListOrdersByUser(ctx, callerID, statusFilter)
	} else if claims.UserType == "EMPLOYEE" || claims.UserType == "ADMIN" {
		orders, err = h.tradingService.ListOrders(ctx, statusFilter)
	} else {
		return nil, status.Error(codes.PermissionDenied, "pristup odbijen")
	}

	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu naloga: %v", err)
	}

	pbOrders := make([]*pb.TradingOrder, 0, len(orders))
	for _, o := range orders {
		pbOrders = append(pbOrders, orderToPb(o))
	}
	return &pb.TradingListOrdersResponse{Orders: pbOrders}, nil
}

// =============================================================================
// TradingApproveOrder
// =============================================================================

// TradingApproveOrder transitions a PENDING order to APPROVED.
// Auth: EMPLOYEE only.
// Mapped to: POST /bank/trading/orders/{order_id}/approve
func (h *BankHandler) TradingApproveOrder(ctx context.Context, req *pb.TradingApproveOrderRequest) (*pb.TradingApproveOrderResponse, error) {
	supervisorID, err := extractEmployeeID(ctx)
	if err != nil {
		return nil, err
	}

	order, err := h.tradingService.ApproveOrder(ctx, req.GetOrderId(), supervisorID)
	if err != nil {
		return nil, tradingError(err)
	}
	return &pb.TradingApproveOrderResponse{Order: orderToPb(*order)}, nil
}

// =============================================================================
// TradingDeclineOrder
// =============================================================================

// TradingDeclineOrder transitions a PENDING order to DECLINED.
// Auth: EMPLOYEE only.
// Mapped to: POST /bank/trading/orders/{order_id}/decline
func (h *BankHandler) TradingDeclineOrder(ctx context.Context, req *pb.TradingDeclineOrderRequest) (*pb.TradingDeclineOrderResponse, error) {
	supervisorID, err := extractEmployeeID(ctx)
	if err != nil {
		return nil, err
	}

	order, err := h.tradingService.DeclineOrder(ctx, req.GetOrderId(), supervisorID)
	if err != nil {
		return nil, tradingError(err)
	}
	return &pb.TradingDeclineOrderResponse{Order: orderToPb(*order)}, nil
}

// =============================================================================
// TradingCancelOrder
// =============================================================================

// TradingCancelOrder stops an active (PENDING or APPROVED) order.
// Auth: the order's owner OR any EMPLOYEE.
// Mapped to: POST /bank/trading/orders/{order_id}/cancel
func (h *BankHandler) TradingCancelOrder(ctx context.Context, req *pb.TradingCancelOrderRequest) (*pb.TradingCancelOrderResponse, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "niste autentifikovani")
	}
	callerID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "neispravan korisnički ID u tokenu: %v", err)
	}

	order, err := h.tradingService.CancelOrder(ctx, req.GetOrderId(), callerID)
	if err != nil {
		return nil, tradingError(err)
	}
	return &pb.TradingCancelOrderResponse{Order: orderToPb(*order)}, nil
}
