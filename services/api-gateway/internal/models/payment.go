package models

import "time"

type Payment struct {
	ID             string    `json:"id"`
	Amount         float64   `json:"amount"`
	Currency       string    `json:"currency"`
	CustomerID     string    `json:"customer_id"`
	MerchantID     string    `json:"merchant_id"`
	Status         string    `json:"status"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

type CreatePaymentRequest struct {
	Amount     float64 `json:"amount" binding:"required"`
	Currency   string  `json:"currency" binding:"required"`
	CustomerID string  `json:"customer_id" binding:"required"`
	MerchantID string  `json:"merchant_id" binding:"required"`
}
