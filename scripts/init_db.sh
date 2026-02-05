#!/bin/bash

# Database initialization script for microservices
# Applies migrations to each service's database in a single PostgreSQL instance

set -e

DB_HOST="${DB_HOST:-localhost}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-postgres}"
DB_PASSWORD="${DB_PASSWORD:-postgres}"
CONTAINER_NAME="${CONTAINER_NAME:-payment_postgres}"

echo "🚀 Initializing databases for all microservices"
echo "📍 PostgreSQL: $DB_HOST:$DB_PORT"
echo ""

# Database configurations for each service
# Format: service_name:dbname:migration_file
declare -a SERVICES=(
    "api-gateway:api_gateway_db:001_api_gateway_schema.sql"
    "payment-orchestrator:payment_orchestrator_db:001_payment_orchestrator_schema.sql"
    "fraud-service:fraud_service_db:001_fraud_service_schema.sql"
    "ledger-service:ledger_service_db:001_ledger_service_schema.sql"
)

MIGRATIONS_DIR="$(cd "$(dirname "$0")/../migrations" && pwd)"

# Check if running in Docker mode
USE_DOCKER=false
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    USE_DOCKER=true
    echo "🐳 Using Docker mode (container: $CONTAINER_NAME)"
else
    echo "💻 Using local psql mode"
fi

echo ""

# Function to initialize a database
init_database() {
    local config=$1
    
    IFS=':' read -r service_name dbname migration <<< "$config"
    
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "📦 Service: $service_name"
    echo "📍 Database: $dbname"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    
    # Wait for PostgreSQL to be ready
    if [ "$USE_DOCKER" = true ]; then
        echo "⏳ Waiting for PostgreSQL to be ready..."
        local max_attempts=30
        local attempt=1
        
        until docker exec "$CONTAINER_NAME" pg_isready -U "$DB_USER" > /dev/null 2>&1; do
            if [ $attempt -ge $max_attempts ]; then
                echo "❌ Failed to connect to PostgreSQL after $max_attempts attempts"
                return 1
            fi
            echo "   Attempt $attempt/$max_attempts - waiting..."
            sleep 2
            ((attempt++))
        done
        echo "✅ PostgreSQL is ready!"
    else
        echo "⏳ Waiting for PostgreSQL to be ready..."
        until PGPASSWORD=$DB_PASSWORD psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d postgres -c '\q' 2>/dev/null; do
            echo "   PostgreSQL is unavailable - sleeping"
            sleep 2
        done
        echo "✅ PostgreSQL is ready!"
    fi
    
    echo ""
    
    # Apply migration
    local migration_file="$MIGRATIONS_DIR/$migration"
    if [ -f "$migration_file" ]; then
        echo "📝 Applying migration: $migration"
        
        if [ "$USE_DOCKER" = true ]; then
            # Copy migration to container and apply
            docker cp "$migration_file" "$CONTAINER_NAME:/tmp/migration.sql"
            if docker exec "$CONTAINER_NAME" psql -U "$DB_USER" -d "$dbname" -f /tmp/migration.sql > /dev/null 2>&1; then
                echo "✅ Migration applied successfully!"
                docker exec "$CONTAINER_NAME" rm /tmp/migration.sql 2>/dev/null || true
            else
                echo "❌ Failed to apply migration"
                return 1
            fi
        else
            # Apply migration using local psql
            if PGPASSWORD=$DB_PASSWORD psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$dbname" -f "$migration_file" > /dev/null 2>&1; then
                echo "✅ Migration applied successfully!"
            else
                echo "❌ Failed to apply migration"
                return 1
            fi
        fi
    else
        echo "⚠️  Migration file not found: $migration_file"
        return 1
    fi
    
    # Show statistics
    echo ""
    echo "📊 Database statistics:"
    
    if [ "$USE_DOCKER" = true ]; then
        docker exec "$CONTAINER_NAME" psql -U "$DB_USER" -d "$dbname" -t -c "
            SELECT 
                'Tables: ' || COUNT(*) || ' | Size: ' || pg_size_pretty(SUM(pg_total_relation_size(schemaname||'.'||tablename)))
            FROM pg_tables 
            WHERE schemaname = 'public';
        " | xargs
        
        echo ""
        echo "📋 Tables:"
        docker exec "$CONTAINER_NAME" psql -U "$DB_USER" -d "$dbname" -c "
            SELECT 
                tablename,
                pg_size_pretty(pg_total_relation_size('public.'||tablename)) as size
            FROM pg_tables 
            WHERE schemaname = 'public'
            ORDER BY tablename;
        "
    else
        PGPASSWORD=$DB_PASSWORD psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$dbname" -t -c "
            SELECT 
                'Tables: ' || COUNT(*) || ' | Size: ' || pg_size_pretty(SUM(pg_total_relation_size(schemaname||'.'||tablename)))
            FROM pg_tables 
            WHERE schemaname = 'public';
        " | xargs
        
        echo ""
        echo "📋 Tables:"
        PGPASSWORD=$DB_PASSWORD psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$dbname" -c "
            SELECT 
                tablename,
                pg_size_pretty(pg_total_relation_size('public.'||tablename)) as size
            FROM pg_tables 
            WHERE schemaname = 'public'
            ORDER BY tablename;
        "
    fi
    
    echo ""
}

# Initialize all databases
failed_services=()
for service_config in "${SERVICES[@]}"; do
    if ! init_database "$service_config"; then
        IFS=':' read -r service_name _ _ <<< "$service_config"
        failed_services+=("$service_name")
    fi
done

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

if [ ${#failed_services[@]} -eq 0 ]; then
    echo "🎉 All databases initialized successfully!"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "📌 Database Summary (Single PostgreSQL Instance):"
    echo "   Host: $DB_HOST:$DB_PORT"
    echo "   • api_gateway_db"
    echo "   • payment_orchestrator_db"
    echo "   • fraud_service_db"
    echo "   • ledger_service_db"
    echo ""
    echo "💡 Connect to databases:"
    if [ "$USE_DOCKER" = true ]; then
        echo "   docker exec -it $CONTAINER_NAME psql -U postgres -d api_gateway_db"
        echo "   docker exec -it $CONTAINER_NAME psql -U postgres -d payment_orchestrator_db"
        echo "   docker exec -it $CONTAINER_NAME psql -U postgres -d fraud_service_db"
        echo "   docker exec -it $CONTAINER_NAME psql -U postgres -d ledger_service_db"
    else
        echo "   psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d api_gateway_db"
        echo "   psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d payment_orchestrator_db"
        echo "   psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d fraud_service_db"
        echo "   psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d ledger_service_db"
    fi
    echo ""
    echo "📊 List all databases:"
    if [ "$USE_DOCKER" = true ]; then
        echo "   docker exec $CONTAINER_NAME psql -U postgres -l"
    else
        echo "   psql -h $DB_HOST -p $DB_PORT -U $DB_USER -l"
    fi
    echo ""
    exit 0
else
    echo "❌ Failed to initialize databases for: ${failed_services[*]}"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    exit 1
fi
