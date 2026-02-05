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
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/akylbek/payment-system/ledger-service/internal/telemetry"
)

type PaymentStateChangedEvent struct {
	PaymentID     string    `json:"payment_id"`
	State         string    `json:"state"`
	PreviousState string    `json:"previous_state"`
	Timestamp     time.Time `json:"timestamp"`
}

type LedgerEntry struct {
	ID         int64
	AccountID  string
	PaymentID  string
	Type       string // debit or credit
	Amount     decimal.Decimal
	Balance    decimal.Decimal
	CreatedAt  time.Time
}

type Account struct {
	ID        string
	Type      string // merchant, platform, customer
	Balance   decimal.Decimal
	CreatedAt time.Time
}

var db *sql.DB

func main() {
	var err error

	// Initialize telemetry
	if err := telemetry.InitTelemetry("ledger-service"); err != nil {
		panic(fmt.Sprintf("Failed to initialize telemetry: %v", err))
	}
	defer telemetry.Shutdown(context.Background())

	telemetry.Logger.Info("Starting Ledger Service")

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

	// Start Kafka consumer
	go consumePaymentStateChanges()

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(telemetry.TracingMiddleware())

	// Prometheus metrics endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "ledger-service"})
	})

	r.GET("/accounts/:id/balance", getAccountBalance)
	r.GET("/accounts/:id/entries", getAccountEntries)
	r.GET("/payments/:id/entries", getPaymentEntries)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8084"
	}

	// Setup HTTP server
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		telemetry.Logger.Info("Ledger Service starting", zap.String("port", port))
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
		`CREATE TABLE IF NOT EXISTS accounts (
			id VARCHAR(255) PRIMARY KEY,
			type VARCHAR(50) NOT NULL,
			balance DECIMAL(20,2) DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_type ON accounts(type)`,
		
		`CREATE TABLE IF NOT EXISTS ledger_entries (
			id BIGSERIAL PRIMARY KEY,
			account_id VARCHAR(255) NOT NULL,
			payment_id VARCHAR(255) NOT NULL,
			type VARCHAR(50) NOT NULL,
			amount DECIMAL(20,2) NOT NULL,
			balance DECIMAL(20,2) NOT NULL,
			idempotency_key VARCHAR(255) UNIQUE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_entries_account_id ON ledger_entries(account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_entries_payment_id ON ledger_entries(payment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_entries_idempotency_key ON ledger_entries(idempotency_key)`,
	}

	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}

	// Create default platform account
	db.Exec(`
		INSERT INTO accounts (id, type, balance)
		VALUES ('platform-001', 'platform', 0)
		ON CONFLICT (id) DO NOTHING
	`)

	return nil
}

func consumePaymentStateChanges() {
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{kafkaBrokers},
		Topic:    "payment.state.changed",
		GroupID:  "ledger-service",
		MinBytes: 10e3,
		MaxBytes: 10e6,
	})
	defer reader.Close()

	ctx := context.Background()

	telemetry.Logger.Info("Started consuming payment.state.changed events")

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			telemetry.Logger.Error("Error reading message from Kafka", zap.Error(err))
			continue
		}

		var event PaymentStateChangedEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			telemetry.Logger.Error("Error unmarshaling event", zap.Error(err))
			continue
		}

		// Only process SUCCEEDED state
		if event.State == "SUCCEEDED" {
			telemetry.Logger.Info("Processing ledger entry",
				zap.String("payment_id", event.PaymentID),
				zap.String("state", event.State),
			)
			if err := recordPaymentSuccess(ctx, &event); err != nil {
				telemetry.Logger.Error("Error recording payment success",
					zap.String("payment_id", event.PaymentID),
					zap.Error(err),
				)
			}
		}
	}
}

