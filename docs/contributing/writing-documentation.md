---
title: Writing and previewing documentation
description: "Where a page goes, the shape each quadrant expects, the generated blocks, how to preview the site, and the gates a documentation change passes before it is published."
quadrant: contributing
audience: contributor
---

# Writing and previewing documentation

Every page of this documentation lives under `docs/`, ships through one
VitePress build to `https://b42labs.github.io/tally/`, and is reviewed in the
pull request that changes the code it describes. The rules a page has to
satisfy are on [Authoring conventions](/contributing/authoring-conventions).
This page is the workflow around them.

## Where a page goes

Write the page's purpose down as one sentence first, because that sentence
names the quadrant. A sentence with "learn" in it belongs to a tutorial, one
with "in order to" to a how-to guide, a noun phrase to a reference, and one
with "because" or "rather than" to an explanation.
[Placing a new page](/explanation/how-this-documentation-is-organised#placing-a-new-page)
states the test and what follows from it.

The quadrant is the directory: `docs/tutorials/`, `docs/how-to/`,
`docs/reference/` and `docs/explanation/`. A page about the project rather than
about the product goes under `docs/contributing/`, which sits outside the four
quadrants.

Every page gets one entry in `docs/.vitepress/sidebar.json`. `go test ./docs/`
checks that the entry exists and that the page's `quadrant` field matches the
directory it lies in, so a file in the wrong place fails the gate instead of
reaching the site under a promise its directory does not keep.

## The shape of each kind of page

A tutorial opens, right after the frontmatter, with the comment that says where
its output was taken from:

```html
<!-- Shown output captured on <date> from commit <sha> with <versions> -->
```

Then come `## Before you start` with what the machine needs, the numbered
steps, each with its command in a fenced block and the output that command
printed fenced under it, and at the end `## What you learned` and
`## Where to go next`. The shown output is copied from a run at the commit the
comment names and is never typed, because the reader holds their own terminal
against it.

A how-to guide opens with `## Before you start` and closes with
`## Check the result`, the call that shows the task succeeded. It states no
rationale. Where the reader wants one, the guide links the explanation that
holds it, the way [Run a period](/how-to/engine/run-a-period) links
[metering separated from rating](/explanation/metering-separated-from-rating).

A reference page is hand-written prose around generated blocks, neutral in
voice, one topic per page. [Metrics](/reference/observability/metrics) has that
shape: the prose says who serves the series and under which conditions, and the
tables between the markers come out of the code.

An explanation page is written in the present tense and states how things are
rather than when they were decided. Every roadmap decision it rests on is
linked by its GitHub URL, as
[Alerting design](/explanation/alerting-design) links the phase document that
asked for the two components.

A contributing page describes the repository as it is, in the same voice as a
reference page: the files, the targets and the tests a contributor runs.

## Generated blocks

Part of a reference page is rendered from the code rather than written. The
rendered text sits between two marker lines that name the block:

```html
<!-- refdoc:begin name -->
<!-- refdoc:end name -->
```

What lies between them belongs to a renderer of `internal/refdoc` and is never
edited by hand. Everything outside them is hand-written and stays byte for byte
as it is. `make generate` refreshes every block: it runs the doc tests with
`TALLY_UPDATE_DOCS=1`, which writes the rendered text into the pages instead of
reporting the difference. A block left stale fails `go test ./docs/` with
`block "..." differs from its source, run make generate`.

The blocks live on the reference pages whose subtests `docs/reference_test.go`
names: the two Reporting API pages, the command-line pages, the configuration
pages, the formats pages and the observability pages. The command-line page of
a binary is pinned by that binary's own package as well, in the
`TestReferencePageIsCurrent` of `cmd/tally-engine`, `cmd/tally-reporting-admin`,
`cmd/tally-openstack-collector`, `cmd/tally-openstack-simulator` and
`cmd/tally-vertical-slice`. One handbook page carries a block too: the
make-target table of [The dev stack](/contributing/dev-stack), rendered from
the `## target:` comments of the `Makefile`.

[What to regenerate after a change](/contributing/toolchain#what-to-regenerate-after-a-change)
says which pages a given change touches and which test names the miss.

## Links and placeholders

An internal link is root-relative and carries no extension, as in
`/how-to/engine/run-a-period`. A relative link, one that climbs out of the
page's directory with two dots, is never right here: the published page sits at
a URL the repository layout does not match, so a path that resolves in a
Markdown preview points at nothing on the site.

A file of the repository is no page of the site. Link it by its GitHub URL on
`main`, `https://github.com/B42Labs/tally/blob/main/<path>`, or name it in a
code span where the reader only has to know which file it is. A dev-cluster URL
is written out verbatim, as in `https://api.tally.127-0-0-1.nip.io:8443`.

A placeholder is written in a code span, as `<name>`. VitePress renders
Markdown through Vue, which reads a bare `<name>` as the opening tag of a
component and fails the build on the component it cannot find.

## Preview

`make docs` serves the site with live reload, so a saved file shows up in the
browser without a rebuild. It needs Node 24 or newer on the host, the version
`.nvmrc` names and the `engines` field of `package.json` asks for. The target
runs `npm ci` on the first run and again after `package-lock.json` changes,
through the `node_modules/.package-lock.json` rule of the `Makefile`, and
nothing else has to be installed.

The dev server reads `sidebar.json` and `nav.json` once at start-up:
`docs/.vitepress/config.mts` loads both with `readFileSync`. Restart
`make docs` after editing either file, or the browser keeps showing the old
navigation.

`make docs-build` runs the build CI runs, `npm run docs:build`, into
`docs/.vitepress/dist`. It fails on a dead internal link, which is the link
check of the site.

## The gates

`go test ./docs/` runs the five corpus tests of `docs/docs_test.go`.

- `TestEveryPageCarriesTheFrontmatterContract` catches a missing frontmatter
  field, a `quadrant` that names another directory, an unknown `audience` and a
  `layout` key.
- `TestHeadingsAreSentenceCaseWithOneH1` catches a second H1, an H1 that
  differs from `title`, a heading in title case and a skipped level.
- `TestSidebarReachesEveryPageAndOnlyPages` catches a page no sidebar entry
  links and an entry that links no page.
- `TestNavReachesEverySectionAndOnlyPages` catches a top-navigation entry that
  links no page and a section missing from the navigation.
- `TestNoMarkdownFileEscapesTheGate` catches a Markdown file under `docs/` that
  is not a page of a section.

`TestReferencePagesAreCurrent` and `TestContributingPagesAreCurrent` run beside
them and read the generated blocks, the first over the reference pages and the
second over the handbook. `make docs-build` is the other gate, and it is the
one that finds a dead internal link. The `ci` job of
`.github/workflows/ci.yaml` runs the tests and its `docs` job runs
`npm run docs:build`, both on every pull request.

When the heading gate reports a capitalised word, decide which of the two cases
it is. A proper noun goes into `docs/.vitepress/proper-nouns.txt` in the same
pull request as the heading that needs it. Anything else means the heading is
wrong, and the heading is rewritten.

## Publishing

`.github/workflows/deploy-docs.yaml` publishes the site on a push to `main`
that touches `docs/`, `package.json`, `package-lock.json`, `.nvmrc` or the
workflow itself. A change to the Go code alone leaves the published site where
it was.

A page is served at the path of its file without the extension, because
`cleanUrls` is set: `docs/how-to/engine/run-a-period.md` is published at
`/how-to/engine/run-a-period` under `https://b42labs.github.io/tally/`, whose
path segment is the site's `base`, `/tally/`.

Every page carries a `lastUpdated` date, which VitePress reads out of the git
history of the file. The workflow checks the repository out with
`fetch-depth: 0` for it, because a shallow clone carries no history to read the
date from. Every page carries an edit link beside it, which opens the file on
GitHub.

## Reviewing a documentation change

A reviewer of a documentation change looks at this:

- Both gates are green on the pull request: `go test ./docs/` in the `ci` job
  and `npm run docs:build` in the `docs` job.
- The page sits in the quadrant its one-sentence purpose names.
- A tutorial's shown output comes from a run at the commit its capture comment
  names, and that run stays in the comment: no step has the reader print the
  commit they are on, and no sentence names the machine, the browser or the
  tool versions the page was captured with. A reader cannot act on any of it.
- A how-to guide opens with `## Before you start`, closes with
  `## Check the result`, and links its reasoning instead of stating it.
- A reference page's generated blocks were refreshed by `make generate` rather
  than edited by hand.
- No link is relative.
- The sidebar entry is in place and the preview renders the page it points at.
