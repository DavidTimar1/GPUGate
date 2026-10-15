package provider

import "strings"

// euLocationPrefixes are the data-centre or location-code prefixes that count as "EU" in this
// program: the European Union's member states, plus Iceland and Norway (the European Economic
// Area). RunPod's `GET /catalog/datacenters` carries a continental `region` (NORTH_AMERICA,
// EUROPE, ASIA, OCEANIA) but never EU membership, so `region` alone cannot answer this.
// [DOC] docs.runpod.io/pods/networking, read 2026-10-12: `EU-*` are union members, `EUR-IS-*` is
// Iceland and `EUR-NO-*` is Norway. [DOC] docs.verda.com's locations page: Verda's locations are
// all in Helsinki, Finland, coded `FIN-*`. Canada (`CA-*`), Japan, India and Australia are never
// matched.
var euLocationPrefixes = []string{"EU-", "EUR-IS-", "EUR-NO-", "FIN-"}

// IsEULocation reports whether code names a place the program counts as EU: a member state of
// the European Union, Iceland or Norway. This is the one place that set is decided.
func IsEULocation(code string) bool {
	for _, prefix := range euLocationPrefixes {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	return false
}

// IsUSLocation reports whether code names a place in the United States. RunPod's data-centre ids
// for the United States are of the form "US-<state>-<n>" (for example "US-MO-2"); Verda has
// none, so this is always false on a Verda row.
func IsUSLocation(code string) bool {
	return strings.HasPrefix(code, "US-")
}
