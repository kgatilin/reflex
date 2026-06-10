package graph

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Describe writes a textual table of the graph to w:
//
//	NAME        TYPE          DESCRIPTION                  CONSUMES         EMITS
//	parse       parse_target  ...                           RequestReceived  TargetParsed, ParseFailed(T)
//	...
//
// Terminal emissions are tagged with `(T)`. The intended consumer is
// `reflex describe --config <file>` — humans, not other tools.
func (g *HandlerGraph) Describe(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tTYPE\tDESCRIPTION\tCONSUMES\tEMITS\tLOOP"); err != nil {
		return err
	}
	for _, n := range g.Nodes {
		var emits []string
		seen := map[string]bool{}
		for _, em := range n.Spec.Emits {
			label := em.Type
			if em.Terminal {
				label += "(T)"
			}
			if seen[label] {
				continue
			}
			seen[label] = true
			emits = append(emits, label)
		}
		loopCol := "-"
		if n.LoopCap > 0 {
			loopCol = fmt.Sprintf("%s(max=%d)", n.LoopName, n.LoopCap)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Name, n.Type, n.Spec.Description,
			n.Spec.Consumes, strings.Join(emits, ", "), loopCol); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\n%d handlers, %d edges, %d declared loops\n",
		len(g.Nodes), len(g.Edges), len(g.DeclaredLoops)); err != nil {
		return err
	}
	return nil
}
