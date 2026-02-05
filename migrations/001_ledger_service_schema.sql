-- Ledger Service Database Schema
-- Version: 001
-- Description: Schema for Ledger/Accounting service

-- =====================================================
-- LEDGER SERVICE TABLES
-- =====================================================

-- Accounts (merchant, platform, customer accounts)
CREATE TABLE IF NOT EXISTS accounts (
    id VARCHAR(255) PRIMARY KEY,
    type VARCHAR(50) NOT NULL,
    entity_id VARCHAR(255),
    currency VARCHAR(3) DEFAULT 'USD',
    balance DECIMAL(20,2) DEFAULT 0 CHECK (balance >= 0),
    available_balance DECIMAL(20,2) DEFAULT 0 CHECK (available_balance >= 0),
    hold_balance DECIMAL(20,2) DEFAULT 0 CHECK (hold_balance >= 0),
    status VARCHAR(50) DEFAULT 'active',
    metadata JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Critical for looking up accounts by entity (merchant, customer, etc.)
CREATE INDEX IF NOT EXISTS idx_accounts_entity_type ON accounts(entity_id, type);

-- Ledger entries (double-entry bookkeeping)
CREATE TABLE IF NOT EXISTS ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    account_id VARCHAR(255) NOT NULL REFERENCES accounts(id),
    payment_id VARCHAR(255),
    entry_type VARCHAR(50) NOT NULL,
    amount DECIMAL(20,2) NOT NULL,
    balance DECIMAL(20,2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'USD',
    description TEXT,
    idempotency_key VARCHAR(255) UNIQUE,
    metadata JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Critical: ledger entries by account with time ordering
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account_created ON ledger_entries(account_id, created_at DESC);
-- Lookup by payment
CREATE INDEX IF NOT EXISTS idx_ledger_entries_payment_id ON ledger_entries(payment_id);

-- Settlement batches (for periodic settlements)
CREATE TABLE IF NOT EXISTS settlement_batches (
    id BIGSERIAL PRIMARY KEY,
    batch_number VARCHAR(50) UNIQUE NOT NULL,
    merchant_id VARCHAR(255) NOT NULL,
    total_amount DECIMAL(20,2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'USD',
    payment_count INTEGER NOT NULL,
    status VARCHAR(50) DEFAULT 'pending',
    period_start TIMESTAMP NOT NULL,
    period_end TIMESTAMP NOT NULL,
    settled_at TIMESTAMP,
    metadata JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Composite for merchant settlement queries
CREATE INDEX IF NOT EXISTS idx_settlement_batches_merchant_status ON settlement_batches(merchant_id, status, period_end DESC);

-- Transaction history (audit log)
CREATE TABLE IF NOT EXISTS transactions (
    id BIGSERIAL PRIMARY KEY,
    payment_id VARCHAR(255),
    customer_id VARCHAR(255),
    merchant_id VARCHAR(255),
    transaction_type VARCHAR(50) NOT NULL,
    amount DECIMAL(15,2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'USD',
    status VARCHAR(50) NOT NULL,
    balance_before DECIMAL(15,2),
    balance_after DECIMAL(15,2),
    description TEXT,
    metadata JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Minimal indexes for transactions (audit log)
CREATE INDEX IF NOT EXISTS idx_transactions_payment_id ON transactions(payment_id);
-- Composite for customer/merchant transaction history
CREATE INDEX IF NOT EXISTS idx_transactions_customer_created ON transactions(customer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_transactions_merchant_created ON transactions(merchant_id, created_at DESC);

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
CREATE TRIGGER update_accounts_updated_at BEFORE UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_settlement_batches_updated_at BEFORE UPDATE ON settlement_batches
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- =====================================================
-- INITIAL DATA
-- =====================================================

-- Insert default platform account
INSERT INTO accounts (id, type, entity_id, balance, available_balance)
VALUES ('platform-001', 'platform', 'platform', 0, 0)
ON CONFLICT (id) DO NOTHING;

-- Create merchant account for test merchant
INSERT INTO accounts (id, type, entity_id, balance, available_balance)
VALUES ('account-merchant-001', 'merchant', 'merchant-001', 0, 0)
ON CONFLICT (id) DO NOTHING;

-- =====================================================
-- COMMENTS
-- =====================================================

COMMENT ON TABLE accounts IS 'Financial accounts for merchants, platform, and customers';
COMMENT ON TABLE ledger_entries IS 'Double-entry bookkeeping ledger for all financial movements';
COMMENT ON TABLE settlement_batches IS 'Batch settlements for merchants';
COMMENT ON TABLE transactions IS 'Audit log of all transactions';

-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_migrations (
    version VARCHAR(50) PRIMARY KEY,
    applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO schema_migrations (version) VALUES ('001_ledger_service_schema') ON CONFLICT DO NOTHING;
