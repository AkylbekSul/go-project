package interfaces

import (
	"context"

	"github.com/akylbek/payment-system/api-gateway/internal/models"
)

// PaymentRepository defines the contract for payment data access
type PaymentRepository interface {
	Create(ctx context.Context, payment *models.Payment) error
	GetByID(ctx context.Context, id string) (*models.Payment, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*models.Payment, error)
	UpdateStatus(ctx context.Context, id, status string) error
}
