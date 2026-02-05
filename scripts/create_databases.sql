-- Create databases for each microservice
-- This script runs automatically when PostgreSQL container starts

-- API Gateway database
CREATE DATABASE api_gateway_db;

-- Payment Orchestrator database
CREATE DATABASE payment_orchestrator_db;

-- Fraud Service database
CREATE DATABASE fraud_service_db;

-- Ledger Service database
CREATE DATABASE ledger_service_db;
