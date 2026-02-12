package repository

import (
	"context"
	"database/sql"

	"github.com/akylbek/payment-system/api-gateway/internal/models"
)

type PaymentRepository struct {
	db *sql.DB
}

func NewPaymentRepository(db *sql.DB) *PaymentRepository {
	return &PaymentRepository{db: db}
}

func (r *PaymentRepository) InitDB() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS payments (
			id VARCHAR(255) PRIMARY KEY,
			amount DECIMAL(15,2) NOT NULL,
			currency VARCHAR(3) NOT NULL,
			customer_id VARCHAR(255) NOT NULL,
			merchant_id VARCHAR(255) NOT NULL,
			status VARCHAR(50) NOT NULL,
			idempotency_key VARCHAR(255) UNIQUE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payments_customer_id ON payments(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_payments_idempotency_key ON payments(idempotency_key)`,
	}

	for _, query := range queries {
		if _, err := r.db.Exec(query); err != nil {
			return err
		}
	}

	return nil
}

func (r *PaymentRepository) Create(ctx context.Context, payment *models.Payment) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO payments (id, amount, currency, customer_id, merchant_id, status, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, payment.ID, payment.Amount, payment.Currency, payment.CustomerID,
		payment.MerchantID, payment.Status, payment.IdempotencyKey)
	return err
}

func (r *PaymentRepository) GetByID(ctx context.Context, id string) (*models.Payment, error) {
	var payment models.Payment
	err := r.db.QueryRowContext(ctx, `
		SELECT id, amount, currency, customer_id, merchant_id, status, idempotency_key, created_at
		FROM payments WHERE id = $1
	`, id).Scan(&payment.ID, &payment.Amount, &payment.Currency, &payment.CustomerID,
		&payment.MerchantID, &payment.Status, &payment.IdempotencyKey, &payment.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &payment, nil
}

func (r *PaymentRepository) GetByIdempotencyKey(ctx context.Context, key string) (*models.Payment, error) {
	var payment models.Payment
	err := r.db.QueryRowContext(ctx, `
		SELECT id, amount, currency, customer_id, merchant_id, status, idempotency_key, created_at
		FROM payments WHERE idempotency_key = $1
	`, key).Scan(&payment.ID, &payment.Amount, &payment.Currency, &payment.CustomerID,
		&payment.MerchantID, &payment.Status, &payment.IdempotencyKey, &payment.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &payment, nil
}

func (r *PaymentRepository) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE payments SET status = $1, updated_at = NOW() WHERE id = $2`, status, id)
	return err
}
