// This file pins the generated blocks of the handbook pages to the sources
// they are rendered from. The dev-stack page's table of make targets is
// rendered from the help comments of the Makefile, so a help comment edited or
// a target added without `make generate` fails `go test ./docs/` until the page
// carries the change. The pages are rewritten rather than reported when
// TALLY_UPDATE_DOCS=1 is set, which is how a block is refreshed after the
// source moved. The hand-written text outside the markers is not read here:
// docs_test.go judges it against the authoring conventions.
package docs_test

import (
	"testing"

	"github.com/b42labs/tally/internal/refdoc"
)

// makefileSource is the file the dev-stack page renders its target table from.
const makefileSource = "../Makefile"

func TestContributingPagesAreCurrent(t *testing.T) {
	t.Run("dev-stack.md", func(t *testing.T) {
		targets, err := refdoc.MakeTargets(readSource(t, makefileSource))

		refdoc.Verify(t, "../docs/contributing/dev-stack.md", map[string]string{
			"make-targets": render(t, targets, err),
		})
	})
}
