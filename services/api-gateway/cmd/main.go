package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/akylbek/payment-system/api-gateway/internal/telemetry"
)

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

var (
	db          *sql.DB
	redisClient *redis.Client
	kafkaWriter *kafka.Writer
)

func main() {
	var err error

	// Initialize telemetry
	if err := telemetry.InitTelemetry("api-gateway"); err != nil {
		panic(fmt.Sprintf("Failed to initialize telemetry: %v", err))
	}
	defer telemetry.Shutdown(context.Background())

	telemetry.Logger.Info("Starting API Gateway")

	// Connect to PostgreSQL
	dbURL := os.Getenv("DATABASE_URL")
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		telemetry.Logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer db.Close()

	// Initialize database
	if err := initDB(); err != nil {
		telemetry.Logger.Fatal("Failed to initialize database", zap.Error(err))
	}

	// Connect to Redis
	redisURL := os.Getenv("REDIS_URL")
	redisClient = redis.NewClient(&redis.Options{
		Addr: redisURL,
	})

	// Connect to Kafka
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	kafkaWriter = &kafka.Writer{
		Addr:     kafka.TCP(kafkaBrokers),
		Topic:    "payment.created",
		Balancer: &kafka.LeastBytes{},
	}
	defer kafkaWriter.Close()

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(telemetry.TracingMiddleware())

	// Prometheus metrics endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "api-gateway"})
	})

	r.POST("/payments", idempotencyMiddleware(), createPayment)
	r.GET("/payments/:id", getPayment)
	r.POST("/payments/:id/confirm", confirmPayment)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	// Setup HTTP server
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		telemetry.Logger.Info("API Gateway starting", zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			telemetry.Logger.Fatal("Failed to start server", zap.Error(err))
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	telemetry.Logger.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := srv.Shutdown(ctx); err != nil {
		telemetry.Logger.Error("Server forced to shutdown", zap.Error(err))
	}
	
	telemetry.Logger.Info("Server exited")
}

func initDB() error {
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
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}

	return nil
}

func idempotencyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("Idempotency-Key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key header is required"})
			c.Abort()
			return
		}

		ctx := context.Background()

		// Check Redis cache
		cached, err := redisClient.Get(ctx, fmt.Sprintf("idempotency:%s", key)).Result()
		if err == nil {
			var payment Payment
			if err := json.Unmarshal([]byte(cached), &payment); err == nil {
				c.JSON(http.StatusOK, payment)
				c.Abort()
				return
			}
		}

		// Check database
		var payment Payment
		err = db.QueryRow(`
			SELECT id, amount, currency, customer_id, merchant_id, status, idempotency_key, created_at
			FROM payments WHERE idempotency_key = $1
		`, key).Scan(&payment.ID, &payment.Amount, &payment.Currency, &payment.CustomerID,
			&payment.MerchantID, &payment.Status, &payment.IdempotencyKey, &payment.CreatedAt)

		if err == nil {
			c.JSON(http.StatusOK, payment)
			c.Abort()
			return
		}

		c.Set("idempotency_key", key)
		c.Next()
	}
}

func createPayment(c *gin.Context) {
	ctx := c.Request.Context()
	span := trace.SpanFromContext(ctx)

	var req CreatePaymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		telemetry.Logger.Warn("Invalid payment request", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	idempotencyKey := c.GetString("idempotency_key")

	payment := Payment{
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
	_, err := db.ExecContext(ctx, `
		INSERT INTO payments (id, amount, currency, customer_id, merchant_id, status, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, payment.ID, payment.Amount, payment.Currency, payment.CustomerID,
		payment.MerchantID, payment.Status, payment.IdempotencyKey)

	if err != nil {
		telemetry.Logger.Error("Failed to save payment to database", 
			zap.String("payment_id", payment.ID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create payment"})
		return
	}

	// Cache in Redis
	paymentJSON, _ := json.Marshal(payment)
	redisClient.Set(ctx, fmt.Sprintf("idempotency:%s", idempotencyKey), paymentJSON, 24*time.Hour)

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

	if err := kafkaWriter.WriteMessages(ctx, kafka.Message{
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

func getPayment(c *gin.Context) {
	id := c.Param("id")

	var payment Payment
	err := db.QueryRow(`
		SELECT id, amount, currency, customer_id, merchant_id, status, idempotency_key, created_at
		FROM payments WHERE id = $1
	`, id).Scan(&payment.ID, &payment.Amount, &payment.Currency, &payment.CustomerID,
		&payment.MerchantID, &payment.Status, &payment.IdempotencyKey, &payment.CreatedAt)

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

func confirmPayment(c *gin.Context) {
	id := c.Param("id")

	_, err := db.Exec(`UPDATE payments SET status = 'CONFIRMED', updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm payment"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "confirmed", "payment_id": id})
}
