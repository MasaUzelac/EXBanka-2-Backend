package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"banka-backend/services/bank-service/internal/domain"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ─── GORM modeli ──────────────────────────────────────────────────────────────

type listingModel struct {
	ID          int64      `gorm:"column:id;primaryKey"`
	Ticker      string     `gorm:"column:ticker"`
	Name        string     `gorm:"column:name"`
	ExchangeID  int64      `gorm:"column:exchange_id"`
	ListingType string     `gorm:"column:listing_type"`
	LastRefresh *time.Time `gorm:"column:last_refresh"`
	Price       float64    `gorm:"column:price"`
	Ask         float64    `gorm:"column:ask"`
	Bid         float64    `gorm:"column:bid"`
	Volume      int64      `gorm:"column:volume"`
	DetailsJSON string     `gorm:"column:details_json"`
}

func (listingModel) TableName() string { return "core_banking.listing" }

func (m listingModel) toDomain() domain.Listing {
	return domain.Listing{
		ID:          m.ID,
		Ticker:      m.Ticker,
		Name:        m.Name,
		ExchangeID:  m.ExchangeID,
		ListingType: domain.ListingType(m.ListingType),
		LastRefresh: m.LastRefresh,
		Price:       m.Price,
		Ask:         m.Ask,
		Bid:         m.Bid,
		Volume:      m.Volume,
		DetailsJSON: m.DetailsJSON,
	}
}

type listingDailyPriceInfoModel struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	ListingID   int64     `gorm:"column:listing_id"`
	Date        time.Time `gorm:"column:date"`
	Price       float64   `gorm:"column:price"`
	AskHigh     float64   `gorm:"column:ask_high"`
	BidLow      float64   `gorm:"column:bid_low"`
	PriceChange float64   `gorm:"column:price_change"`
	Volume      int64     `gorm:"column:volume"`
}

func (listingDailyPriceInfoModel) TableName() string {
	return "core_banking.listing_daily_price_info"
}

func (m listingDailyPriceInfoModel) toDomain() domain.ListingDailyPriceInfo {
	return domain.ListingDailyPriceInfo{
		ID:          m.ID,
		ListingID:   m.ListingID,
		Date:        m.Date,
		Price:       m.Price,
		AskHigh:     m.AskHigh,
		BidLow:      m.BidLow,
		PriceChange: m.PriceChange,
		Volume:      m.Volume,
	}
}

// ─── Repository ───────────────────────────────────────────────────────────────

type listingRepository struct {
	db *gorm.DB
}

func NewListingRepository(db *gorm.DB) domain.ListingRepository {
	return &listingRepository{db: db}
}

// List vraća paginisanu listu hartija sa opcionalnim filterima.
func (r *listingRepository) List(ctx context.Context, filter domain.ListingFilter) ([]domain.Listing, int64, error) {
	q := r.db.WithContext(ctx).Model(&listingModel{})

	if filter.ListingType != "" {
		q = q.Where("listing_type = ?", filter.ListingType)
	}
	if len(filter.AllowedListingTypes) > 0 {
		q = q.Where("listing_type IN ?", filter.AllowedListingTypes)
	}

	if filter.Search != "" {
		like := "%" + strings.ToLower(filter.Search) + "%"
		q = q.Where("LOWER(ticker) LIKE ? OR LOWER(name) LIKE ?", like, like)
	}

	if filter.MinPrice != nil {
		q = q.Where("price >= ?", *filter.MinPrice)
	}
	if filter.MaxPrice != nil && *filter.MaxPrice > 0 {
		q = q.Where("price <= ?", *filter.MaxPrice)
	}
	if filter.MinVolume != nil {
		q = q.Where("volume >= ?", *filter.MinVolume)
	}
	if filter.MaxVolume != nil && *filter.MaxVolume > 0 {
		q = q.Where("volume <= ?", *filter.MaxVolume)
	}

	// Datum dospeća (settlement) u details_json — samo za FUTURE/OPTION; ostali tipovi uvek prolaze
	// NAPOMENA: ne koristimo PostgreSQL jsonb ? 'key' operator ovde jer GORM
	// interpretira '?' kao parameter placeholder, što kvari binding vrednosti.
	// Koristimo jsonb_exists() funkciju koja je funkcionalno ekvivalentna.
	if filter.SettlementFrom != "" {
		q = q.Where(`
			listing_type NOT IN ('FUTURE', 'OPTION')
			OR (
				details_json IS NOT NULL AND details_json != ''
				AND jsonb_exists(details_json::jsonb, 'settlement_date')
				AND (details_json::jsonb->>'settlement_date')::date >= ?
			)`, filter.SettlementFrom)
	}
	if filter.SettlementTo != "" {
		q = q.Where(`
			listing_type NOT IN ('FUTURE', 'OPTION')
			OR (
				details_json IS NOT NULL AND details_json != ''
				AND jsonb_exists(details_json::jsonb, 'settlement_date')
				AND (details_json::jsonb->>'settlement_date')::date <= ?
			)`, filter.SettlementTo)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("listing count: %w", err)
	}

	// Sortiranje
	orderCol := "ticker"
	switch filter.SortBy {
	case "price":
		orderCol = "price"
	case "volume":
		orderCol = "volume"
	case "change":
		// change se sortira po price_change iz daily tabele — fallback na ticker
		orderCol = "ticker"
	}
	orderDir := "ASC"
	if strings.ToUpper(filter.SortOrder) == "DESC" {
		orderDir = "DESC"
	}
	q = q.Order(fmt.Sprintf("%s %s", orderCol, orderDir))

	// Paginacija
	page := filter.Page
	if page < 1 {
		page = 1
	}
	pageSize := filter.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	q = q.Offset(int((page - 1) * pageSize)).Limit(int(pageSize))

	var models []listingModel
	if err := q.Find(&models).Error; err != nil {
		return nil, 0, fmt.Errorf("listing find: %w", err)
	}

	listings := make([]domain.Listing, len(models))
	for i, m := range models {
		listings[i] = m.toDomain()
	}
	return listings, total, nil
}

