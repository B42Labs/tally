package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Parse reads a pricing model file. It returns the typed model and the
// canonical JSON document the model was built from, which is the form a version
// is stored in.
//
// The document is canonical in that the same file always yields the same bytes:
// mapping keys come out sorted, and every scalar keeps the text the file spells
// it with. That is what lets a re-import be compared against the stored version
// instead of being decided by the order a map was walked in.
func Parse(data []byte) (Model, []byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))

	var value any
	var node yaml.Node
	switch err := dec.Decode(&node); {
	case errors.Is(err, io.EOF):
		// A file with no content at all yields no node to convert. The value
		// stays nil, encodes as null, and the schema refuses it the way it
		// refuses any document that is not a mapping.
	case err != nil:
		return Model{}, nil, fmt.Errorf("parsing the pricing model: %w", err)
	default:
		value, err = generic(&node)
		if err != nil {
			return Model{}, nil, fmt.Errorf("parsing the pricing model: %w", err)
		}

		// Only the first document is read. A second one, appended to add
		// another platform's prices, would otherwise be stored under a version
		// that prices none of it.
		var extra yaml.Node
		switch err := dec.Decode(&extra); {
		case errors.Is(err, io.EOF):
		case err != nil:
			return Model{}, nil, fmt.Errorf("parsing the pricing model: %w", err)
		default:
			return Model{}, nil, errors.New(
				"parsing the pricing model: the file holds more than one document, the model belongs in the first")
		}
	}

	doc, err := json.Marshal(value)
	if err != nil {
		return Model{}, nil, fmt.Errorf("encoding the pricing model as a JSON document: %w", err)
	}

	model, err := ParseDocument(doc)
	if err != nil {
		return Model{}, nil, err
	}
	return model, doc, nil
}

// A model an operator writes reaches none of the bounds below. A file written
// to exhaust the importer reaches them: an alias is expanded at every
// reference, so a handful of nested anchors multiply into a document the file
// on disk gives no sign of. yaml.v3 bounds that expansion nowhere on this path,
// because a decode into a yaml.Node leaves aliases unexpanded and its own
// budget is only armed by the decoder that expands them.
//
// maxValues counts the nodes one conversion visits and maxBytes the text those
// nodes emit. Neither implies the other, so both are needed: a scalar a
// hundred thousand aliases name visits a fraction of the node budget and still
// writes gigabytes into the one buffer json.Marshal builds.
//
// maxDepth is what a pricing model nests to, not what the conversion survives.
// yaml.v3's scanner refuses a document nested past 10000 levels while it is
// still reading it, so the recursion below is already bounded when it starts.
const (
	maxValues = 1 << 20
	maxDepth  = 64
	maxBytes  = 16 << 20
)

// walk is what one conversion carries across the tree: how many values and how
// many bytes it may still produce, and which anchors the path it is on has
// already expanded.
type walk struct {
	left      int
	leftBytes int
	expanded  map[*yaml.Node]bool
}

// generic converts a decoded YAML node tree into the values encoding/json
// marshals the canonical document from.
func generic(node *yaml.Node) (any, error) {
	w := walk{left: maxValues, leftBytes: maxBytes, expanded: make(map[*yaml.Node]bool)}
	return w.value(node, 0)
}

// spend charges the text one scalar writes into the canonical document against
// the byte budget. Every reference to an anchor emits that text again, so the
// number of nodes a conversion visits says nothing about how large the
// document they marshal into gets.
//
// What is charged is what encoding/json writes, not what the file spells. It
// escapes <, >, & and every control character into six bytes apiece, so a file
// of those understates the buffer json.Marshal builds by as much as a factor of
// six, and the budget would let through a document six times the size it names.
func (w *walk) spend(node *yaml.Node) error {
	width := len(node.Value)
	for _, c := range []byte(node.Value) {
		switch {
		case c == '<', c == '>', c == '&', c < 0x20:
			width += 5
		case c == '"', c == '\\':
			width++
		}
	}
	if w.leftBytes -= width; w.leftBytes < 0 {
		return fmt.Errorf("line %d: the model expands to more than %d bytes, which is what nested anchors do",
			node.Line, maxBytes)
	}
	return nil
}

// value converts one node of that tree.
//
// A number becomes a json.Number holding the digits the file wrote, so the
// document carries 0.0008 rather than whatever a float64 renders it back as,
// and decimal.NewFromString later reads the file's own text. Anything but a
// string or a number is refused by its tag: an unquoted 2026-03-01T00:00:00Z is
// a !!timestamp and an unquoted true is a !!bool, and the operator who wrote
// either needs to be told which scalar to quote.
func (w *walk) value(node *yaml.Node, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("line %d: the model nests more than %d levels deep, which no pricing model does",
			node.Line, maxDepth)
	}
	if w.left--; w.left < 0 {
		return nil, fmt.Errorf("line %d: the model expands to more than %d values, which is what nested anchors do",
			node.Line, maxValues)
	}

	switch node.Kind {
	case yaml.DocumentNode:
		// A decoded document holds exactly one value; an empty one leaves the
		// null an empty file yields.
		if len(node.Content) == 0 {
			return nil, nil
		}
		return w.value(node.Content[0], depth+1)

	case yaml.MappingNode:
		mapping := make(map[string]any, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return nil, fmt.Errorf("line %d: a mapping key is a %s, and every key of the model is a string",
					key.Line, key.Tag)
			}
			// The last spelling of a repeated key would win silently, which is
			// how a corrected price ends up next to the one it corrects.
			if _, repeated := mapping[key.Value]; repeated {
				return nil, fmt.Errorf("line %d: the key %q is set twice", key.Line, key.Value)
			}
			// A key is written into the document too, and an aliased mapping
			// writes its keys again at every reference.
			if err := w.spend(key); err != nil {
				return nil, err
			}

			converted, err := w.value(value, depth+1)
			if err != nil {
				return nil, err
			}
			mapping[key.Value] = converted
		}
		return mapping, nil

	case yaml.SequenceNode:
		sequence := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			converted, err := w.value(item, depth+1)
			if err != nil {
				return nil, err
			}
			sequence = append(sequence, converted)
		}
		return sequence, nil

	case yaml.ScalarNode:
		if err := w.spend(node); err != nil {
			return nil, err
		}
		switch node.Tag {
		case "!!str":
			return node.Value, nil
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		default:
			return nil, fmt.Errorf("line %d: %q is a %s, and a model holds strings and numbers only; quote it",
				node.Line, node.Value, node.Tag)
		}

	case yaml.AliasNode:
		// An anchor is registered before the node it names is parsed, so an
		// anchor can name a node that holds the alias itself. Expanding that
		// one would not return.
		if w.expanded[node.Alias] {
			return nil, fmt.Errorf("line %d: the anchor %q holds itself", node.Line, node.Value)
		}
		w.expanded[node.Alias] = true
		defer delete(w.expanded, node.Alias)
		return w.value(node.Alias, depth+1)

	default:
		return nil, fmt.Errorf("line %d: the document holds a value that is neither a mapping, a sequence, nor a scalar",
			node.Line)
	}
}