func recordPaymentSuccess(ctx context.Context, event *PaymentStateChangedEvent) error {
	// Get payment details (in real system, this would come from the event or API call)
	// For now, we'll create mock entries
	
	merchantAccount := "merchant-" + event.PaymentID[:8]
	platformAccount := "platform-001"
	
	// Ensure merchant account exists
	db.Exec(`
		INSERT INTO accounts (id, type, balance)
		VALUES ($1, 'merchant', 0)
		ON CONFLICT (id) DO NOTHING
	`, merchantAccount)

	// Mock payment amount (in real system, this would come from payment data)
	amount := decimal.NewFromFloat(100.00)
	platformFee := decimal.NewFromFloat(2.00)
	merchantAmount := amount.Sub(platformFee)

	idempotencyKey := event.PaymentID + "-" + event.State

	// Record double-entry bookkeeping
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Credit merchant account
	if err := recordEntry(tx, merchantAccount, event.PaymentID, "credit", merchantAmount, idempotencyKey+"-merchant"); err != nil {
		return err
	}

	// Credit platform account (fee)
	if err := recordEntry(tx, platformAccount, event.PaymentID, "credit", platformFee, idempotencyKey+"-platform"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	telemetry.Logger.Info("Recorded ledger entries",
		zap.String("payment_id", event.PaymentID),
		zap.String("merchant_amount", merchantAmount.String()),
		zap.String("platform_fee", platformFee.String()),
	)

	return nil
}

func recordEntry(tx *sql.Tx, accountID, paymentID, entryType string, amount decimal.Decimal, idempotencyKey string) error {
	// Get current balance
	var balance decimal.Decimal
	err := tx.QueryRow(`
		SELECT balance FROM accounts WHERE id = $1 FOR UPDATE
	`, accountID).Scan(&balance)

	if err != nil {
		return err
	}

	// Calculate new balance
	newBalance := balance
	if entryType == "credit" {
		newBalance = balance.Add(amount)
	} else {
		newBalance = balance.Sub(amount)
	}

	// Insert ledger entry (idempotency check)
	_, err = tx.Exec(`
		INSERT INTO ledger_entries (account_id, payment_id, type, amount, balance, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, accountID, paymentID, entryType, amount, newBalance, idempotencyKey)

	if err != nil {
		return err
	}

	// Update account balance
	_, err = tx.Exec(`
		UPDATE accounts SET balance = $1, updated_at = NOW()
		WHERE id = $2
	`, newBalance, accountID)

	return err
}

func getAccountBalance(c *gin.Context) {
	accountID := c.Param("id")

	var account Account
	err := db.QueryRow(`
		SELECT id, type, balance, created_at
		FROM accounts WHERE id = $1
	`, accountID).Scan(&account.ID, &account.Type, &account.Balance, &account.CreatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Account not found"})
		return
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch account"})
		return
	}

	c.JSON(http.StatusOK, account)
}

func getAccountEntries(c *gin.Context) {
	accountID := c.Param("id")

	rows, err := db.Query(`
		SELECT id, account_id, payment_id, type, amount, balance, created_at
		FROM ledger_entries
		WHERE account_id = $1
		ORDER BY created_at DESC
		LIMIT 100
	`, accountID)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch entries"})
		return
	}
	defer rows.Close()

	var entries []LedgerEntry
	for rows.Next() {
		var entry LedgerEntry
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.PaymentID,
			&entry.Type, &entry.Amount, &entry.Balance, &entry.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	c.JSON(http.StatusOK, entries)
}

func getPaymentEntries(c *gin.Context) {
	paymentID := c.Param("id")

	rows, err := db.Query(`
		SELECT id, account_id, payment_id, type, amount, balance, created_at
		FROM ledger_entries
		WHERE payment_id = $1
		ORDER BY created_at ASC
	`, paymentID)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch entries"})
		return
	}
	defer rows.Close()

	var entries []LedgerEntry
	for rows.Next() {
		var entry LedgerEntry
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.PaymentID,
			&entry.Type, &entry.Amount, &entry.Balance, &entry.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	c.JSON(http.StatusOK, entries)
}
