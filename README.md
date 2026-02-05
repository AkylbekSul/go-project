# Payment Processing System

Микросервисная система обработки платежей.

## Архитектура

**Сервисы:**
- **API Gateway** (8081) - точка входа, идемпотентность, публикация событий
- **Payment Orchestrator** (8082) - оркестрация платежа, state machine
- **Fraud Service** (8083) - проверка на мошенничество
- **Ledger Service** (8084) - двойная бухгалтерия, учет операций

**Инфраструктура:** PostgreSQL, Redis, Kafka, NATS

## Границы сервисов

### API Gateway
- **БД:** `api_gateway_db` (таблицы: `payments`, `customers`, `merchants`)
- **Зависимости:** PostgreSQL, Redis (кэш), Kafka (публикация `payment.created`)

### Payment Orchestrator
- **БД:** `payment_orchestrator_db` (таблицы: `payment_states`, `outbox_events`, `inbox_events`)
- **Зависимости:** PostgreSQL, Redis (блокировки), Kafka (потребление/публикация), NATS (запросы к Fraud)
- **Состояния:** NEW → AUTH_PENDING → AUTHORIZED → CAPTURED → SUCCEEDED/FAILED

### Fraud Service
- **БД:** `fraud_service_db` (таблицы: `fraud_rules`, `fraud_decisions`, `velocity_counters`)
- **Зависимости:** PostgreSQL, Redis (velocity counters), NATS (`fraud.check`)
- **Решения:** `approve`, `deny`, `manual_review`

### Ledger Service
- **БД:** `ledger_service_db` (таблицы: `accounts`, `ledger_entries`, `settlement_batches`)
- **Зависимости:** PostgreSQL, Kafka (потребление `payment.state.changed`)

## API Endpoints

### API Gateway (8081)
- `POST /payments` - создание платежа (требуется `Idempotency-Key`)
- `GET /payments/:id` - получение платежа
- `POST /payments/:id/confirm` - подтверждение платежа
- `GET /health` - health check

### Payment Orchestrator (8082)
- `GET /payments/:id/state` - состояние платежа (NEW, AUTH_PENDING, AUTHORIZED, CAPTURED, SUCCEEDED, FAILED)
- `GET /health` - health check

### Fraud Service (8083)
- `GET /fraud/stats` - статистика проверок
- `GET /health` - health check
- NATS: `fraud.check` (request-reply)

### Ledger Service (8084)
- `GET /accounts/:id/balance` - баланс счета
- `GET /accounts/:id/entries` - записи по счету
- `GET /payments/:id/entries` - записи по платежу
- `GET /health` - health check

## Схема взаимодействия

```
Client → API Gateway (8081)
         │ Kafka: payment.created
         ▼
Payment Orchestrator (8082)
         │ NATS: fraud.check (request-reply)
         ▼
Fraud Service (8083)
         │ NATS: decision
         ▼
Payment Orchestrator (8082)
         │ Kafka: payment.state.changed
         ▼
Ledger Service (8084)
```

### События

**Kafka:**
- `payment.created` (API Gateway → Payment Orchestrator)
- `payment.state.changed` (Payment Orchestrator → Ledger Service)

**NATS:**
- `fraud.check` (Payment Orchestrator ↔ Fraud Service, request-reply)

### State Machine

```
NEW → AUTH_PENDING → AUTHORIZED → CAPTURED → SUCCEEDED
                      ↓
                    FAILED
```

### Правила Fraud Service
- Платежи > $10,000 → deny
- Максимум 5 платежей/час на клиента → deny
- Платежи > $5,000 → manual_review

## Запуск

### Быстрый старт (с дефолтными значениями)

```bash
docker-compose up -d
```

### Настройка окружения

1. Скопируйте файл `.env.example` в `.env`:
```bash
cp .env.example .env
```

2. Отредактируйте `.env` и установите безопасные пароли:
```bash
POSTGRES_PASSWORD=your_secure_password_here
GRAFANA_ADMIN_PASSWORD=your_secure_password_here
```

3. Запустите сервисы:
```bash
docker-compose up -d
```

**Важно:** Файл `.env` не должен попадать в Git (уже добавлен в `.gitignore`). Используйте `.env.example` как шаблон.

### Переменные окружения

Основные переменные, которые можно настроить в `.env`:

- `POSTGRES_USER` - пользователь PostgreSQL (по умолчанию: `postgres`)
- `POSTGRES_PASSWORD` - пароль PostgreSQL (по умолчанию: `postgres`)
- `GRAFANA_ADMIN_USER` - пользователь Grafana (по умолчанию: `admin`)
- `GRAFANA_ADMIN_PASSWORD` - пароль Grafana (по умолчанию: `admin`)
- `DATABASE_URL_API_GATEWAY` - полный URL подключения к БД API Gateway (опционально, по умолчанию использует `postgres:postgres`)
- `DATABASE_URL_PAYMENT_ORCHESTRATOR` - полный URL подключения к БД Payment Orchestrator
- `DATABASE_URL_FRAUD_SERVICE` - полный URL подключения к БД Fraud Service
- `DATABASE_URL_LEDGER_SERVICE` - полный URL подключения к БД Ledger Service
- `REDIS_URL` - URL Redis (по умолчанию: `redis:6379`)
- `KAFKA_BROKERS` - адреса брокеров Kafka (по умолчанию: `kafka:29092`)
- `NATS_URL` - URL NATS (по умолчанию: `nats://nats:4222`)
- `JAEGER_ENDPOINT` - endpoint Jaeger (по умолчанию: `jaeger:4318`)
- `PORT_API_GATEWAY` - порт API Gateway (по умолчанию: `8081`)
- `PORT_PAYMENT_ORCHESTRATOR` - порт Payment Orchestrator (по умолчанию: `8082`)
- `PORT_FRAUD_SERVICE` - порт Fraud Service (по умолчанию: `8083`)
- `PORT_LEDGER_SERVICE` - порт Ledger Service (по умолчанию: `8084`)

**Примечание:** Если вы изменили `POSTGRES_PASSWORD`, обязательно установите переменные `DATABASE_URL_*` для каждого сервиса с правильными учетными данными, например:
```
DATABASE_URL_API_GATEWAY=postgres://postgres:your_password@postgres:5432/api_gateway_db?sslmode=disable
```
