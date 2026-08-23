-- name: ListBillingPeriods :many
SELECT period_from, status, finalized_run_id, finalized_at
FROM billing_periods
ORDER BY period_from;

-- name: InsertPricingModel :execrows
INSERT INTO pricing_models (version, valid_from, currency, document)
VALUES ($1, $2, $3, $4)
ON CONFLICT (version) DO NOTHING;

-- name: GetPricingModel :one
SELECT version, valid_from, currency, document, imported_at
FROM pricing_models
WHERE version = $1;

-- name: ListPricingModels :many
SELECT version, valid_from, currency, imported_at
FROM pricing_models
ORDER BY valid_from;

-- name: PricingModelForPeriod :one
SELECT version, valid_from, currency, document, imported_at
FROM pricing_models
WHERE valid_from <= $1
ORDER BY valid_from DESC
LIMIT 1;
