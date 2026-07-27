package vision

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

const (
	// NoneLabel marks a corpus frame the recognizer must refuse to identify.
	NoneLabel = "_none"
	// NonePrediction is the confusion matrix column for "nothing recognized".
	NonePrediction = "<none>"
)

// Prediction is one frame's true label beside what the recognizer called it.
// Predicted is empty when recognition failed.
type Prediction struct {
	Hash      string
	Label     string
	Predicted string
}

// Report summarizes recognizer performance over a labeled corpus.
type Report struct {
	Total   int
	Correct int
	// Matrix is true label → predicted → count, with NonePrediction standing
	// in for a failed recognition.
	Matrix  map[string]map[string]int
	Labels  []string // matrix rows, sorted, NoneLabel last
	Columns []string // matrix columns, sorted, NonePrediction last
}

// Score builds a Report. It is pure: everything expensive and image-shaped
// happens in the caller, so the rules that decide right from wrong get fast
// exhaustive tests.
//
// A positive is correct on an exact match. A negative is correct only when
// recognition failed — that is the rule that stops thresholds so loose every
// frame matches something from passing the gate.
func Score(preds []Prediction) Report {
	r := Report{Matrix: map[string]map[string]int{}}
	cols := map[string]bool{}

	for _, p := range preds {
		r.Total++
		predicted := p.Predicted
		if predicted == "" {
			predicted = NonePrediction
		}
		if r.Matrix[p.Label] == nil {
			r.Matrix[p.Label] = map[string]int{}
		}
		r.Matrix[p.Label][predicted]++
		cols[predicted] = true

		switch {
		case p.Label == "":
			// An empty label carries no ground truth, so nothing — not even
			// another empty string — can match it correctly. Score is the
			// arbiter of the accuracy gate, and malformed input must never
			// read as a pass. It still counts toward Total and gets a matrix
			// row: a corpus that produced blank labels is for the operator to
			// see, not for this function to quietly absorb.
		case p.Label == NoneLabel:
			if p.Predicted == "" {
				r.Correct++
			}
		case p.Predicted == p.Label:
			r.Correct++
		}
	}

	r.Labels = sortedWithNoneLast(keys(r.Matrix), NoneLabel)
	r.Columns = sortedWithNoneLast(keys(cols), NonePrediction)
	return r
}

// Accuracy is the fraction of frames scored correctly. An empty corpus is 0,
// not NaN: a gate comparison against NaN is always false in confusing ways.
func (r Report) Accuracy() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Correct) / float64(r.Total)
}

// FormatMatrix renders the confusion matrix as a text table.
func (r Report) FormatMatrix() string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)

	fmt.Fprint(tw, "true \\ predicted")
	for _, c := range r.Columns {
		fmt.Fprintf(tw, "\t%s", c)
	}
	fmt.Fprintln(tw)

	for _, l := range r.Labels {
		fmt.Fprint(tw, l)
		for _, c := range r.Columns {
			n := r.Matrix[l][c]
			if n == 0 {
				fmt.Fprint(tw, "\t.")
				continue
			}
			fmt.Fprintf(tw, "\t%d", n)
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	return sb.String()
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sortedWithNoneLast sorts alphabetically but pins the none bucket to the end,
// where it reads as a summary column rather than an alphabetical accident.
func sortedWithNoneLast(in []string, none string) []string {
	sort.Slice(in, func(i, j int) bool {
		if in[i] == none {
			return false
		}
		if in[j] == none {
			return true
		}
		return in[i] < in[j]
	})
	return in
}
