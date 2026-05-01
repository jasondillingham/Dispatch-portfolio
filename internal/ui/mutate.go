package ui

import "strings"

// ReplaceOwner returns a new category list with the Owner: entry set to `owner`.
// If owner is empty, the Owner: entry is removed.
func ReplaceOwner(cats []string, owner string) []string {
	out := make([]string, 0, len(cats)+1)
	for _, c := range cats {
		if !strings.HasPrefix(c, ownerPrefix) {
			out = append(out, c)
		}
	}
	if owner = strings.TrimSpace(owner); owner != "" {
		out = append(out, ownerPrefix+owner)
	}
	return out
}

// StripAllBlockers returns a new category list with every Blocker: entry
// removed. If Status was Blocked, downgrades to New so the row re-enters the
// active queue. Used by the auto-recheck-blocked sweep when a recon that
// previously discrepant now reconciles cleanly (buyer updated the PO,
// vendor sent corrected pricing, etc).
func StripAllBlockers(cats []string) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		if strings.HasPrefix(c, blockerPrefix) {
			continue
		}
		out = append(out, c)
	}
	// If Status was Blocked, downgrade. Anything else stays as-is.
	hasBlocked := false
	idx := -1
	for i, c := range out {
		if c == statusPrefix+"Blocked" {
			hasBlocked = true
			idx = i
			break
		}
	}
	if hasBlocked {
		out[idx] = statusPrefix + "New"
	}
	return out
}

// StripKind returns a new category list with any Kind: entry removed. Used
// when a clerk corrects a misclassification — e.g. an AI tagged
// "Kind: Marketing" on what's actually a real invoice from Sample-Electric.
// Stripping the Kind returns the row to the main queue (HiddenByDefault no
// longer fires). Status and other categories are preserved untouched.
func StripKind(cats []string) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		if strings.HasPrefix(c, kindPrefix) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ReplaceStatus returns a new category list with the Status: entry set to `status`.
// If status is empty, the Status: entry is removed.
func ReplaceStatus(cats []string, status string) []string {
	out := make([]string, 0, len(cats)+1)
	for _, c := range cats {
		if !strings.HasPrefix(c, statusPrefix) {
			out = append(out, c)
		}
	}
	if status = strings.TrimSpace(status); status != "" {
		out = append(out, statusPrefix+status)
	}
	return out
}

// ToggleBlocker returns a new category list with the given Blocker: entry
// added if missing, removed if present. Also ensures Status: Blocked is set
// whenever any blocker remains; clears Status: Blocked if no blockers remain
// and current Status was Blocked (falls back to In Progress — clerk can finalize).
func ToggleBlocker(cats []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return cats
	}
	target := blockerPrefix + name
	found := false
	out := make([]string, 0, len(cats)+1)
	for _, c := range cats {
		if strings.EqualFold(c, target) {
			found = true
			continue // remove this blocker
		}
		out = append(out, c)
	}
	if !found {
		out = append(out, target)
	}
	// Auto-manage Status
	hasOtherBlocker := false
	for _, c := range out {
		if strings.HasPrefix(c, blockerPrefix) {
			hasOtherBlocker = true
			break
		}
	}
	if hasOtherBlocker {
		out = ReplaceStatus(out, "Blocked")
	} else {
		// If we just removed the last blocker and Status was Blocked, downgrade to In Progress
		currentStatus := ""
		for _, c := range out {
			if strings.HasPrefix(c, statusPrefix) {
				currentStatus = strings.TrimPrefix(c, statusPrefix)
				break
			}
		}
		if currentStatus == "Blocked" {
			out = ReplaceStatus(out, "In Progress")
		}
	}
	return out
}

// ValidStatus returns true if s is one of the allowed Status values. Accepts
// the user-selectable set (StatusOptions) plus the system-only "Done" — Done
// is set by voucher sync, not by clerks, but still has to pass the validator
// when the system writes it.
func ValidStatus(s string) bool {
	for _, o := range allValidStatuses {
		if o == s {
			return true
		}
	}
	return false
}

// ValidBlocker returns true if b is one of the allowed Blocker values.
func ValidBlocker(b string) bool {
	for _, o := range BlockerOptions {
		if o == b {
			return true
		}
	}
	return false
}
