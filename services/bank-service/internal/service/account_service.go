package service

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"banka-backend/services/bank-service/internal/domain"
)

// currencyService implementira domain.CurrencyService.
type currencyService struct {
	repo domain.CurrencyRepository
}

func NewCurrencyService(repo domain.CurrencyRepository) domain.CurrencyService {
	return &currencyService{repo: repo}
}

func (s *currencyService) GetCurrencies(ctx context.Context) ([]domain.Currency, error) {
	return s.repo.GetAll(ctx)
}

// ── Delatnost ─────────────────────────────────────────────────────────────────

type delatnostService struct {
	repo domain.DelatnostRepository
}

func NewDelatnostService(repo domain.DelatnostRepository) domain.DelatnostService {
	return &delatnostService{repo: repo}
}

func (s *delatnostService) GetDelatnosti(ctx context.Context) ([]domain.Delatnost, error) {
	return s.repo.GetAll(ctx)
}

// ── AccountService ────────────────────────────────────────────────────────────

var validDevizneValute = map[string]bool{
	"EUR": true, "CHF": true, "USD": true,
	"GBP": true, "JPY": true, "CAD": true, "AUD": true,
}

type accountService struct {
	repo         domain.AccountRepository
	currencyRepo domain.CurrencyRepository
}

func NewAccountService(repo domain.AccountRepository, currencyRepo domain.CurrencyRepository) domain.AccountService {
	return &accountService{repo: repo, currencyRepo: currencyRepo}
}

func (s *accountService) CreateAccount(ctx context.Context, input domain.CreateAccountInput) (int64, error) {
	if err := s.validateCurrency(ctx, input.ValutaID, input.KategorijaRacuna); err != nil {
		return 0, err
	}
	prefix, err := accountTypePrefix(input.KategorijaRacuna, input.VrstaRacuna, input.Podvrsta)
	if err != nil {
		return 0, domain.ErrInvalidPodvrsta
	}
	brojRacuna, err := generateAccountNumber(prefix)
	if err != nil {
		return 0, fmt.Errorf("greška pri generisanju broja računa: %w", err)
	}
	return s.repo.CreateAccount(ctx, input, brojRacuna)
}

func (s *accountService) GetAllAccounts(ctx context.Context, brojRacunaFilter string) ([]domain.EmployeeAccountListItem, error) {
	return s.repo.GetAllAccounts(ctx, brojRacunaFilter)
}

func (s *accountService) FindAccountIDByNumber(ctx context.Context, brojRacuna string) (int64, error) {
	return s.repo.FindAccountIDByNumber(ctx, brojRacuna)
}

func (s *accountService) GetClientAccounts(ctx context.Context, vlasnikID int64) ([]domain.AccountListItem, error) {
	return s.repo.GetClientAccounts(ctx, vlasnikID)
}

func (s *accountService) GetAccountDetail(ctx context.Context, accountID, vlasnikID int64) (*domain.AccountDetail, error) {
	return s.repo.GetAccountDetail(ctx, accountID, vlasnikID)
}

func (s *accountService) GetAccountTransactions(ctx context.Context, input domain.GetAccountTransactionsInput, vlasnikID int64) ([]domain.Transakcija, error) {
	return s.repo.GetAccountTransactions(ctx, input, vlasnikID)
}

func (s *accountService) RenameAccount(ctx context.Context, input domain.RenameAccountInput) error {
	return s.repo.RenameAccount(ctx, input)
}

func (s *accountService) UpdateAccountLimit(ctx context.Context, input domain.UpdateLimitInput) (int64, error) {
	return s.repo.UpdateAccountLimit(ctx, input)
}

func (s *accountService) GetPendingActions(ctx context.Context, vlasnikID int64) ([]domain.PendingAction, error) {
	return s.repo.GetPendingActions(ctx, vlasnikID)
}

func (s *accountService) GetPendingAction(ctx context.Context, actionID, vlasnikID int64) (*domain.PendingAction, error) {
	return s.repo.GetPendingAction(ctx, actionID, vlasnikID)
}

func (s *accountService) ApprovePendingAction(ctx context.Context, actionID, vlasnikID int64) (string, time.Time, error) {
	return s.repo.ApprovePendingAction(ctx, actionID, vlasnikID)
}

func (s *accountService) VerifyAndApplyLimit(ctx context.Context, input domain.VerifyLimitInput) error {
	return s.repo.VerifyAndApplyLimit(ctx, input)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *accountService) validateCurrency(ctx context.Context, valutaID int64, kategorija string) error {
	currency, err := s.currencyRepo.GetByID(ctx, valutaID)
	if err != nil {
		return fmt.Errorf("valuta sa ID %d nije pronađena: %w", valutaID, err)
	}
	switch kategorija {
	case "TEKUCI":
		if currency.Oznaka != "RSD" {
			return domain.ErrInvalidCurrency
		}
	case "DEVIZNI":
		if !validDevizneValute[currency.Oznaka] {
			return domain.ErrInvalidCurrency
		}
	}
	return nil
}

func accountTypePrefix(kategorija, vrsta, podvrsta string) (string, error) {
	const bankCode = "666"
	const branchCode = "0001"

	var typeCode string
	switch {
	case kategorija == "TEKUCI" && vrsta == "POSLOVNI":
		typeCode = "12"
	case kategorija == "DEVIZNI" && vrsta == "LICNI":
		typeCode = "21"
	case kategorija == "DEVIZNI" && vrsta == "POSLOVNI":
		typeCode = "22"
	case kategorija == "TEKUCI" && vrsta == "LICNI":
		switch strings.ToUpper(podvrsta) {
		case "STANDARDNI":
			typeCode = "11"
		case "STEDNI":
			typeCode = "13"
		case "PENZIONERSKI":
			typeCode = "14"
		case "MLADI":
			typeCode = "15"
		case "STUDENT":
			typeCode = "16"
		case "NEZAPOSLENI":
			typeCode = "17"
		default:
			return "", fmt.Errorf("nepoznata podvrsta: %s", podvrsta)
		}
	default:
		return "", fmt.Errorf("nevalidna kombinacija kategorija/vrsta: %s/%s", kategorija, vrsta)
	}

	return bankCode + branchCode + typeCode, nil
}

func generateAccountNumber(prefix string) (string, error) {
	prefixSum := 0
	for _, ch := range prefix {
		prefixSum += int(ch - '0')
	}

	maxRand := big.NewInt(100_000_000)

	for {
		n, err := rand.Int(rand.Reader, maxRand)
		if err != nil {
			return "", fmt.Errorf("crypto/rand: %w", err)
		}

		random8 := fmt.Sprintf("%08d", n.Int64())

		s := prefixSum
		for _, ch := range random8 {
			s += int(ch - '0')
		}

		check := (11 - s%11) % 11
		if check > 9 {
			continue
		}

		return prefix + random8 + fmt.Sprintf("%d", check), nil
	}
}
