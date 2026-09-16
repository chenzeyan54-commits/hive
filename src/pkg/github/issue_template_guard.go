package github

import (
	"fmt"
	"regexp"
	"strings"
)

// Unfilled-template gate (#7153).
//
// repo_ref_validate.go catches an agent that copies a policy prompt's `--repo
// "<org>/<target-repo>"` verbatim, because a repo name has a tiny legal
// charset and a placeholder is therefore cheap to reject with certainty. The
// same copy-the-template-literally failure happens one field over, in the
// issue's own title and body, where there is no charset to lean on.
//
// Observed live as hivecommons/hive#7153, filed by the guide agent:
//
//	title: [guide] <specific description of the documentation gap>
//	body:  ## Documentation Gap\n\n<what is missing or incorrect>\n\n## Recommendation\n\n<what should be added>
//
// Two independent defects in one filing, which is why this guard checks for
// both:
//
//  1. The `<...>` spans were never substituted. The agent emitted its prompt's
//     skeleton instead of its findings, so the issue describes nothing — it
//     cannot be triaged, reproduced, or fixed, and no PR can be written
//     against it. Unlike a bad repo ref this one SUCCEEDS at the API, so
//     nothing pushes back: it becomes a real issue that a human has to read,
//     understand is empty, and close by hand.
//
//  2. Those `\n` are literal backslash-n, not newlines. The body went through
//     a layer that did not interpret escapes (a single-quoted shell string is
//     the usual culprit), so even the headings render as one unbroken line.
//
// Both are structural defects in the request itself, knowable without asking
// GitHub anything, so this gate FAILS CLOSED — it refuses the create, exactly
// as validateRepoRef does. That is the opposite posture from the dedupe and
// rejected-twin gates in CreateIssue, and deliberately so: those two infer
// something about the world from a lookup that might have failed for unrelated
// reasons, so they file on doubt. This one is a statement about the bytes in
// hand. Failing open here just means the garbage issue gets created and a
// maintainer does the cleanup, which is the bug being fixed.
//
// The error text names the offending span, because the fix belongs in an
// agent's output and the agent gets the error back on its next kick.

// htmlTagNames are the element names that may legitimately open a `<...>` span
// in an issue body. Only those that can plausibly appear bare and multi-word
// need listing; anything carrying attributes is already excluded by the
// charset rule in looksLikeUnfilledPlaceholder.
var htmlTagNames = map[string]bool{
	"a": true, "abbr": true, "b": true, "blockquote": true, "br": true,
	"code": true, "del": true, "details": true, "div": true, "em": true,
	"h1": true, "h2": true, "h3": true, "hr": true, "i": true, "img": true,
	"kbd": true, "li": true, "ol": true, "p": true, "pre": true, "q": true,
	"s": true, "span": true, "strong": true, "sub": true, "summary": true,
	"sup": true, "table": true, "td": true, "th": true, "tr": true, "ul": true,
}

// placeholderSpan matches the inside of a `<...>` that reads as instructional
// prose rather than markup.
//
// The shape is deliberately narrow, because a false positive here refuses a
// legitimate filing and an issue body is arbitrary user text full of HTML,
// autolinks and code. It requires TWO OR MORE space-separated lowercase words
// and permits no '=', quotes, or uppercase — which is what separates
// "what is missing or incorrect" from everything that legitimately looks
// similar:
//
//	<details>, <br>            single token, no space          -> no match
//	<img src="x">, <a href=…>  '=' and '"' outside the charset -> no match
//	<https://example.com>      single token                    -> no match
//	List<T>, Map<K, V>         uppercase                       -> no match
//	a < b and c > d            span starts with a space        -> no match
//
// It does match a genuine hand-written placeholder such as "<your name here>",
// which is the intended behaviour.
var placeholderSpan = regexp.MustCompile(`<([a-z][a-z0-9'./,:;-]*(?: [a-z0-9'./,:;-]+)+)>`)

// looksLikeUnfilledPlaceholder reports the first unsubstituted template
// placeholder in s, if any.
func looksLikeUnfilledPlaceholder(s string) (string, bool) {
	for _, m := range placeholderSpan.FindAllStringSubmatch(s, -1) {
		inner := m[1]
		// An HTML element that happens to be followed by a bare word would
		// otherwise trip the two-word rule; markup is not a placeholder.
		if first, _, _ := strings.Cut(inner, " "); htmlTagNames[first] {
			continue
		}
		return m[0], true
	}
	return "", false
}

// literalEscapeThreshold is the number of literal `\n` sequences that, in a
// body with no real newline anywhere, marks the body as un-interpreted rather
// than merely as text that mentions an escape sequence in passing. Two,
// because a single one is plausibly prose ("separate them with \n") while a
// body built from a multi-section template always has several.
const literalEscapeThreshold = 2

// hasUninterpretedEscapes reports whether body carries literal backslash-n
// sequences and not one actual line break. The conjunction matters: a document
// that genuinely discusses `\n` still has real newlines around that
// discussion, so requiring the total absence of them keeps this off well-formed
// bodies.
func hasUninterpretedEscapes(body string) bool {
	if strings.Contains(body, "\n") {
		return false
	}
	return strings.Count(body, `\n`) >= literalEscapeThreshold
}

// validateIssueTemplateFilled refuses an issue whose title or body is still the
// template the agent was shown. See the package comment above for why this
// fails closed.
func validateIssueTemplateFilled(title, body string) error {
	if span, ok := looksLikeUnfilledPlaceholder(title); ok {
		return fmt.Errorf("refusing to file an issue whose title still contains the placeholder %q: substitute the template with the actual finding before filing", span)
	}
	if span, ok := looksLikeUnfilledPlaceholder(body); ok {
		return fmt.Errorf("refusing to file an issue whose body still contains the placeholder %q: substitute the template with the actual finding before filing", span)
	}
	if hasUninterpretedEscapes(body) {
		return fmt.Errorf(`refusing to file an issue whose body contains literal \n escapes and no real line breaks: the body was passed through a layer that did not interpret escapes (a single-quoted shell string is the usual cause) — write the body to a file and pass --body-file instead`)
	}
	return nil
}
