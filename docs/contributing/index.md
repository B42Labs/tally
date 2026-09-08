---
title: Contributing
description: How the Tally project itself is developed and documented.
quadrant: contributing
audience: contributor
---

# Contributing

This section documents the project rather than the product. It is for people
who change the repository: how the code is built and tested, how a change is
reviewed, and how the documentation itself is written.

It sits outside Diátaxis on purpose. The four quadrants are cut by what a
reader of Tally needs, and the questions a contributor asks fit none of them.
Pressing this material into a quadrant would make that quadrant harder to read
for everyone who is not a contributor.

## The handbook

Each page below covers one part of the project.

- [The dev stack](/contributing/dev-stack): what `make up` builds on your
  machine, and which make target owns each piece of it.
- [The toolchain](/contributing/toolchain): the Go toolchain, the code
  generators, the linter and the formatter, the container build, and what to
  regenerate after a change.
- [How the tests are organised](/contributing/tests): the four kinds of test,
  what each one needs from your machine, and why the golden suites accept no
  tolerance.
- [Writing and previewing documentation](/contributing/writing-documentation):
  where a page goes, the shape each kind of page takes, the generated blocks,
  how to preview the site, and the gates a change passes.
- [Authoring conventions](/contributing/authoring-conventions): the rules every
  page under `docs/` has to satisfy, and the test that enforces them.
- [Phase 2: the alert drill](/contributing/drills/phase2) and
  [Phase 3: the month drill](/contributing/drills/phase3): the acceptance drill
  records, one of the alert that went from a seeded event to its resolution,
  one of the full simulated month, both as they ran on a dev cluster.

A drill record holds the procedure as it was written and the observations of
the run it records. It is kept because it is evidence of how the system
behaved. A record is not maintained as a guide: its commands are those of the
commit it names, and the current steps are on the how-to guides and the
tutorials.
