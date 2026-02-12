package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/akylbek/payment-system/api-gateway/internal/interfaces"
	"github.com/akylbek/payment-system/api-gateway/internal/models"
	"github.com/akylbek/payment-system/api-gateway/internal/telemetry"
)

type PaymentHandler struct {
	repo        interfaces.PaymentRepository
	redisClient *redis.Client
	kafkaWriter *kafka.Writer
}

func NewPaymentHandler(repo interfaces.PaymentRepository, redisClient *redis.Client, kafkaWriter *kafka.Writer) *PaymentHandler {
	return &PaymentHandler{
		repo:        repo,
		redisClient: redisClient,
		kafkaWriter: kafkaWriter,
	}
}

func (h *PaymentHandler) CreatePayment(c *gin.Context) {
	ctx := c.Request.Context()
	span := trace.SpanFromContext(ctx)

	var req models.CreatePaymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		telemetry.Logger.Warn("Invalid payment request", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	idempotencyKey := c.GetString("idempotency_key")

	payment := models.Payment{
		ID:             uuid.New().String(),
		Amount:         req.Amount,
		Currency:       req.Currency,
		CustomerID:     req.CustomerID,
		MerchantID:     req.MerchantID,
		Status:         "NEW",
		IdempotencyKey: idempotencyKey,
		CreatedAt:      time.Now(),
	}

	telemetry.Logger.Info("Creating payment",
		zap.String("payment_id", payment.ID),
		zap.String("customer_id", payment.CustomerID),
		zap.Float64("amount", payment.Amount),
		zap.String("trace_id", span.SpanContext().TraceID().String()),
	)

	// Save to database
	if err := h.repo.Create(ctx, &payment); err != nil {
		telemetry.Logger.Error("Failed to save payment to database",
			zap.String("payment_id", payment.ID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create payment"})
		return
	}

	// Cache in Redis
	paymentJSON, _ := json.Marshal(payment)
	h.redisClient.Set(ctx, fmt.Sprintf("idempotency:%s", idempotencyKey), paymentJSON, 24*time.Hour)

	// Publish to Kafka
	event := map[string]interface{}{
		"payment_id":  payment.ID,
		"amount":      payment.Amount,
		"currency":    payment.Currency,
		"customer_id": payment.CustomerID,
		"merchant_id": payment.MerchantID,
		"status":      payment.Status,
		"created_at":  payment.CreatedAt,
	}
	eventJSON, _ := json.Marshal(event)

	if err := h.kafkaWriter.WriteMessages(ctx, kafka.Message{
		Key:   []byte(payment.ID),
		Value: eventJSON,
	}); err != nil {
		telemetry.Logger.Error("Failed to publish payment event to Kafka",
			zap.String("payment_id", payment.ID),
			zap.Error(err),
		)
	}

	telemetry.Logger.Info("Payment created successfully",
		zap.String("payment_id", payment.ID),
	)

	c.JSON(http.StatusCreated, payment)
}

func (h *PaymentHandler) GetPayment(c *gin.Context) {
	id := c.Param("id")

	payment, err := h.repo.GetByID(c.Request.Context(), id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch payment"})
		return
	}

	c.JSON(http.StatusOK, payment)
}

func (h *PaymentHandler) ConfirmPayment(c *gin.Context) {
	id := c.Param("id")

	if err := h.repo.UpdateStatus(c.Request.Context(), id, "CONFIRMED"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm payment"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "confirmed", "payment_id": id})
}
