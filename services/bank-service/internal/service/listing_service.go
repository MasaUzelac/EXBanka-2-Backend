package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"banka-backend/services/bank-service/internal/domain"
)


// listingService implementira domain.ListingService.
type listingService struct {
	repo domain.ListingRepository
}

func NewListingService(repo domain.ListingRepository) domain.ListingService {
	return &listingService{repo: repo}
}

// ListListings vraća paginisanu listu hartija sa izvedenim finansijskim vrednostima.
func (s *listingService) ListListings(ctx context.Context, filter domain.ListingFilter) ([]domain.ListingCalculated, int64, error) {
	listings, total, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("listing service list: %w", err)
	}

	result := make([]domain.ListingCalculated, 0, len(listings))
	for _, l := range listings {
		change, _ := s.repo.GetLatestDailyChange(ctx, l.ID)
		result = append(result, calculate(l, change))
	}
	return result, total, nil
}

// GetListingByID vraća detalje jedne hartije sa izvedenim finansijskim vrednostima.
func (s *listingService) GetListingByID(ctx context.Context, id int64) (*domain.ListingCalculated, error) {
	l, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	change, _ := s.repo.GetLatestDailyChange(ctx, l.ID)
	calc := calculate(*l, change)
	return &calc, nil
}

// GetListingHistory vraća istoriju dnevnih cena za datu hartiju i period.
func (s *listingService) GetListingHistory(ctx context.Context, id int64, from, to time.Time) ([]domain.ListingDailyPriceInfo, error) {
	// Proveriti da hartija postoji
	if _, err := s.repo.GetByID(ctx, id); err != nil {
		return nil, err
	}
	return s.repo.GetHistory(ctx, id, from, to)
}

// ─── Finansijske kalkulacije ──────────────────────────────────────────────────

// calculate izračunava sve izvedene vrednosti za datu hartiju.
// change = price_change iz poslednjeg dnevnog zapisa (0 ako nema istorije).
func calculate(l domain.Listing, change float64) domain.ListingCalculated {
	cs, mm := contractSizeAndMargin(l)

	changePercent := 0.0
	if prev := l.Price - change; prev != 0 {
		changePercent = 100 * change / prev
	}

	return domain.ListingCalculated{
		Listing:           l,
		ChangePercent:     changePercent,
		DollarVolume:      float64(l.Volume) * l.Price,
		NominalValue:      cs * l.Price,
		ContractSize:      cs,
		MaintenanceMargin: mm,
		InitialMarginCost: mm * 1.1,
	}
}

// contractSizeAndMargin vraća Contract Size i Maintenance Margin prema tipu hartije.
//
//   - STOCK:  CS=1, MM=50% * Price
//   - FOREX:  CS=1000, MM=CS * Price * 10%
//   - FUTURE: CS=iz JSON ("contract_size"), MM=CS * (Price-tax) * 10%
//   - OPTION: CS=100, MM=CS * 50% * underlying_price (iz JSON)
func contractSizeAndMargin(l domain.Listing) (contractSize, maintenanceMargin float64) {
	switch l.ListingType {
	case domain.ListingTypeStock:
		contractSize = 1
		maintenanceMargin = 0.5 * l.Price

	case domain.ListingTypeForex:
		contractSize = 1000
		maintenanceMargin = contractSize * l.Price * 0.1

	case domain.ListingTypeFuture:
		contractSize = parseFutureContractSize(l.DetailsJSON)
		if contractSize == 0 {
			contractSize = 1
		}
		maintenanceMargin = contractSize * l.Price * 0.1

	case domain.ListingTypeOption:
		contractSize = 100
		underlyingPrice := parseOptionUnderlyingPrice(l.DetailsJSON)
		maintenanceMargin = contractSize * 0.5 * underlyingPrice
	}
	return contractSize, maintenanceMargin
}

// parseFutureContractSize čita "contract_size" iz details_json fajla.
func parseFutureContractSize(detailsJSON string) float64 {
	var details struct {
		ContractSize float64 `json:"contract_size"`
	}
	if err := json.Unmarshal([]byte(detailsJSON), &details); err != nil {
		return 0
	}
	return details.ContractSize
}

// parseOptionUnderlyingPrice čita "underlying_price" iz details_json fajla.
func parseOptionUnderlyingPrice(detailsJSON string) float64 {
	var details struct {
		UnderlyingPrice float64 `json:"underlying_price"`
	}
	if err := json.Unmarshal([]byte(detailsJSON), &details); err != nil {
		return 0
	}
	return details.UnderlyingPrice
}
