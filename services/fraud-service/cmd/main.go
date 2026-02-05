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
	"go.uber.org/zap"

	"github.com/akylbek/payment-system/fraud-service/internal/telemetry"
)

type FraudCheckRequest struct {
	PaymentID  string  `json:"payment_id"`
	Amount     float64 `json:"amount"`
	CustomerID string  `json:"customer_id"`
}

type FraudCheckResponse struct {
	Decision string `json:"decision"` // approve, deny, manual_review
	Reason   string `json:"reason"`
}

type FraudRule struct {
	ID          int
	Name        string
	MaxAmount   float64
	MaxPerHour  int
	Description string
}

var (
	db          *sql.DB
	redisClient *redis.Client
	nc          *nats.Conn
)

func main() {
	var err error

	// Initialize telemetry
	if err := telemetry.InitTelemetry("fraud-service"); err != nil {
		panic(fmt.Sprintf("Failed to initialize telemetry: %v", err))
	}
	defer telemetry.Shutdown(context.Background())

	telemetry.Logger.Info("Starting Fraud Service")

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

	// Subscribe to fraud check requests
	nc.Subscribe("fraud.check", handleFraudCheckRequest)

	telemetry.Logger.Info("Subscribed to fraud.check on NATS")

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(telemetry.TracingMiddleware())

	// Prometheus metrics endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "fraud-service"})
	})

	r.GET("/fraud/stats", getFraudStats)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8083"
	}

	// Setup HTTP server
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		telemetry.Logger.Info("Fraud Service starting", zap.String("port", port))
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
		`CREATE TABLE IF NOT EXISTS fraud_rules (
			id SERIAL PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			max_amount DECIMAL(15,2),
			max_per_hour INTEGER,
			description TEXT,
			active BOOLEAN DEFAULT true,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS fraud_decisions (
			id SERIAL PRIMARY KEY,
			payment_id VARCHAR(255) NOT NULL,
			customer_id VARCHAR(255) NOT NULL,
			amount DECIMAL(15,2) NOT NULL,
			decision VARCHAR(50) NOT NULL,
			reason TEXT,
			risk_score INTEGER,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fraud_decisions_payment_id ON fraud_decisions(payment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_fraud_decisions_customer_id ON fraud_decisions(customer_id)`,
	}

	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}

	// Insert default rules
	db.Exec(`
		INSERT INTO fraud_rules (name, max_amount, max_per_hour, description)
		VALUES 
			('High Amount Check', 10000.00, NULL, 'Deny payments over $10,000'),
			('Velocity Check', NULL, 5, 'Max 5 payments per hour per customer')
		ON CONFLICT DO NOTHING
	`)

	return nil
}

func handleFraudCheckRequest(msg *nats.Msg) {
	var req FraudCheckRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		telemetry.Logger.Error("Error unmarshaling fraud check request", zap.Error(err))
		return
	}

	telemetry.Logger.Info("Fraud check request",
		zap.String("payment_id", req.PaymentID),
		zap.Float64("amount", req.Amount),
		zap.String("customer_id", req.CustomerID),
	)

	ctx := context.Background()
	decision := checkFraud(ctx, &req)

	// Save decision to database
	_, err := db.ExecContext(ctx, `
		INSERT INTO fraud_decisions (payment_id, customer_id, amount, decision, reason, risk_score)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, req.PaymentID, req.CustomerID, req.Amount, decision.Decision, decision.Reason, calculateRiskScore(&req))

	if err != nil {
		telemetry.Logger.Error("Error saving fraud decision",
			zap.String("payment_id", req.PaymentID),
			zap.Error(err),
		)
	}

	// Send response back via NATS
	respJSON, _ := json.Marshal(decision)
	msg.Respond(respJSON)

	telemetry.Logger.Info("Fraud check completed",
		zap.String("payment_id", req.PaymentID),
		zap.String("decision", decision.Decision),
		zap.String("reason", decision.Reason),
	)
}

func checkFraud(ctx context.Context, req *FraudCheckRequest) *FraudCheckResponse {
	// Rule 1: High amount check
	if req.Amount > 10000 {
		return &FraudCheckResponse{
			Decision: "deny",
			Reason:   "Amount exceeds $10,000 limit",
		}
	}

	// Rule 2: Velocity check (max 5 payments per hour)
	velocityKey := "fraud:velocity:" + req.CustomerID
	count, err := redisClient.Incr(ctx, velocityKey).Result()
	if err == nil {
		if count == 1 {
			redisClient.Expire(ctx, velocityKey, time.Hour)
		}
		if count > 5 {
			return &FraudCheckResponse{
				Decision: "deny",
				Reason:   "Too many payments in the last hour (velocity check failed)",
			}
		}
	}

	// Rule 3: Random manual review (10% of transactions)
	// In real system, this would be based on more sophisticated ML models
	if req.Amount > 5000 {
		return &FraudCheckResponse{
			Decision: "manual_review",
			Reason:   "High-value transaction requires manual review",
		}
	}

	return &FraudCheckResponse{
		Decision: "approve",
		Reason:   "All fraud checks passed",
	}
}

func calculateRiskScore(req *FraudCheckRequest) int {
	// Simple risk scoring logic
	score := 0

	if req.Amount > 1000 {
		score += 30
	}
	if req.Amount > 5000 {
		score += 50
	}

	return score
}

func getFraudStats(c *gin.Context) {
	var stats struct {
		TotalChecks    int `json:"total_checks"`
		ApprovedCount  int `json:"approved_count"`
		DeniedCount    int `json:"denied_count"`
		ManualReview   int `json:"manual_review_count"`
		AvgRiskScore   int `json:"avg_risk_score"`
	}

	db.QueryRow(`
		SELECT 
			COUNT(*) as total,
			COUNT(CASE WHEN decision = 'approve' THEN 1 END) as approved,
			COUNT(CASE WHEN decision = 'deny' THEN 1 END) as denied,
			COUNT(CASE WHEN decision = 'manual_review' THEN 1 END) as manual_review,
			COALESCE(AVG(risk_score), 0) as avg_risk_score
		FROM fraud_decisions
	`).Scan(&stats.TotalChecks, &stats.ApprovedCount, &stats.DeniedCount,
		&stats.ManualReview, &stats.AvgRiskScore)

	c.JSON(http.StatusOK, stats)
}
