package repository

import (
	"context"
	"fmt"
	"log"
	"time"

	"banka-backend/services/bank-service/internal/domain"
	"banka-backend/services/bank-service/internal/trading"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type fundsManager struct {
	db              *gorm.DB
	exchangeService domain.ExchangeService
}

// NewFundsManager vraća implementaciju trading.FundsManager koja direktno
// ažurira core_banking.racun u jednoj SQL naredbi (atomično).
// exchangeService se koristi za konverziju USD iznosa u valutu klijentskog računa.
func NewFundsManager(db *gorm.DB, exchangeService domain.ExchangeService) trading.FundsManager {
	return &fundsManager{db: db, exchangeService: exchangeService}
}

// accountCurrency vraća ISO kod valute za dati račun.
// Vraća "USD" kao fallback ako dohvat ne uspe (konzervativno).
func (f *fundsManager) accountCurrency(ctx context.Context, accountID int64) string {
	var currency string
	f.db.WithContext(ctx).Raw(`
		SELECT v.oznaka FROM core_banking.racun r
		JOIN core_banking.valuta v ON v.id = r.valuta_id
		WHERE r.id = ?
	`, accountID).Scan(&currency)
	if currency == "" {
		return "USD"
	}
	return currency
}

// convertUSDToAccountCurrency konvertuje USD iznos u valutu računa koristeći
// srednji kurs (bez provizije za zaposlene, ili prodajni kurs za klijente).
// Trenutna implementacija koristi srednji kurs za sve korisnike.
func (f *fundsManager) convertUSDToAccountCurrency(ctx context.Context, usdAmount decimal.Decimal, targetCurrency string) decimal.Decimal {
	if targetCurrency == "USD" {
		return usdAmount
	}

	rates, err := f.exchangeService.GetRates(ctx)
	if err != nil {
		log.Printf("[funds_manager] nije moguće dohvatiti kurseve za konverziju: %v — koristi se USD iznos", err)
		return usdAmount
	}

	// USD → RSD
	var usdToRSD float64
	for _, r := range rates {
		if r.Oznaka == "USD" {
			usdToRSD = r.Srednji
			break
		}
	}
	if usdToRSD <= 0 {
		return usdAmount
	}
	rsdAmount := usdAmount.Mul(decimal.NewFromFloat(usdToRSD))

	if targetCurrency == "RSD" {
		return rsdAmount
	}

	// RSD → target currency
	for _, r := range rates {
		if r.Oznaka == targetCurrency && r.Srednji > 0 {
			return rsdAmount.Div(decimal.NewFromFloat(r.Srednji))
		}
	}

	// Valuta nije pronađena — vrati RSD iznos kao sigurni fallback
	log.Printf("[funds_manager] valuta %q nije pronađena u kursnoj listi — koristi se RSD iznos", targetCurrency)
	return rsdAmount
}

// ReserveFunds povećava rezervisana_sredstva za dati iznos.
func (f *fundsManager) ReserveFunds(ctx context.Context, accountID int64, amount decimal.Decimal) error {
	result := f.db.WithContext(ctx).Exec(
		`UPDATE core_banking.racun
		 SET rezervisana_sredstva = rezervisana_sredstva + ?
		 WHERE id = ?`,
		amount.InexactFloat64(), accountID,
	)
	if result.Error != nil {
		return fmt.Errorf("rezervacija sredstava za račun %d: %w", accountID, result.Error)
	}
	return nil
}

// ReleaseFunds smanjuje rezervisana_sredstva za dati iznos (ne ide ispod 0).
func (f *fundsManager) ReleaseFunds(ctx context.Context, accountID int64, amount decimal.Decimal) error {
	result := f.db.WithContext(ctx).Exec(
		`UPDATE core_banking.racun
		 SET rezervisana_sredstva = GREATEST(0, rezervisana_sredstva - ?)
		 WHERE id = ?`,
		amount.InexactFloat64(), accountID,
	)
	if result.Error != nil {
		return fmt.Errorf("oslobađanje sredstava za račun %d: %w", accountID, result.Error)
	}
	return nil
}

// SettleBuyFill atomično smanjuje i stanje_racuna i rezervisana_sredstva
// za iznos konvertovan u valutu računa. Kreira i zapis u transakcija tabeli.
// amount je u USD (valuta berze); konvertuje se u valutu računa pre oduzimanja.
func (f *fundsManager) SettleBuyFill(ctx context.Context, accountID int64, amount decimal.Decimal) error {
	currency := f.accountCurrency(ctx, accountID)
	debit := f.convertUSDToAccountCurrency(ctx, amount, currency)

	result := f.db.WithContext(ctx).Exec(
		`UPDATE core_banking.racun
		 SET stanje_racuna        = stanje_racuna - ?,
		     rezervisana_sredstva = GREATEST(0, rezervisana_sredstva - ?)
		 WHERE id = ?`,
		debit.InexactFloat64(), debit.InexactFloat64(), accountID,
	)
	if result.Error != nil {
		return fmt.Errorf("namirenje BUY filla za račun %d: %w", accountID, result.Error)
	}
	f.db.WithContext(ctx).Create(&transakcijaModel{
		RacunID:          accountID,
		TipTransakcije:   "ISPLATA",
		Iznos:            debit.InexactFloat64(),
		Opis:             "Kupovina hartije od vrednosti",
		VremeIzvrsavanja: time.Now().UTC(),
		Status:           "IZVRSEN",
	})
	return nil
}

// CreditSellFill povećava stanje_racuna za iznos konvertovan u valutu računa.
// amount je u USD (valuta berze); konvertuje se u valutu računa pre dodavanja.
func (f *fundsManager) CreditSellFill(ctx context.Context, accountID int64, amount decimal.Decimal) error {
	currency := f.accountCurrency(ctx, accountID)
	credit := f.convertUSDToAccountCurrency(ctx, amount, currency)

	result := f.db.WithContext(ctx).Exec(
		`UPDATE core_banking.racun
		 SET stanje_racuna = stanje_racuna + ?
		 WHERE id = ?`,
		credit.InexactFloat64(), accountID,
	)
	if result.Error != nil {
		return fmt.Errorf("kredit SELL filla za račun %d: %w", accountID, result.Error)
	}
	f.db.WithContext(ctx).Create(&transakcijaModel{
		RacunID:          accountID,
		TipTransakcije:   "UPLATA",
		Iznos:            credit.InexactFloat64(),
		Opis:             "Prodaja hartije od vrednosti",
		VremeIzvrsavanja: time.Now().UTC(),
		Status:           "IZVRSEN",
	})
	return nil
}

// ChargeCommission smanjuje stanje_racuna za iznos provizije (ne dirá rezervisana_sredstva)
// i kreira zapis u transakcija tabeli.
func (f *fundsManager) ChargeCommission(ctx context.Context, accountID int64, amount decimal.Decimal) error {
	result := f.db.WithContext(ctx).Exec(
		`UPDATE core_banking.racun
		 SET stanje_racuna = stanje_racuna - ?
		 WHERE id = ?`,
		amount.InexactFloat64(), accountID,
	)
	if result.Error != nil {
		return fmt.Errorf("naplata provizije za račun %d: %w", accountID, result.Error)
	}
	f.db.WithContext(ctx).Create(&transakcijaModel{
		RacunID:          accountID,
		TipTransakcije:   "ISPLATA",
		Iznos:            amount.InexactFloat64(),
		Opis:             "Provizija za hartiju od vrednosti",
		VremeIzvrsavanja: time.Now().UTC(),
		Status:           "IZVRSEN",
	})
	return nil
}
