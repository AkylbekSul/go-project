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
	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"github.com/akylbek/payment-system/payment-orchestrator/internal/telemetry"
)

type PaymentState string

const (
	StateNew         PaymentState = "NEW"
	StateAuthPending PaymentState = "AUTH_PENDING"
	StateAuthorized  PaymentState = "AUTHORIZED"
	StateCaptured    PaymentState = "CAPTURED"
	StateSucceeded   PaymentState = "SUCCEEDED"
	StateFailed      PaymentState = "FAILED"
	StateCanceled    PaymentState = "CANCELED"
)

type PaymentEvent struct {
	PaymentID  string    `json:"payment_id"`
	Amount     float64   `json:"amount"`
	Currency   string    `json:"currency"`
	CustomerID string    `json:"customer_id"`
	MerchantID string    `json:"merchant_id"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

type FraudCheckRequest struct {
	PaymentID  string  `json:"payment_id"`
	Amount     float64 `json:"amount"`
	CustomerID string  `json:"customer_id"`
}

type FraudCheckResponse struct {
	Decision string `json:"decision"` // approve, deny, manual_review
	Reason   string `json:"reason"`
}

var (
	db          *sql.DB
	redisClient *redis.Client
	nc          *nats.Conn
	kafkaWriter *kafka.Writer
)

func main() {
	var err error

	// Initialize telemetry
	if err := telemetry.InitTelemetry("payment-orchestrator"); err != nil {
		panic(fmt.Sprintf("Failed to initialize telemetry: %v", err))
	}
	defer telemetry.Shutdown(context.Background())

	telemetry.Logger.Info("Starting Payment Orchestrator")

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

	// Connect to NATS
	natsURL := os.Getenv("NATS_URL")
	nc, err = nats.Connect(natsURL)
	if err != nil {
		telemetry.Logger.Fatal("Failed to connect to NATS", zap.Error(err))
	}
	defer nc.Close()

	// Connect to Kafka
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	kafkaWriter = &kafka.Writer{
		Addr:     kafka.TCP(kafkaBrokers),
		Topic:    "payment.state.changed",
		Balancer: &kafka.LeastBytes{},
	}
	defer kafkaWriter.Close()

	// Start Kafka consumer
	go consumePaymentEvents()

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(telemetry.TracingMiddleware())

	// Prometheus metrics endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "payment-orchestrator"})
	})

	r.GET("/payments/:id/state", getPaymentState)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	// Setup HTTP server
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		telemetry.Logger.Info("Payment Orchestrator starting", zap.String("port", port))
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
		`CREATE TABLE IF NOT EXISTS payment_states (
			payment_id VARCHAR(255) PRIMARY KEY,
			state VARCHAR(50) NOT NULL,
			previous_state VARCHAR(50),
			fraud_decision VARCHAR(50),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_states_state ON payment_states(state)`,
	}

	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}

	return nil
}

func consumePaymentEvents() {
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{kafkaBrokers},
		Topic:    "payment.created",
		GroupID:  "payment-orchestrator",
		MinBytes: 10e3,
		MaxBytes: 10e6,
	})
	defer reader.Close()

	ctx := context.Background()

	telemetry.Logger.Info("Started consuming payment.created events")

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			telemetry.Logger.Error("Error reading message from Kafka", zap.Error(err))
			continue
		}

		var event PaymentEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			telemetry.Logger.Error("Error unmarshaling event", zap.Error(err))
			continue
		}

		telemetry.Logger.Info("Processing payment",
			zap.String("payment_id", event.PaymentID),
			zap.Float64("amount", event.Amount),
		)

		if err := processPayment(ctx, &event); err != nil {
			telemetry.Logger.Error("Error processing payment",
				zap.String("payment_id", event.PaymentID),
				zap.Error(err),
			)
		}
	}
}

func processPayment(ctx context.Context, event *PaymentEvent) error {
	// Acquire lock
	lockKey := fmt.Sprintf("payment_lock:%s", event.PaymentID)
	locked := redisClient.SetNX(ctx, lockKey, "1", 30*time.Second)
	if !locked.Val() {
		return fmt.Errorf("payment %s is already being processed", event.PaymentID)
	}
	defer redisClient.Del(ctx, lockKey)

	// Save initial state
	_, err := db.Exec(`
		INSERT INTO payment_states (payment_id, state, previous_state)
		VALUES ($1, $2, $3)
		ON CONFLICT (payment_id) DO NOTHING
	`, event.PaymentID, StateNew, "")

	if err != nil {
		return err
	}

	// Transition to AUTH_PENDING
	if err := transitionState(ctx, event.PaymentID, StateNew, StateAuthPending); err != nil {
		return err
	}

	// Check fraud via NATS
	fraudReq := FraudCheckRequest{
		PaymentID:  event.PaymentID,
		Amount:     event.Amount,
		CustomerID: event.CustomerID,
	}
	fraudReqJSON, _ := json.Marshal(fraudReq)

	msg, err := nc.Request("fraud.check", fraudReqJSON, 5*time.Second)
	if err != nil {
		telemetry.Logger.Warn("Fraud check timeout",
			zap.String("payment_id", event.PaymentID),
			zap.Error(err),
		)
		transitionState(ctx, event.PaymentID, StateAuthPending, StateFailed)
		return err
	}

	var fraudResp FraudCheckResponse
	if err := json.Unmarshal(msg.Data, &fraudResp); err != nil {
		return err
	}

	// Save fraud decision
	db.Exec(`UPDATE payment_states SET fraud_decision = $1 WHERE payment_id = $2`,
		fraudResp.Decision, event.PaymentID)

	if fraudResp.Decision == "approve" {
		transitionState(ctx, event.PaymentID, StateAuthPending, StateAuthorized)
		transitionState(ctx, event.PaymentID, StateAuthorized, StateCaptured)
		transitionState(ctx, event.PaymentID, StateCaptured, StateSucceeded)
	} else {
		transitionState(ctx, event.PaymentID, StateAuthPending, StateFailed)
	}

	return nil
}

func transitionState(ctx context.Context, paymentID string, from, to PaymentState) error {
	result, err := db.Exec(`
		UPDATE payment_states 
		SET state = $1, previous_state = $2, updated_at = NOW()
		WHERE payment_id = $3 AND state = $4
	`, to, from, paymentID, from)

	if err != nil {
		return err
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("invalid state transition from %s to %s for payment %s", from, to, paymentID)
	}

	// Publish state change event
	stateEvent := map[string]interface{}{
		"payment_id":     paymentID,
		"state":          to,
		"previous_state": from,
		"timestamp":      time.Now(),
	}
	eventJSON, _ := json.Marshal(stateEvent)

	kafkaWriter.WriteMessages(ctx, kafka.Message{
		Key:   []byte(paymentID),
		Value: eventJSON,
	})

	telemetry.Logger.Info("Payment state transition",
		zap.String("payment_id", paymentID),
		zap.String("from_state", string(from)),
		zap.String("to_state", string(to)),
	)

	return nil
}

func getPaymentState(c *gin.Context) {
	paymentID := c.Param("id")

	var state, previousState, fraudDecision string
	var createdAt, updatedAt time.Time

	err := db.QueryRow(`
		SELECT state, previous_state, fraud_decision, created_at, updated_at
		FROM payment_states WHERE payment_id = $1
	`, paymentID).Scan(&state, &previousState, &fraudDecision, &createdAt, &updatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Payment state not found"})
		return
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch payment state"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"payment_id":     paymentID,
		"state":          state,
		"previous_state": previousState,
		"fraud_decision": fraudDecision,
		"created_at":     createdAt,
		"updated_at":     updatedAt,
	})
}
