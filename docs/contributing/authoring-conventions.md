---
title: Authoring conventions
description: The frontmatter, heading, link and sidebar rules every page under docs/ must satisfy, and the gate that enforces them.
quadrant: contributing
audience: contributor
---

# Authoring conventions

Every page under `docs/` ships through one VitePress build and one set of
rules. The rules are checked by `docs/docs_test.go`, so a page that breaks one
of them fails the test run instead of reaching the site.

## Frontmatter contract

Every page opens with a YAML frontmatter block that carries these fields. A
key the table does not list is left to VitePress and ignored by the gate.

| Field | Type | Required | Values |
| --- | --- | --- | --- |
| `title` | string | yes | Sentence case; identical to the page's H1 |
| `description` | string | yes | One sentence; used for the `<meta>` description and search snippets |
| `quadrant` | enum | yes | `orientation` (only `docs/index.md`), `tutorial`, `how-to`, `reference`, `explanation`, `contributing`; must match the directory |
| `audience` | enum | yes | `operator`, `integrator`, `contributor`, `all` |
| `layout` | string | no | Must be absent: the site has no landing page and no custom layouts |

Put a description in double quotes when it contains a colon followed by a
space, or when it starts with a character YAML reads as syntax.

## Headings

A page has exactly one H1 and its text is identical to `title`. Everything
below the H1 starts at H2, and no level is skipped.

Every heading from H1 to H6 is sentence case. The gate applies this rule: an
inline code span counts as one capitalised token and always passes; the heading
is split on whitespace and each word is stripped of surrounding punctuation;
the first word has to start with an uppercase letter or a digit; every later
word that starts with an uppercase letter has to carry a second uppercase
letter or a digit after its first character, such as `OpenStack`, `API` or
`H1`, or appear verbatim in `docs/.vitepress/proper-nouns.txt`.

So `## Find your path` passes and `## Find Your Path` fails.

For a capitalised word the rule does not cover, add the word to
`docs/.vitepress/proper-nouns.txt`: one word per line, `#` starts a comment,
blank lines are ignored. Extend that file in the same pull request as the
heading that needs the word.

## Placeholders

VitePress renders Markdown through Vue, so a placeholder in bare angle brackets
is parsed as a component name and the build fails on the unknown component.
Write the placeholder in a code span instead.

The form that fails the build is the bare text `<Placeholder>` with no
backticks around it. The form that builds is `` `<Placeholder>` ``, the same
text inside a code span. This holds in table cells and headings as well as in
running prose.

## Links

An internal link is root-relative and carries no extension, such as
`/how-to/`. A link that ends in `/` names that directory's `index.md`, so
`/how-to/` resolves to `docs/how-to/index.md`. An internal link to a page that
does not exist fails `npm run docs:build`, which is what `make docs-build`
runs. An external link is a plain URL.

## Sidebar

Every page under a section directory has an entry in
`docs/.vitepress/sidebar.json`, and every link in that file resolves to a page.
The gate checks both directions, so a page without an entry and an entry
without a page each fail.

The file maps a path prefix to a list of groups. A group has a `text` and a
list of `items`. A group nested inside `items` can set `collapsed` to start
folded.

```json
{
  "/how-to/": [
    {
      "text": "How-to guides",
      "items": [
        { "text": "Overview", "link": "/how-to/" },
        {
          "text": "OpenStack",
          "collapsed": true,
          "items": [
            { "text": "Collect from Ceilometer", "link": "/how-to/openstack/ceilometer" }
          ]
        }
      ]
    }
  ]
}
```

The five entries of the top navigation are the same kind of data, one per
section, in `docs/.vitepress/nav.json`. The gate checks that each entry links a
page and that no section is missing from it, because VitePress checks dead
links in rendered Markdown only and these five sit on every page.

The dev server reads `sidebar.json` and `nav.json` once at start-up, so restart
`make docs` after editing either file.

## What the gate reports

`go test ./docs/` runs five tests over the site.

`TestEveryPageCarriesTheFrontmatterContract` checks that every page opens with
a frontmatter block in which `title` and `description` are set, `quadrant`
matches the page's directory, `audience` is one of the four values and `layout`
is absent.

`TestHeadingsAreSentenceCaseWithOneH1` checks that every page has one H1 equal
to its `title`, that every heading is sentence case, and that no heading level
is skipped.

`TestSidebarReachesEveryPageAndOnlyPages` checks that every page has a sidebar
entry and that every sidebar link resolves to a page.

`TestNavReachesEverySectionAndOnlyPages` checks that every entry of the top
navigation links a page and names a section in its `activeMatch`, and that no
section is missing from it.

`TestNoMarkdownFileEscapesTheGate` checks that every Markdown file under
`docs/` is either a page of the site or listed in the `srcExclude` array of
`docs/.vitepress/config.mts`. A file in neither set is published by VitePress
and read by none of the tests above, so a new page belongs under a section
directory and a new document that is not one belongs in that array.

The tests beside these five feed each rule the input that has to fail it, so a
rule that stops deciding anything fails a test rather than passing every page.

A failure names the file with a path relative to `docs/`, then the line, then
the problem, as in `how-to/scratch.md:1: no frontmatter block`. The three
tests that read the pages report one subtest per page, so `go test ./docs/ -run
'TestHeadingsAreSentenceCaseWithOneH1/how-to'` narrows a run to one section
while a page is being fixed.

## Preview and build

`make docs` serves the site locally with live reload, so a saved file shows up
in the browser without a rebuild. `make docs-build` builds the site into
`docs/.vitepress/dist` and fails on a dead internal link.

Both targets need Node 24 or newer on the host. They run `npm ci` on the first
build and again after `package-lock.json` changes, so there is no separate
install step.
