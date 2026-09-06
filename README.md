# Tally

Tally does reporting, metering and rating for cloud platforms. It collects
events, metrics and inventory data from a cloud, records the usage it finds in
neutral units, rates that usage with a versioned pricing model and exports the
result as statements. A billing system, a cost dashboard, a capacity plan or a
chargeback workflow reads those statements. OpenStack is the provider that
exists, and the provider pattern behind it is made for further platforms.

The documentation site at
[b42labs.github.io/tally](https://b42labs.github.io/tally/) is generated from
`docs/` in this repository.

## Where to start

- New to Tally: the [tutorials](https://b42labs.github.io/tally/tutorials/)
  take you from an empty machine to a rated month.
- Running Tally as an operator: the
  [how-to guides](https://b42labs.github.io/tally/how-to/) give the steps for
  one task at a time, and the
  [reference](https://b42labs.github.io/tally/reference/) states the exact
  contract of every surface.
- Changing Tally as a contributor: the
  [contributing section](https://b42labs.github.io/tally/contributing/) has the
  conventions a change has to follow, and the `Makefile` has the entry points,
  one `##` comment per target.
- The reasoning behind the design: the
  [explanation quadrant](https://b42labs.github.io/tally/explanation/) says why
  Tally has the shape it has, and what each choice costs.

## Status

The [roadmap](https://b42labs.github.io/tally/explanation/roadmap) says what
exists and what is still ahead.

## License

Tally is licensed under the Apache License 2.0; the full text is in
[LICENSE](LICENSE).
