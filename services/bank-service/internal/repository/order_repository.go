package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"banka-backend/services/bank-service/internal/trading"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ─── GORM modeli ──────────────────────────────────────────────────────────────

type orderModel struct {
	ID                int64    `gorm:"column:id;primaryKey"`
	UserID            int64    `gorm:"column:user_id"`
	AccountID         int64    `gorm:"column:account_id"`
	ListingID         int64    `gorm:"column:listing_id"`
	OrderType         string   `gorm:"column:order_type"`
	Direction         string   `gorm:"column:direction"`
	Quantity          int32    `gorm:"column:quantity"`
	ContractSize      int32    `gorm:"column:contract_size"`
	PricePerUnit      *string  `gorm:"column:price_per_unit"`
	StopPrice         *string  `gorm:"column:stop_price"`
	Status            string   `gorm:"column:status"`
	ApprovedBy        *int64   `gorm:"column:approved_by"`
	IsDone            bool     `gorm:"column:is_done"`
	RemainingPortions int32    `gorm:"column:remaining_portions"`
	AfterHours        bool     `gorm:"column:after_hours"`
	AllOrNone         bool     `gorm:"column:all_or_none"`
	Margin            bool     `gorm:"column:margin"`
	LastModified      time.Time `gorm:"column:last_modified"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (orderModel) TableName() string { return "core_banking.orders" }

func (m orderModel) toDomain() trading.Order {
	o := trading.Order{
		ID:                m.ID,
		UserID:            m.UserID,
		AccountID:         m.AccountID,
		ListingID:         m.ListingID,
		OrderType:         trading.OrderType(m.OrderType),
		Direction:         trading.OrderDirection(m.Direction),
		Quantity:          m.Quantity,
		ContractSize:      m.ContractSize,
		Status:            trading.OrderStatus(m.Status),
		ApprovedBy:        m.ApprovedBy,
		IsDone:            m.IsDone,
		RemainingPortions: m.RemainingPortions,
		AfterHours:        m.AfterHours,
		AllOrNone:         m.AllOrNone,
		Margin:            m.Margin,
		LastModified:      m.LastModified,
		CreatedAt:         m.CreatedAt,
	}
	if m.PricePerUnit != nil {
		d, err := decimal.NewFromString(*m.PricePerUnit)
		if err == nil {
			o.PricePerUnit = &d
		}
	}
	if m.StopPrice != nil {
		d, err := decimal.NewFromString(*m.StopPrice)
		if err == nil {
			o.StopPrice = &d
		}
	}
	return o
}

type orderTransactionModel struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	OrderID          int64     `gorm:"column:order_id"`
	ExecutedQuantity int32     `gorm:"column:executed_quantity"`
	ExecutedPrice    string    `gorm:"column:executed_price"`
	ExecutionTime    time.Time `gorm:"column:execution_time"`
}

func (orderTransactionModel) TableName() string { return "core_banking.order_transactions" }

func (m orderTransactionModel) toDomain() trading.OrderTransaction {
	price, _ := decimal.NewFromString(m.ExecutedPrice)
	return trading.OrderTransaction{
		ID:               m.ID,
		OrderID:          m.OrderID,
		ExecutedQuantity: m.ExecutedQuantity,
		ExecutedPrice:    price,
		ExecutionTime:    m.ExecutionTime,
	}
}

// ─── Repository ───────────────────────────────────────────────────────────────

type orderRepository struct {
	db *gorm.DB
}

// NewOrderRepository vraća implementaciju trading.OrderRepository koja koristi GORM.
func NewOrderRepository(db *gorm.DB) trading.OrderRepository {
	return &orderRepository{db: db}
}

// decimalToStr konvertuje *decimal.Decimal u *string za GORM (NUMERIC kolone).
func decimalToStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

// Create upisuje novi nalog u bazu. Status je određen od strane service sloja.
func (r *orderRepository) Create(ctx context.Context, req trading.CreateOrderRequest, status trading.OrderStatus) (*trading.Order, error) {
	now := time.Now().UTC()
	m := orderModel{
		UserID:            req.UserID,
		AccountID:         req.AccountID,
		ListingID:         req.ListingID,
		OrderType:         string(req.OrderType),
		Direction:         string(req.Direction),
		Quantity:          req.Quantity,
		ContractSize:      req.ContractSize,
		PricePerUnit:      decimalToStr(req.PricePerUnit),
		StopPrice:         decimalToStr(req.StopPrice),
		Status:            string(status),
		IsDone:            false,
		RemainingPortions: req.Quantity,
		AfterHours:        req.AfterHours,
		AllOrNone:         req.AllOrNone,
		Margin:            req.Margin,
		LastModified:      now,
	}
	if err := r.db.WithContext(ctx).Create(&m).Error; err != nil {
		return nil, fmt.Errorf("order create: %w", err)
	}
	o := m.toDomain()
	return &o, nil
}

// GetByID vraća nalog po PK. Vraća ErrOrderNotFound kada red ne postoji.
func (r *orderRepository) GetByID(ctx context.Context, id int64) (*trading.Order, error) {
	var m orderModel
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, trading.ErrOrderNotFound
		}
		return nil, fmt.Errorf("order get by id: %w", err)
	}
	o := m.toDomain()
	return &o, nil
}

// UpdateStatus atomično menja status i opcionog odobravaoca naloga.
func (r *orderRepository) UpdateStatus(ctx context.Context, id int64, status trading.OrderStatus, approvedBy *int64) (*trading.Order, error) {
	updates := map[string]interface{}{
		"status":        string(status),
		"last_modified": gorm.Expr("NOW()"),
	}
	if approvedBy != nil {
		updates["approved_by"] = *approvedBy
	}

	result := r.db.WithContext(ctx).
		Model(&orderModel{}).
		Where("id = ?", id).
		Updates(updates)
	if result.Error != nil {
		return nil, fmt.Errorf("order update status: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, trading.ErrOrderNotFound
	}
	return r.GetByID(ctx, id)
}

// UpdateRemainingPortions smanjuje remaining_portions i postavlja is_done.
func (r *orderRepository) UpdateRemainingPortions(ctx context.Context, id int64, remaining int32, isDone bool) (*trading.Order, error) {
	result := r.db.WithContext(ctx).
		Model(&orderModel{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"remaining_portions": remaining,
			"is_done":            isDone,
			"last_modified":      gorm.Expr("NOW()"),
		})
	if result.Error != nil {
		return nil, fmt.Errorf("order update remaining portions: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, trading.ErrOrderNotFound
	}
	return r.GetByID(ctx, id)
}

// ListByUserID vraća sve naloge korisnika, od najnovijeg.
// Ako je statusFilter != nil, rezultati se filtriraju po statusu.
func (r *orderRepository) ListByUserID(ctx context.Context, userID int64, statusFilter *trading.OrderStatus) ([]trading.Order, error) {
	q := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("created_at DESC")
	if statusFilter != nil {
		q = q.Where("status = ?", string(*statusFilter))
	}
	var models []orderModel
	if err := q.Find(&models).Error; err != nil {
		return nil, fmt.Errorf("order list by user: %w", err)
	}
	return modelsToOrders(models), nil
}

// ListByStatus vraća sve naloge sa datim statusom (nil = svi statusi).
func (r *orderRepository) ListByStatus(ctx context.Context, status *trading.OrderStatus) ([]trading.Order, error) {
	q := r.db.WithContext(ctx).Order("created_at DESC")
	if status != nil {
		q = q.Where("status = ?", string(*status))
	}
	var models []orderModel
	if err := q.Find(&models).Error; err != nil {
		return nil, fmt.Errorf("order list by status: %w", err)
	}
	return modelsToOrders(models), nil
}

// ListActiveByListing vraća sve APPROVED, nezavršene naloge za datu hartiju.
func (r *orderRepository) ListActiveByListing(ctx context.Context, listingID int64) ([]trading.Order, error) {
	var models []orderModel
	err := r.db.WithContext(ctx).
		Where("listing_id = ? AND status = 'APPROVED' AND is_done = FALSE", listingID).
		Find(&models).Error
	if err != nil {
		return nil, fmt.Errorf("order list active by listing: %w", err)
	}
	return modelsToOrders(models), nil
}

// CreateTransaction beleži jedan parcijalni izvršaj (fill chunk).
func (r *orderRepository) CreateTransaction(ctx context.Context, orderID int64, qty int32, price decimal.Decimal) (*trading.OrderTransaction, error) {
	m := orderTransactionModel{
		OrderID:          orderID,
		ExecutedQuantity: qty,
		ExecutedPrice:    price.String(),
		ExecutionTime:    time.Now().UTC(),
	}
	if err := r.db.WithContext(ctx).Create(&m).Error; err != nil {
		return nil, fmt.Errorf("order transaction create: %w", err)
	}
	ot := m.toDomain()
	return &ot, nil
}

// GetTransactionsByOrderID vraća sve fill zapise za nalog, od najstarijeg.
func (r *orderRepository) GetTransactionsByOrderID(ctx context.Context, orderID int64) ([]trading.OrderTransaction, error) {
	var models []orderTransactionModel
	err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).
		Order("execution_time ASC").
		Find(&models).Error
	if err != nil {
		return nil, fmt.Errorf("order get transactions: %w", err)
	}
	result := make([]trading.OrderTransaction, len(models))
	for i, m := range models {
		result[i] = m.toDomain()
	}
	return result, nil
}

// MarkDone atomično postavlja status=DONE, is_done=true, remaining_portions=0.
func (r *orderRepository) MarkDone(ctx context.Context, id int64) (*trading.Order, error) {
	result := r.db.WithContext(ctx).
		Model(&orderModel{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":             "DONE",
			"is_done":            true,
			"remaining_portions": 0,
			"last_modified":      gorm.Expr("NOW()"),
		})
	if result.Error != nil {
		return nil, fmt.Errorf("order mark done: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, trading.ErrOrderNotFound
	}
	return r.GetByID(ctx, id)
}

// Cancel atomično postavlja status=CANCELED, remaining_portions=0, is_done=true.
func (r *orderRepository) Cancel(ctx context.Context, id int64) (*trading.Order, error) {
	result := r.db.WithContext(ctx).
		Model(&orderModel{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":             "CANCELED",
			"is_done":            true,
			"remaining_portions": 0,
			"last_modified":      gorm.Expr("NOW()"),
		})
	if result.Error != nil {
		return nil, fmt.Errorf("order cancel: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, trading.ErrOrderNotFound
	}
	return r.GetByID(ctx, id)
}

// ─── Helper ───────────────────────────────────────────────────────────────────

func modelsToOrders(models []orderModel) []trading.Order {
	result := make([]trading.Order, len(models))
	for i, m := range models {
		result[i] = m.toDomain()
	}
	return result
}
