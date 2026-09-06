---
title: Money and rounding
description: "Why money is decimal end to end, rounded once per dimension, and never summed across currencies."
quadrant: explanation
audience: all
---

# Money and rounding

A statement is checked against an invoice by hand, so the arithmetic behind it
has to be reproducible to the cent. Three rules get that: decimals everywhere,
one rounding step per dimension, and one currency per pricing model version. The
normative text is section 6 of
[the roadmap conventions](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md),
and the single implementation is
[`internal/core/money`](https://github.com/B42Labs/tally/blob/main/internal/core/money/money.go).

## Why decimals end to end

A binary float cannot hold 0.01, so a chain of float multiplications drifts away
from the number a customer can recompute. Money, prices and usage quantities are
therefore decimals from end to end, and a decimal is constructed from a string
or an integer only.

Floats are kept out of the money paths by the `forbidigo` rules in
[`.golangci.yml`](https://github.com/B42Labs/tally/blob/main/.golangci.yml),
which refuse `decimal.NewFromFloat` and `InexactFloat64`. The database stores
money as `NUMERIC`, never as `real` or `double precision`. Every division runs
through the money helpers at an explicit working precision of 28 places, so no
result depends on a package-level default somewhere else.

## Why rounding happens once

A per-dimension cost is computed at full precision and rounded once, half away
from zero, to two decimal places per dimension per usage record. Every aggregate
above it, per resource, per project and per period, is a sum of already-rounded
values. A total therefore always equals the sum of the line items shown beside
it, which is what makes a statement checkable line by line.

`money.Round2` is the single rounding entry point. The three scales the package
fixes are `AmountPlaces` at 2 for a monetary value, `QuantityPlaces` at 4 for a
usage quantity such as `minutes` or a counter, and `RatePlaces` at 6 for the
rate of a pricing adjustment.

An adjustment follows the same rule one level up: each adjustment line is
rounded once, and the chain applies those rounded amounts, so the net a kickback
is computed on is the net the statement prints (decision D7 of
[the commercial pricing roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/05-phase-5-commercial-pricing.md),
see [commercial pricing on relations](/explanation/commercial-pricing-on-relations)).

## One currency per model version

The currency is defined per pricing model version, and it is EUR today. Values
in different currencies are never aggregated: a sum over two currencies has no
meaning without a rate, and the rate is not something a billing run is allowed
to invent.

## How amounts are rendered

An export keeps the two decimal places a money value was rounded to, so `19.20`
never reaches a customer as `19.2`. A bare decimal drops the trailing zero, so
the money package wraps a monetary value in a type whose JSON marshaller writes
it at exactly two places. A usage quantity is wrapped the same way at four
places, and an adjustment rate at six.