// GetByID vraća jednu hartiju po ID-u.
func (r *listingRepository) GetByID(ctx context.Context, id int64) (*domain.Listing, error) {
	var m listingModel
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrListingNotFound
		}
		return nil, fmt.Errorf("listing get by id: %w", err)
	}
	l := m.toDomain()
	return &l, nil
}

// GetHistory vraća istoriju dnevnih cena za dati period.
func (r *listingRepository) GetHistory(ctx context.Context, id int64, from, to time.Time) ([]domain.ListingDailyPriceInfo, error) {
	var models []listingDailyPriceInfoModel
	err := r.db.WithContext(ctx).
		Where("listing_id = ? AND date BETWEEN ? AND ?", id, from, to).
		Order("date ASC").
		Find(&models).Error
	if err != nil {
		return nil, fmt.Errorf("listing history: %w", err)
	}

	result := make([]domain.ListingDailyPriceInfo, len(models))
	for i, m := range models {
		result[i] = m.toDomain()
	}
	return result, nil
}

// GetLatestDailyChange vraća poslednju vrednost price_change za datu hartiju.
func (r *listingRepository) GetLatestDailyChange(ctx context.Context, id int64) (float64, error) {
	var m listingDailyPriceInfoModel
	err := r.db.WithContext(ctx).
		Where("listing_id = ?", id).
		Order("date DESC").
		Limit(1).
		First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing latest change: %w", err)
	}
	return m.PriceChange, nil
}

// UpdatePrices ažurira tekuće cene i vreme osvežavanja hartije.
func (r *listingRepository) UpdatePrices(ctx context.Context, id int64, price, ask, bid float64, volume int64, at time.Time) error {
	err := r.db.WithContext(ctx).
		Model(&listingModel{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"price":        price,
			"ask":          ask,
			"bid":          bid,
			"volume":       volume,
			"last_refresh": at,
		}).Error
	if err != nil {
		return fmt.Errorf("listing update prices: %w", err)
	}
	return nil
}

// UpdateDetails ažurira details_json kolonu za datu hartiju.
func (r *listingRepository) UpdateDetails(ctx context.Context, id int64, detailsJSON string) error {
	err := r.db.WithContext(ctx).
		Model(&listingModel{}).
		Where("id = ?", id).
		Update("details_json", detailsJSON).Error
	if err != nil {
		return fmt.Errorf("listing update details: %w", err)
	}
	return nil
}

// AppendDailyPrice upisuje (ili ažurira) dnevni cenovni zapis (upsert po uq_listing_daily).
func (r *listingRepository) AppendDailyPrice(ctx context.Context, info domain.ListingDailyPriceInfo) error {
	m := listingDailyPriceInfoModel{
		ListingID:   info.ListingID,
		Date:        info.Date,
		Price:       info.Price,
		AskHigh:     info.AskHigh,
		BidLow:      info.BidLow,
		PriceChange: info.PriceChange,
		Volume:      info.Volume,
	}
	err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "listing_id"}, {Name: "date"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"price", "ask_high", "bid_low", "price_change", "volume",
			}),
		}).
		Create(&m).Error
	if err != nil {
		return fmt.Errorf("listing append daily price: %w", err)
	}
	return nil
}

// ListAll vraća sve hartije bez filtera — koristi ga worker za iteraciju.
func (r *listingRepository) ListAll(ctx context.Context) ([]domain.Listing, error) {
	var models []listingModel
	if err := r.db.WithContext(ctx).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("listing list all: %w", err)
	}

	listings := make([]domain.Listing, len(models))
	for i, m := range models {
		listings[i] = m.toDomain()
	}
	return listings, nil
}
