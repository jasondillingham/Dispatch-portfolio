package ui

import "strings"

// CollapseQuotedHTML wraps the quoted-reply portion of an Outlook HTML body in
// a <details>/<summary> element so the current reply is what the clerk sees
// first, with the chain history available on click.
//
// Outlook marks the boundary with <div id="divRplyFwdMsg"> (and x_divRplyFwdMsg
// for nested levels). Typically preceded by an <hr> separator. We cut at the
// hr if one is present immediately before; otherwise at the div itself.
//
// If no marker is found, returns the body unchanged and hasHistory=false.
// The output is still valid HTML and safe inside our sandboxed iframe.
func CollapseQuotedHTML(body string) (out string, hasHistory bool) {
	if body == "" {
		return body, false
	}
	// Find the earliest reply-fwd marker. Prefer outermost (divRplyFwdMsg over x_...).
	markers := []string{
		`id="divRplyFwdMsg"`,
		`id='divRplyFwdMsg'`,
	}
	splitAt := -1
	for _, m := range markers {
		if i := strings.Index(body, m); i >= 0 {
			if splitAt < 0 || i < splitAt {
				splitAt = i
			}
		}
	}
	if splitAt < 0 {
		// also look for plain text markers (forwarded plain-text emails)
		for _, m := range []string{"-----Original Message-----", "________________________________"} {
			if i := strings.Index(body, m); i >= 0 {
				if splitAt < 0 || i < splitAt {
					splitAt = i
				}
			}
		}
	}
	if splitAt < 0 {
		return body, false
	}

	// Walk back to a clean tag boundary. Prefer <hr ...> (the visible divider).
	// Then fall back to the opening <div of the divRplyFwdMsg block.
	cutoff := splitAt
	if hr := strings.LastIndex(body[:splitAt], "<hr"); hr >= 0 && splitAt-hr < 500 {
		cutoff = hr
	} else if div := strings.LastIndex(body[:splitAt], "<div"); div >= 0 && splitAt-div < 500 {
		cutoff = div
	}

	before := body[:cutoff]
	after := body[cutoff:]

	// Insert </body></html> closer + details wrapper. Close any open <body>/<html>
	// at the end of `after` so the wrapper is well-formed. Browsers render this
	// in a sandboxed iframe regardless of minor malformation, but being tidy
	// avoids visual glitches.

	// If the body ends with </body></html>, strip those from `after` and
	// re-add them at the very end, after </details>.
	afterTrimmed := after
	tail := ""
	for _, closer := range []string{"</body>", "</html>"} {
		if i := strings.LastIndex(afterTrimmed, closer); i >= 0 {
			tail = afterTrimmed[i:] + tail
			afterTrimmed = afterTrimmed[:i]
		}
	}

	const style = `<style>
details.dispatch-quoted { margin-top: 1rem; border-top: 1px solid #e5e7eb; padding-top: .5rem; }
details.dispatch-quoted > summary { cursor: pointer; color: #6b7280; font-size: .85rem; user-select: none; padding: .35rem 0; list-style: none; }
details.dispatch-quoted > summary::-webkit-details-marker { display: none; }
details.dispatch-quoted > summary::before { content: "▸ "; font-size: .8em; }
details.dispatch-quoted[open] > summary::before { content: "▾ "; }
details.dispatch-quoted > summary:hover { color: #111827; }
</style>`

	out = before +
		style +
		`<details class="dispatch-quoted"><summary>Show quoted history</summary>` +
		afterTrimmed +
		`</details>` +
		tail
	return out, true
}
