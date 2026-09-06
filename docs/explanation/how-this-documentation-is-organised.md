---
title: How this documentation is organised
description: Why the documentation follows Diátaxis and how to place a new page in it.
quadrant: explanation
audience: all
---

# How this documentation is organised

This documentation follows [Diátaxis](https://diataxis.fr), a way of organising
technical documentation around what the reader needs at the moment they open a
page. Diátaxis names four such needs: learning, doing, information and
understanding. Each need wants a different kind of page written in a different
voice, and a page that tries to serve two of them serves neither well.

## The four quadrants

| Quadrant | What it serves | The reader's question | The voice |
| --- | --- | --- | --- |
| [Tutorials](/tutorials/) | learning | "Teach me" | a lesson |
| [How-to guides](/how-to/) | a task | "How do I do X" | a recipe |
| [Reference](/reference/) | information | "What exactly is X" | a specification |
| [Explanation](/explanation/) | understanding | "Why is it like this" | a discussion |

## Telling the quadrants apart

Two distinctions go wrong in practice.

The first is tutorial against how-to guide. Both are a numbered series of
steps, so they look alike on the page. A tutorial is for a reader who does not
yet know what they are doing and has to end up somewhere; a how-to guide is for
a reader who knows exactly what they want and needs the steps that get it. A
tutorial may therefore choose values for the reader and leave cases out. A
how-to guide may not.

The second is reference against explanation. Both are prose about a design. A
reference states the contract and stops. An explanation gives the reasoning and
states no contract the reader could build against.

One test settles both. Write a one-sentence purpose statement for the page. If
it contains "learn", the page is a tutorial. If it contains "in order to", it
is a how-to guide. If it is a noun phrase, it is a reference. If it contains
"because" or "rather than", it is an explanation.

## Placing a new page

Pick the single need the page serves. Put the file under that quadrant's
directory and set `quadrant` in the frontmatter to the same quadrant. Add one
entry for the page to `docs/.vitepress/sidebar.json`. Run `go test ./docs/`,
which checks the frontmatter, the headings and the sidebar entry.

A page that drifts toward a second need is two pages. Split it and link the
halves to each other.

## Beyond the four quadrants

The [Contributing](/contributing/) section documents the project rather than
the product, so it sits outside the four quadrants. Diátaxis cuts the material
by what a reader of Tally needs; a contributor asks about the repository
instead, and none of the four quadrants answers that.

## Why this documentation adopted it

The corpus was organised by component before: one long document per subsystem.
Every one of those documents mixed the steps to set the subsystem up, the
contract of its configuration and the rationale for its design. A reader with
one of the four needs had to mine it out of a document written for all four,
and a reader who wanted to add a section had no obvious place to put it.
