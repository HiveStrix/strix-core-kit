// Package textnorm normalizes names for search.
//
// Why this exists instead of `unaccent` in Postgres: unaccent is an untrusted
// extension, so CREATE EXTENSION needs superuser, and the tenant role the
// Shell's provisioner creates is not one — requiring it would break
// provisioning. Normalizing in the application keeps the index an ordinary
// index over an ordinary column.
//
// It also sidesteps a known trap: unaccent is STABLE, and Postgres rejects a
// non-IMMUTABLE function in an index expression without a wrapper declared
// immutable.
package textnorm

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Search normalizes a name for storage in a search_name column and for building
// the query that matches against it. BOTH SIDES MUST USE THIS FUNCTION, or a
// search for "azucar" will not find "Azúcar".
//
// It decomposes to NFD, drops the combining marks, lowercases and collapses
// runs of whitespace.
func Search(s string) string {
	t := transform.Chain(
		norm.NFD,
		runes.Remove(runes.In(unicode.Mn)), // Mn: combining marks (the accents)
		norm.NFC,
	)
	folded, _, err := transform.String(t, s)
	if err != nil {
		// The transform above cannot fail on valid UTF-8; on invalid input,
		// falling back to the raw string keeps search degraded rather than
		// dropping the row's searchability entirely.
		folded = s
	}
	return strings.Join(strings.Fields(strings.ToLower(folded)), " ")
}
