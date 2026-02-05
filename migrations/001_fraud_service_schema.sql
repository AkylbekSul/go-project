-- Fraud Service Database Schema
-- Version: 001
-- Description: Schema for Fraud Detection service

-- =====================================================
-- FRAUD SERVICE TABLES
-- =====================================================

-- Fraud rules
CREATE TABLE IF NOT EXISTS fraud_rules (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    rule_type VARCHAR(50) NOT NULL,
    max_amount DECIMAL(15,2),
    max_per_hour INTEGER,
    max_per_day INTEGER,
    countries_blacklist TEXT[],
    countries_whitelist TEXT[],
    priority INTEGER DEFAULT 0,
    active BOOLEAN DEFAULT TRUE,
    description TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Composite index for active rules ordered by priority
CREATE INDEX IF NOT EXISTS idx_fraud_rules_active_priority ON fraud_rules(active, priority DESC) 
    WHERE active = TRUE;

-- Fraud decisions
CREATE TABLE IF NOT EXISTS fraud_decisions (
    id BIGSERIAL PRIMARY KEY,
    payment_id VARCHAR(255) NOT NULL,
    customer_id VARCHAR(255) NOT NULL,
    amount DECIMAL(15,2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'USD',
    decision VARCHAR(50) NOT NULL,
    reason TEXT,
    risk_score INTEGER CHECK (risk_score >= 0 AND risk_score <= 100),
    rules_triggered TEXT[],
    metadata JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Primary lookup by payment_id
CREATE INDEX IF NOT EXISTS idx_fraud_decisions_payment_id ON fraud_decisions(payment_id);
-- Composite for customer fraud history
CREATE INDEX IF NOT EXISTS idx_fraud_decisions_customer_created ON fraud_decisions(customer_id, created_at DESC);

-- Velocity counters (backup for Redis)
CREATE TABLE IF NOT EXISTS velocity_counters (
    id BIGSERIAL PRIMARY KEY,
    entity_type VARCHAR(50) NOT NULL,
    entity_id VARCHAR(255) NOT NULL,
    counter_type VARCHAR(50) NOT NULL,
    count INTEGER DEFAULT 0,
    window_start TIMESTAMP NOT NULL,
    window_end TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_velocity_counters_entity ON velocity_counters(entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_velocity_counters_window ON velocity_counters(window_start, window_end);

-- =====================================================
-- FUNCTIONS AND TRIGGERS
-- =====================================================

-- Function to update updated_at timestamp
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Triggers for updated_at
CREATE TRIGGER update_fraud_rules_updated_at BEFORE UPDATE ON fraud_rules
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_velocity_counters_updated_at BEFORE UPDATE ON velocity_counters
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- =====================================================
-- INITIAL DATA
-- =====================================================

-- Insert default fraud rules
INSERT INTO fraud_rules (name, rule_type, max_amount, description, priority)
VALUES 
    ('High Amount Check', 'amount_limit', 10000.00, 'Flag payments over $10,000', 100),
    ('Medium Amount Check', 'amount_limit', 5000.00, 'Review payments over $5,000', 50)
ON CONFLICT DO NOTHING;

INSERT INTO fraud_rules (name, rule_type, max_per_hour, description, priority)
VALUES 
    ('Velocity Check Hourly', 'velocity', NULL, 'Max 5 payments per hour per customer', 80)
ON CONFLICT DO NOTHING;

-- =====================================================
-- VIEWS FOR ANALYTICS
-- =====================================================

-- Fraud statistics view
CREATE OR REPLACE VIEW fraud_statistics AS
SELECT 
    DATE(created_at) as date,
    decision,
    COUNT(*) as count,
    AVG(risk_score) as avg_risk_score,
    SUM(amount) as total_amount
FROM fraud_decisions
GROUP BY DATE(created_at), decision
ORDER BY date DESC;

-- =====================================================
-- COMMENTS
-- =====================================================

COMMENT ON TABLE fraud_rules IS 'Configuration rules for fraud detection';
COMMENT ON TABLE fraud_decisions IS 'Fraud detection results for each payment';
COMMENT ON TABLE velocity_counters IS 'Backup velocity counters (primary storage in Redis)';

-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_migrations (
    version VARCHAR(50) PRIMARY KEY,
    applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO schema_migrations (version) VALUES ('001_fraud_service_schema') ON CONFLICT DO NOTHING;
