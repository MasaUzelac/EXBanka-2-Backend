package mocks

import (
	"context"
	"time"

	"github.com/stretchr/testify/mock"

	"banka-backend/services/bank-service/internal/domain"
)

// MockAccountRepository is a testify mock for domain.AccountRepository.
type MockAccountRepository struct {
	mock.Mock
}

func (m *MockAccountRepository) CreateAccount(ctx context.Context, input domain.CreateAccountInput, brojRacuna string) (int64, error) {
	args := m.Called(ctx, input, brojRacuna)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockAccountRepository) GetAllAccounts(ctx context.Context, filter string) ([]domain.EmployeeAccountListItem, error) {
	args := m.Called(ctx, filter)
	v, _ := args.Get(0).([]domain.EmployeeAccountListItem)
	return v, args.Error(1)
}

func (m *MockAccountRepository) GetClientAccounts(ctx context.Context, vlasnikID int64) ([]domain.AccountListItem, error) {
	args := m.Called(ctx, vlasnikID)
	v, _ := args.Get(0).([]domain.AccountListItem)
	return v, args.Error(1)
}

func (m *MockAccountRepository) GetAccountDetail(ctx context.Context, accountID, vlasnikID int64) (*domain.AccountDetail, error) {
	args := m.Called(ctx, accountID, vlasnikID)
	v, _ := args.Get(0).(*domain.AccountDetail)
	return v, args.Error(1)
}

func (m *MockAccountRepository) GetAccountTransactions(ctx context.Context, input domain.GetAccountTransactionsInput, vlasnikID int64) ([]domain.Transakcija, error) {
	args := m.Called(ctx, input, vlasnikID)
	v, _ := args.Get(0).([]domain.Transakcija)
	return v, args.Error(1)
}

func (m *MockAccountRepository) RenameAccount(ctx context.Context, input domain.RenameAccountInput) error {
	args := m.Called(ctx, input)
	return args.Error(0)
}

func (m *MockAccountRepository) UpdateAccountLimit(ctx context.Context, input domain.UpdateLimitInput) (int64, error) {
	args := m.Called(ctx, input)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockAccountRepository) GetPendingActions(ctx context.Context, vlasnikID int64) ([]domain.PendingAction, error) {
	args := m.Called(ctx, vlasnikID)
	v, _ := args.Get(0).([]domain.PendingAction)
	return v, args.Error(1)
}

func (m *MockAccountRepository) GetPendingAction(ctx context.Context, actionID, vlasnikID int64) (*domain.PendingAction, error) {
	args := m.Called(ctx, actionID, vlasnikID)
	v, _ := args.Get(0).(*domain.PendingAction)
	return v, args.Error(1)
}

func (m *MockAccountRepository) ApprovePendingAction(ctx context.Context, actionID, vlasnikID int64) (string, time.Time, error) {
	args := m.Called(ctx, actionID, vlasnikID)
	return args.String(0), args.Get(1).(time.Time), args.Error(2)
}

func (m *MockAccountRepository) VerifyAndApplyLimit(ctx context.Context, input domain.VerifyLimitInput) error {
	args := m.Called(ctx, input)
	return args.Error(0)
}

func (m *MockAccountRepository) FindAccountIDByNumber(ctx context.Context, brojRacuna string) (int64, error) {
	args := m.Called(ctx, brojRacuna)
	return args.Get(0).(int64), args.Error(1)
}
