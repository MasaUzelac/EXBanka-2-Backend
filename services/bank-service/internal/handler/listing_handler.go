package handler

import (
	"context"
	"errors"
	"time"

	pb "banka-backend/proto/banka"
	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetListings vraća paginisanu listu hartija od vrednosti sa filterima.
// Mapped to: GET /bank/listings
func (h *BankHandler) GetListings(ctx context.Context, req *pb.GetListingsRequest) (*pb.GetListingsResponse, error) {
	if err := requireClientOrEmployee(ctx); err != nil {
		return nil, err
	}

	claims, _ := auth.ClaimsFromContext(ctx)

	var minPrice *float64
	if req.GetMinPrice() > 0 {
		v := req.GetMinPrice()
		minPrice = &v
	}
	var maxPrice *float64
	if req.GetMaxPrice() > 0 {
		v := req.GetMaxPrice()
		maxPrice = &v
	}
	var minVolume *int64
	if req.GetMinVolume() > 0 {
		v := req.GetMinVolume()
		minVolume = &v
	}
	var maxVolume *int64
	if req.GetMaxVolume() > 0 {
		v := req.GetMaxVolume()
		maxVolume = &v
	}

	filter := domain.ListingFilter{
		ListingType:    req.GetListingType(),
		Search:         req.GetSearch(),
		MinPrice:       minPrice,
		MaxPrice:       maxPrice,
		MinVolume:      minVolume,
		MaxVolume:      maxVolume,
		SettlementFrom: req.GetSettlementFrom(),
		SettlementTo:   req.GetSettlementTo(),
		SortBy:         req.GetSortBy(),
		SortOrder:      req.GetSortOrder(),
		Page:           req.GetPage(),
		PageSize:       req.GetPageSize(),
	}

	// Klijenti mogu da vide i trguju samo akcijama i futures-ima (spec §2)
	if claims != nil && claims.UserType == "CLIENT" {
		filter.AllowedListingTypes = []string{
			string(domain.ListingTypeStock),
			string(domain.ListingTypeFuture),
		}
	}

	listings, total, err := h.listingService.ListListings(ctx, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to fetch listings: %v", err)
	}

	items := make([]*pb.ListingListItem, 0, len(listings))
	for _, l := range listings {
		items = append(items, listingCalculatedToProto(l))
	}

	return &pb.GetListingsResponse{
		Listings: items,
		Total:    int32(total),
	}, nil
}

// GetListingByID vraća detalje jedne hartije od vrednosti.
// Mapped to: GET /bank/listings/{id}
func (h *BankHandler) GetListingByID(ctx context.Context, req *pb.GetListingByIDRequest) (*pb.ListingDetail, error) {
	if err := requireClientOrEmployee(ctx); err != nil {
		return nil, err
	}

	calc, err := h.listingService.GetListingByID(ctx, req.GetId())
	if err != nil {
		if errors.Is(err, domain.ErrListingNotFound) {
			return nil, status.Errorf(codes.NotFound, "hartija od vrednosti nije pronađena: %d", req.GetId())
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch listing: %v", err)
	}

	return &pb.ListingDetail{
		Base:              listingCalculatedToProto(*calc),
		NominalValue:      calc.NominalValue,
		ContractSize:      calc.ContractSize,
		MaintenanceMargin: calc.MaintenanceMargin,
		DetailsJson:       calc.DetailsJSON,
	}, nil
}

// GetListingHistory vraća istoriju dnevnih cena za datu hartiju.
// Mapped to: GET /bank/listings/{id}/history
func (h *BankHandler) GetListingHistory(ctx context.Context, req *pb.GetListingHistoryRequest) (*pb.GetListingHistoryResponse, error) {
	if err := requireClientOrEmployee(ctx); err != nil {
		return nil, err
	}

	// Parsovanje datuma — default: poslednja godina
	toDate := time.Now().UTC()
	fromDate := toDate.AddDate(-1, 0, 0)

	if s := req.GetFromDate(); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			fromDate = t
		}
	}
	if s := req.GetToDate(); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			toDate = t
		}
	}

	history, err := h.listingService.GetListingHistory(ctx, req.GetId(), fromDate, toDate)
	if err != nil {
		if errors.Is(err, domain.ErrListingNotFound) {
			return nil, status.Errorf(codes.NotFound, "hartija od vrednosti nije pronađena: %d", req.GetId())
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch listing history: %v", err)
	}

	items := make([]*pb.ListingHistoryItem, 0, len(history))
	for _, h := range history {
		items = append(items, &pb.ListingHistoryItem{
			Date:        h.Date.Format("2006-01-02"),
			Price:       h.Price,
			AskHigh:     h.AskHigh,
			BidLow:      h.BidLow,
			PriceChange: h.PriceChange,
			Volume:      h.Volume,
		})
	}

	return &pb.GetListingHistoryResponse{History: items}, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func listingCalculatedToProto(c domain.ListingCalculated) *pb.ListingListItem {
	lastRefresh := ""
	if c.LastRefresh != nil {
		lastRefresh = c.LastRefresh.Format(time.RFC3339)
	}
	return &pb.ListingListItem{
		Id:                c.ID,
		Ticker:            c.Ticker,
		Name:              c.Name,
		ListingType:       string(c.ListingType),
		ExchangeId:        c.ExchangeID,
		Price:             c.Price,
		Ask:               c.Ask,
		Bid:               c.Bid,
		Volume:            c.Volume,
		ChangePercent:     c.ChangePercent,
		DollarVolume:      c.DollarVolume,
		InitialMarginCost: c.InitialMarginCost,
		LastRefresh:       lastRefresh,
	}
}
