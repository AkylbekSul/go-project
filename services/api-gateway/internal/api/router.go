package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"

	"github.com/akylbek/payment-system/api-gateway/internal/handlers"
	"github.com/akylbek/payment-system/api-gateway/internal/interfaces"
	"github.com/akylbek/payment-system/api-gateway/internal/middleware"
	"github.com/akylbek/payment-system/api-gateway/internal/telemetry"
)

func NewRouter(paymentRepo interfaces.PaymentRepository, redisClient *redis.Client, kafkaWriter *kafka.Writer) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(telemetry.TracingMiddleware())

	// Prometheus metrics
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "api-gateway"})
	})

	// Payment routes
	paymentHandler := handlers.NewPaymentHandler(paymentRepo, redisClient, kafkaWriter)
	payments := r.Group("/payments")
	{
		payments.POST("", middleware.IdempotencyMiddleware(redisClient, paymentRepo), paymentHandler.CreatePayment)
		payments.GET("/:id", paymentHandler.GetPayment)
		payments.POST("/:id/confirm", paymentHandler.ConfirmPayment)
	}

	return r
}
