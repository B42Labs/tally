-- name: ListBillingPeriods :many
SELECT period_from, status, finalized_run_id, finalized_at
FROM billing_periods
ORDER BY period_from;
