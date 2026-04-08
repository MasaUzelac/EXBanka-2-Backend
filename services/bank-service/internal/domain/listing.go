package domain

import (
	"context"
	"errors"
	"time"
)

// ErrListingNotFound se vraća kada hartija od vrednosti nije pronađena.
var ErrListingNotFound = errors.New("hartija od vrednosti nije pronađena")

// ListingType definiše tip hartije od vrednosti.
type ListingType string

const (
	ListingTypeStock  ListingType = "STOCK"
	ListingTypeForex  ListingType = "FOREX"
	ListingTypeFuture ListingType = "FUTURE"
	ListingTypeOption ListingType = "OPTION"
)

// Listing je osnovna domain struktura za hartiju od vrednosti.
type Listing struct {
	ID          int64
	Ticker      string
	Name        string
	ExchangeID  int64
	ListingType ListingType
	LastRefresh *time.Time
	Price       float64
	Ask         float64
	Bid         float64
	Volume      int64
	DetailsJSON string // raw JSON kao TEXT (isti pattern kao params_json)
}

// ListingDailyPriceInfo čuva dnevne cenovne podatke za hartiju.
type ListingDailyPriceInfo struct {
	ID          int64
	ListingID   int64
	Date        time.Time
	Price       float64
	AskHigh     float64
	BidLow      float64
	PriceChange float64
	Volume      int64
}

// ListingFilter definiše parametre za filtriranje i paginaciju liste hartija.
type ListingFilter struct {
	ListingType         string
	AllowedListingTypes []string // ako je neprazno, ograničava rezultate samo na navedene tipove (npr. za klijente)
	Search              string
	MinPrice            *float64
	MaxPrice            *float64
	MinVolume           *int64
	MaxVolume           *int64
	SettlementFrom      string // YYYY-MM-DD, iz details_json za FUTURE/OPTION
	SettlementTo        string
	SortBy              string // "price" | "volume" | "change" | "ticker"
	SortOrder           string // "ASC" | "DESC"
	Page                int32
	PageSize            int32
}

// ListingCalculated sadrži izvedene finansijske vrednosti izračunate u servisnom sloju.
type ListingCalculated struct {
	Listing
	ChangePercent     float64
	DollarVolume      float64
	NominalValue      float64
	ContractSize      float64
	MaintenanceMargin float64
	InitialMarginCost float64
}

// ListingRepository definiše ugovor za pristup podacima o hartijama.
type ListingRepository interface {
	List(ctx context.Context, filter ListingFilter) ([]Listing, int64, error)
	GetByID(ctx context.Context, id int64) (*Listing, error)
	GetHistory(ctx context.Context, id int64, from, to time.Time) ([]ListingDailyPriceInfo, error)
	GetLatestDailyChange(ctx context.Context, id int64) (float64, error)
	UpdatePrices(ctx context.Context, id int64, price, ask, bid float64, volume int64, at time.Time) error
	UpdateDetails(ctx context.Context, id int64, detailsJSON string) error
	AppendDailyPrice(ctx context.Context, info ListingDailyPriceInfo) error
	ListAll(ctx context.Context) ([]Listing, error)
}

// ListingService definiše ugovor za poslovnu logiku nad hartijama.
type ListingService interface {
	ListListings(ctx context.Context, filter ListingFilter) ([]ListingCalculated, int64, error)
	GetListingByID(ctx context.Context, id int64) (*ListingCalculated, error)
	GetListingHistory(ctx context.Context, id int64, from, to time.Time) ([]ListingDailyPriceInfo, error)
}
